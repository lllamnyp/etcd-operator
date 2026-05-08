/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clientv3 "go.etcd.io/etcd/client/v3"

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

// EtcdMemberReconciler reconciles an EtcdMember object.
type EtcdMemberReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdmembers,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdmembers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdmembers/finalizers,verbs=update
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdclusters,verbs=get
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete

func (r *EtcdMemberReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	member := &lll.EtcdMember{}
	if err := r.Get(ctx, req.NamespacedName, member); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ── Deletion ───────────────────────────────────────────────────────
	if !member.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, member)
	}

	// ── Ensure finalizer ───────────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(member, MemberFinalizer) {
		controllerutil.AddFinalizer(member, MemberFinalizer)
		if err := r.Update(ctx, member); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ── Ensure PVC ─────────────────────────────────────────────────────
	if err := r.ensurePVC(ctx, member); err != nil {
		log.Error(err, "failed to ensure PVC")
		return ctrl.Result{}, err
	}

	// ── Ensure Pod ─────────────────────────────────────────────────────
	if err := r.ensurePod(ctx, member); err != nil {
		log.Error(err, "failed to ensure Pod")
		return ctrl.Result{}, err
	}

	// ── Update status ──────────────────────────────────────────────────
	return r.updateStatus(ctx, member)
}

// ── Deletion ─────────────────────────────────────────────────────────────

func (r *EtcdMemberReconciler) handleDeletion(ctx context.Context, member *lll.EtcdMember) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(member, MemberFinalizer) {
		return ctrl.Result{}, nil
	}

	log := log.FromContext(ctx)

	// If the owning cluster is gone, skip etcd-level removal.
	clusterDeleting := false
	cluster := &lll.EtcdCluster{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: member.Namespace,
		Name:      member.Spec.ClusterName,
	}, cluster)
	if errors.IsNotFound(err) || (err == nil && !cluster.DeletionTimestamp.IsZero()) {
		clusterDeleting = true
	}

	if !clusterDeleting && member.Status.MemberID != "" {
		if err := r.removeMemberFromEtcd(ctx, member); err != nil {
			log.Error(err, "failed to remove member from etcd, will retry")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	controllerutil.RemoveFinalizer(member, MemberFinalizer)
	if err := r.Update(ctx, member); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *EtcdMemberReconciler) removeMemberFromEtcd(ctx context.Context, member *lll.EtcdMember) error {
	// Build endpoints from other members of the same cluster.
	memberList := &lll.EtcdMemberList{}
	if err := r.List(ctx, memberList,
		client.InNamespace(member.Namespace),
		client.MatchingLabels{LabelCluster: member.Spec.ClusterName},
	); err != nil {
		return err
	}

	var endpoints []string
	for _, m := range memberList.Items {
		if m.Name != member.Name {
			endpoints = append(endpoints, clientURL(m.Name, member.Spec.ClusterName, member.Namespace))
		}
	}
	if len(endpoints) == 0 {
		return nil // last member, nothing to remove from
	}

	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	defer etcdClient.Close()

	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	id, err := strconv.ParseUint(member.Status.MemberID, 16, 64)
	if err != nil {
		return fmt.Errorf("parse memberID %q: %w", member.Status.MemberID, err)
	}
	_, err = etcdClient.MemberRemove(rmCtx, id)
	return err
}

// ── PVC ──────────────────────────────────────────────────────────────────

func (r *EtcdMemberReconciler) ensurePVC(ctx context.Context, member *lll.EtcdMember) error {
	pvcName := "data-" + member.Name
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: pvcName}, pvc)
	if err == nil {
		member.Status.PVCName = pvcName
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: member.Namespace,
			Labels:    memberLabels(member.Spec.ClusterName, member.Name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: member.Spec.Storage,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(member, pvc, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pvc); err != nil {
		return err
	}
	member.Status.PVCName = pvcName
	return nil
}

// ── Pod ──────────────────────────────────────────────────────────────────

func (r *EtcdMemberReconciler) ensurePod(ctx context.Context, member *lll.EtcdMember) error {
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: member.Name}, pod)
	if err == nil {
		member.Status.PodName = pod.Name
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	pod = r.buildPod(member)
	if err := controllerutil.SetControllerReference(member, pod, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); err != nil {
		return err
	}
	member.Status.PodName = pod.Name
	return nil
}

