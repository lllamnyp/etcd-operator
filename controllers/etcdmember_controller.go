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

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

// EtcdMemberReconciler reconciles an EtcdMember object.
type EtcdMemberReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	EtcdClientFactory EtcdClientFactory
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

	if !member.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, member)
	}

	if !controllerutil.ContainsFinalizer(member, MemberFinalizer) {
		controllerutil.AddFinalizer(member, MemberFinalizer)
		if err := r.Update(ctx, member); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensurePVC(ctx, member); err != nil {
		log.Error(err, "failed to ensure PVC")
		return ctrl.Result{}, err
	}

	if err := r.ensurePod(ctx, member); err != nil {
		log.Error(err, "failed to ensure Pod")
		return ctrl.Result{}, err
	}

	return r.updateStatus(ctx, member)
}

// ── Deletion ─────────────────────────────────────────────────────────────

func (r *EtcdMemberReconciler) handleDeletion(ctx context.Context, member *lll.EtcdMember) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(member, MemberFinalizer) {
		return ctrl.Result{}, nil
	}

	log := log.FromContext(ctx)

	clusterDeleting := false
	cluster := &lll.EtcdCluster{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: member.Namespace,
		Name:      member.Spec.ClusterName,
	}, cluster)
	switch {
	case errors.IsNotFound(err):
		clusterDeleting = true
	case err != nil:
		// Don't silently treat a transient apiserver error as "cluster
		// alive". Propagate so controller-runtime retries with backoff.
		return ctrl.Result{}, fmt.Errorf("get owner EtcdCluster: %w", err)
	case !cluster.DeletionTimestamp.IsZero():
		clusterDeleting = true
	}

	if !clusterDeleting {
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

// removeMemberFromEtcd handles three cases:
//
//  1. status.MemberID is set — call MemberRemove(id) directly.
//  2. status.MemberID is empty (e.g. the pod never finished bootstrapping
//     before the user scaled back down) — list the etcd cluster, find a
//     member matching this name and call MemberRemove on its ID. Etcd may
//     have an entry from MemberAdd even if the pod never started, so we must
//     still clean it up.
//  3. The member is not in etcd's list at all — treat as already gone and
//     return nil so the finalizer can be removed.
func (r *EtcdMemberReconciler) removeMemberFromEtcd(ctx context.Context, member *lll.EtcdMember) error {
	memberList := &lll.EtcdMemberList{}
	if err := r.List(ctx, memberList,
		client.InNamespace(member.Namespace),
		client.MatchingLabels{LabelCluster: member.Spec.ClusterName},
	); err != nil {
		return err
	}

	// Only dial peers that aren't themselves being deleted. Endpoints of
	// a Terminating pod can hang the client until our 10s deadline and
	// don't help us anyway.
	var endpoints []string
	otherMembers := 0
	for _, m := range filterActiveMembers(memberList.Items) {
		if m.Name == member.Name {
			continue
		}
		otherMembers++
		if m.Status.PodName != "" {
			endpoints = append(endpoints, clientURL(m.Name, member.Spec.ClusterName, member.Namespace))
		}
	}
	if len(endpoints) == 0 {
		if otherMembers > 0 {
			// Other members exist on the CR side but none have a PodName —
			// transient state (controller restart hasn't repopulated status,
			// or pods haven't been created yet). Don't silently skip
			// MemberRemove; return an error so the finalizer requeues.
			return fmt.Errorf("no reachable peers (%d other member(s) have empty PodName); will retry", otherMembers)
		}
		// Genuinely the last member — nothing to remove from.
		return nil
	}

	etcdClient, err := r.EtcdClientFactory(ctx, endpoints)
	if err != nil {
		return err
	}
	defer etcdClient.Close()

	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	id, err := r.resolveMemberID(rmCtx, member, etcdClient)
	if err != nil {
		return err
	}
	if id == 0 {
		// Not found in etcd. Already gone.
		return nil
	}

	_, err = etcdClient.MemberRemove(rmCtx, id)
	return err
}

// resolveMemberID returns the etcd member ID this EtcdMember refers to. If
// status.MemberID was populated we trust it; otherwise we ask etcd to look up
// the member by name. Returns 0 if not found in etcd.
func (r *EtcdMemberReconciler) resolveMemberID(
	ctx context.Context,
	member *lll.EtcdMember,
	c EtcdClusterClient,
) (uint64, error) {
	if member.Status.MemberID != "" {
		id, err := strconv.ParseUint(member.Status.MemberID, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("parse memberID %q: %w", member.Status.MemberID, err)
		}
		return id, nil
	}

	resp, err := c.MemberList(ctx)
	if err != nil {
		return 0, err
	}
	for _, m := range resp.Members {
		if m.Name == member.Name {
			return m.ID, nil
		}
		// Match by peer URL too — etcd may have a member added via MemberAdd
		// whose Name is empty until it joins.
		expected := peerURL(member.Name, member.Spec.ClusterName, member.Namespace)
		for _, p := range m.PeerURLs {
			if p == expected {
				return m.ID, nil
			}
		}
	}
	return 0, nil
}

// ── PVC ──────────────────────────────────────────────────────────────────

func (r *EtcdMemberReconciler) ensurePVC(ctx context.Context, member *lll.EtcdMember) error {
	pvcName := "data-" + member.Name
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: member.Namespace, Name: pvcName}, pvc)
	if err == nil {
		// Refuse to reuse a PVC owned by a different (typically deleted)
		// EtcdMember. Same-named members across scale-down/up cycles would
		// otherwise inherit the prior member's data dir and crashloop after
		// trying to rejoin with a removed memberID.
		if !pvcOwnedBy(pvc, member) {
			return fmt.Errorf("PVC %q is owned by a different EtcdMember; awaiting GC before reuse", pvcName)
		}
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

// pvcOwnedBy returns true only if the PVC carries an EtcdMember owner
// reference whose UID matches this member — i.e. we created it. PVCs with
// no owner refs, or with owner refs pointing at anything else, are refused.
// Adoption of pre-existing PVCs (e.g. for a future scale-to-zero feature)
// will need an explicit re-parenting step rather than implicit acceptance;
// see https://github.com/lllamnyp/etcd-operator/issues/3.
func pvcOwnedBy(pvc *corev1.PersistentVolumeClaim, member *lll.EtcdMember) bool {
	for _, o := range pvc.OwnerReferences {
		if o.Kind == "EtcdMember" && o.UID == member.UID {
			return true
		}
	}
	return false
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
			Subdomain: member.Spec.ClusterName,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptrBool(true),
				RunAsUser:    ptrInt64(65532),
				RunAsGroup:   ptrInt64(65532),
				FSGroup:      ptrInt64(65532),
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
				// Liveness MUST NOT require quorum. /health returns non-200
				// on a member that can't reach peers — if we put HTTPGet on
				// /health here, a transient partition cascade-kills every
				// pod and turns a 30s blip into a permanent outage that
				// needs a snapshot restore. TCP on the peer port (2380) is
				// the canonical "etcd process alive" check; quorum-aware
				// signalling stays in the readiness probe.
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{
							Port: intstr.FromInt(2380),
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

	switch {
	case !podReady:
		setMemberCondition(member, lll.MemberReady, metav1.ConditionFalse, "PodNotReady",
			fmt.Sprintf("pod phase: %s", pod.Status.Phase))
	case member.Status.MemberID == "":
		// Pod ready but we haven't matched it to an etcd member yet.
		// Try once, but don't claim Ready=True until we have the ID — the
		// finalizer needs it to perform a clean MemberRemove on scale-down.
		if id, err := r.discoverMemberID(ctx, member); err == nil {
			member.Status.MemberID = fmt.Sprintf("%016x", id)
			setMemberCondition(member, lll.MemberReady, metav1.ConditionTrue, "PodReady",
				"etcd member is ready")
		} else {
			setMemberCondition(member, lll.MemberReady, metav1.ConditionFalse, "DiscoveringMemberID",
				fmt.Sprintf("waiting for memberID: %v", err))
		}
	default:
		setMemberCondition(member, lll.MemberReady, metav1.ConditionTrue, "PodReady",
			"etcd member is ready")
	}

	if err := r.Status().Update(ctx, member); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// discoverMemberID asks etcd for this member's ID. It prefers asking peers
// rather than the member's own pod: if this member is crashlooping (PVC
// corruption, OOM, etc.) its own etcd never responds, but the rest of the
// cluster knows perfectly well what its ID is. Falling back to self last
// keeps single-node bootstrap working.
func (r *EtcdMemberReconciler) discoverMemberID(ctx context.Context, member *lll.EtcdMember) (uint64, error) {
	memberList := &lll.EtcdMemberList{}
	if err := r.List(ctx, memberList,
		client.InNamespace(member.Namespace),
		client.MatchingLabels{LabelCluster: member.Spec.ClusterName},
	); err != nil {
		return 0, err
	}

	var endpoints []string
	for _, m := range filterActiveMembers(memberList.Items) {
		if m.Name == member.Name {
			continue
		}
		if m.Status.PodName != "" {
			endpoints = append(endpoints, clientURL(m.Name, member.Spec.ClusterName, member.Namespace))
		}
	}
	// Self last — used during single-node bootstrap when there are no
	// other peers.
	endpoints = append(endpoints, clientURL(member.Name, member.Spec.ClusterName, member.Namespace))

	c, err := r.EtcdClientFactory(ctx, endpoints)
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
	if r.EtcdClientFactory == nil {
		r.EtcdClientFactory = DefaultEtcdClientFactory
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&lll.EtcdMember{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