func (r *EtcdMemberReconciler) buildPod(member *lll.EtcdMember) *corev1.Pod {
	clusterState := "new"
	if !member.Spec.Bootstrap {
		clusterState = "existing"
	}

	pAddr := peerURL(member.Name, member.Spec.ClusterName, member.Namespace)
	cAddr := clientURL(member.Name, member.Spec.ClusterName, member.Namespace)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      member.Name,
			Namespace: member.Namespace,
			Labels:    memberLabels(member.Spec.ClusterName, member.Name),
		},
		Spec: corev1.PodSpec{
			Hostname:  member.Name,
			Subdomain: member.Spec.ClusterName, // headless service name
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptrBool(true),
				RunAsUser:    ptrInt64(60000),
				RunAsGroup:   ptrInt64(60000),
				FSGroup:      ptrInt64(60000),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:  "etcd",
				Image: fmt.Sprintf("%s:v%s", EtcdImage, member.Spec.Version),
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptrBool(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
				Command: []string{
					"etcd",
					"--name=" + member.Name,
					"--data-dir=/var/lib/etcd",
					"--listen-peer-urls=http://0.0.0.0:2380",
					"--listen-client-urls=http://0.0.0.0:2379",
					"--advertise-client-urls=" + cAddr,
					"--initial-advertise-peer-urls=" + pAddr,
					"--initial-cluster=" + member.Spec.InitialCluster,
					"--initial-cluster-token=" + member.Spec.ClusterToken,
					"--initial-cluster-state=" + clusterState,
				},
				Ports: []corev1.ContainerPort{
					{Name: "client", ContainerPort: 2379},
					{Name: "peer", ContainerPort: 2380},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/var/lib/etcd"},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/health",
							Port: intstr.FromInt(2379),
						},
					},
					InitialDelaySeconds: 15,
					PeriodSeconds:       10,
					FailureThreshold:    3,
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/health",
							Port: intstr.FromInt(2379),
						},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
					FailureThreshold:    3,
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "data-" + member.Name,
					},
				},
			}},
		},
	}
}

// ── Status ───────────────────────────────────────────────────────────────

func (r *EtcdMemberReconciler) updateStatus(ctx context.Context, member *lll.EtcdMember) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: member.Name}, pod); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	member.Status.PodName = pod.Name
	member.Status.PVCName = "data-" + member.Name

	podReady := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			podReady = true
			break
		}
	}

	if podReady {
		setCondition(&member.Status.Conditions, metav1.Condition{
			Type:    lll.MemberReady,
			Status:  metav1.ConditionTrue,
			Reason:  "PodReady",
			Message: "etcd member is ready",
		})
		// Once the pod is ready, learn this member's etcd ID and record
		// it on status. The finalizer needs it to call MemberRemove.
		if member.Status.MemberID == "" {
			if id, err := r.discoverMemberID(ctx, member); err == nil {
				member.Status.MemberID = fmt.Sprintf("%x", id)
			}
			// On error we silently retry on the next reconcile.
		}
	} else {
		setCondition(&member.Status.Conditions, metav1.Condition{
			Type:    lll.MemberReady,
			Status:  metav1.ConditionFalse,
			Reason:  "PodNotReady",
			Message: fmt.Sprintf("pod phase: %s", pod.Status.Phase),
		})
	}

	if err := r.Status().Update(ctx, member); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// discoverMemberID asks etcd for this member's ID by calling MemberList against
// its own client endpoint and matching by name.
func (r *EtcdMemberReconciler) discoverMemberID(ctx context.Context, member *lll.EtcdMember) (uint64, error) {
	endpoint := clientURL(member.Name, member.Spec.ClusterName, member.Namespace)
	c, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return 0, err
	}
	defer c.Close()

	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := c.MemberList(listCtx)
	if err != nil {
		return 0, err
	}
	for _, m := range resp.Members {
		if m.Name == member.Name {
			return m.ID, nil
		}
	}
	return 0, fmt.Errorf("member %q not found in etcd member list", member.Name)
}

// ── Manager wiring ───────────────────────────────────────────────────────

func (r *EtcdMemberReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lll.EtcdMember{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
