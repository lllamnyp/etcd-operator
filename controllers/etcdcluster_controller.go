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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clientv3 "go.etcd.io/etcd/client/v3"

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

// EtcdClusterReconciler reconciles an EtcdCluster object.
type EtcdClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdclusters,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdmembers,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch

func (r *EtcdClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// ── Fetch cluster ──────────────────────────────────────────────────
	cluster := &lll.EtcdCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// ── Ensure Services ────────────────────────────────────────────────
	if err := r.ensureServices(ctx, cluster); err != nil {
		log.Error(err, "failed to ensure services")
		return ctrl.Result{}, err
	}

	// ── List active members ────────────────────────────────────────────
	memberList := &lll.EtcdMemberList{}
	if err := r.List(ctx, memberList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{LabelCluster: cluster.Name},
	); err != nil {
		return ctrl.Result{}, err
	}
	active := filterActiveMembers(memberList.Items)

	desired := int32(3)
	if cluster.Spec.Replicas != nil {
		desired = *cluster.Spec.Replicas
	}
	current := int32(len(active))

	// ── Bootstrap ──────────────────────────────────────────────────────
	if cluster.Status.ClusterID == nil {
		if current < desired {
			log.Info("bootstrapping cluster", "current", current, "desired", desired)
			return r.bootstrap(ctx, cluster, active, desired)
		}
		// All bootstrap members exist; wait for the cluster to form.
		log.Info("waiting for cluster to form", "members", current)
		return r.tryDiscoverCluster(ctx, cluster, active)
	}

	// ── Scale ──────────────────────────────────────────────────────────
	if current < desired {
		log.Info("scaling up", "current", current, "desired", desired)
		return r.scaleUp(ctx, cluster, active)
	}
	if current > desired {
		log.Info("scaling down", "current", current, "desired", desired)
		return r.scaleDown(ctx, cluster, active)
	}

	// ── Steady state ───────────────────────────────────────────────────
	return r.updateStatus(ctx, cluster, active)
}

// ── Bootstrap ────────────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) bootstrap(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	existing []lll.EtcdMember,
	desired int32,
) (ctrl.Result, error) {
	existingNames := make(map[string]bool, len(existing))
	for _, m := range existing {
		existingNames[m.Name] = true
	}

	// Build the complete list of member names for the initial cluster.
	allNames := make([]string, 0, desired)
	for _, m := range existing {
		allNames = append(allNames, m.Name)
	}
	for i := int32(0); int32(len(allNames)) < desired; i++ {
		name := fmt.Sprintf("%s-%d", cluster.Name, i)
		if !existingNames[name] {
			allNames = append(allNames, name)
		}
	}
	sort.Strings(allNames)

	initialCluster := buildInitialCluster(allNames, cluster.Name, cluster.Namespace)

	for _, name := range allNames {
		if existingNames[name] {
			continue
		}
		member := &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
				Labels:    memberLabels(cluster.Name, name),
			},
			Spec: lll.EtcdMemberSpec{
				ClusterName:    cluster.Name,
				Version:        cluster.Spec.Version,
				Storage:        cluster.Spec.Storage,
				Bootstrap:      true,
				InitialCluster: initialCluster,
			},
		}
		if err := controllerutil.SetControllerReference(cluster, member, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, member); err != nil && !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// ── Cluster discovery ────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) tryDiscoverCluster(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	endpoints := memberEndpoints(members, cluster.Name, cluster.Namespace)
	if len(endpoints) == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		// Cluster not reachable yet; retry.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer etcdClient.Close()

	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := etcdClient.MemberList(listCtx)
	if err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	clusterID := resp.Header.ClusterId
	cluster.Status.ClusterID = &clusterID
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil // requeue via watch
}

// ── Scale up ─────────────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) scaleUp(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	endpoints := memberEndpoints(members, cluster.Name, cluster.Namespace)
	etcdClient, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Error(err, "cannot connect to etcd for scale-up")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer etcdClient.Close()

	newName := nextMemberName(cluster.Name, members)
	newPeerURL := peerURL(newName, cluster.Name, cluster.Namespace)

	addCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err = etcdClient.MemberAdd(addCtx, []string{newPeerURL}); err != nil {
		log.Error(err, "MemberAdd failed")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Build initial-cluster including the new member.
	allNames := make([]string, 0, len(members)+1)
	for _, m := range members {
		allNames = append(allNames, m.Name)
	}
	allNames = append(allNames, newName)
	initialCluster := buildInitialCluster(allNames, cluster.Name, cluster.Namespace)

	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newName,
			Namespace: cluster.Namespace,
			Labels:    memberLabels(cluster.Name, newName),
		},
		Spec: lll.EtcdMemberSpec{
			ClusterName:    cluster.Name,
			Version:        cluster.Spec.Version,
			Storage:        cluster.Spec.Storage,
			Bootstrap:      false,
			InitialCluster: initialCluster,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, member, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, member); err != nil {
		return ctrl.Result{}, err
	}

	// Wait for the new member to become ready before adding more.
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// ── Scale down ───────────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) scaleDown(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	// Remove the member with the highest ordinal.
	sort.Slice(members, func(i, j int) bool {
		return memberOrdinal(members[i].Name) > memberOrdinal(members[j].Name)
	})
	victim := members[0]

	if err := r.Delete(ctx, &victim); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// ── Status ───────────────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) updateStatus(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	desired := int32(3)
	if cluster.Spec.Replicas != nil {
		desired = *cluster.Spec.Replicas
	}

	ready := int32(0)
	for _, m := range members {
		for _, c := range m.Status.Conditions {
			if c.Type == lll.MemberReady && c.Status == metav1.ConditionTrue {
				ready++
				break
			}
		}
	}

	cluster.Status.ReadyMembers = ready

	switch {
	case ready == desired:
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    lll.ClusterAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "QuorumHealthy",
			Message: "All members are ready",
		})
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   lll.ClusterDegraded,
			Status: metav1.ConditionFalse,
			Reason: "QuorumHealthy",
		})
	case ready > desired/2:
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    lll.ClusterAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "QuorumAvailable",
			Message: fmt.Sprintf("%d/%d members ready", ready, desired),
		})
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    lll.ClusterDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  "MembersUnhealthy",
			Message: fmt.Sprintf("%d/%d members ready", ready, desired),
		})
	default:
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:    lll.ClusterAvailable,
			Status:  metav1.ConditionFalse,
			Reason:  "QuorumLost",
			Message: fmt.Sprintf("%d/%d members ready, quorum lost", ready, desired),
		})
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   lll.ClusterDegraded,
			Status: metav1.ConditionTrue,
			Reason: "QuorumLost",
		})
	}

	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// ── Services ─────────────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) ensureServices(ctx context.Context, cluster *lll.EtcdCluster) error {
	// Headless service — provides per-pod DNS for peer discovery.
	if err := r.ensureService(ctx, cluster, cluster.Name, corev1.ServiceSpec{
		ClusterIP:                corev1.ClusterIPNone,
		PublishNotReadyAddresses: true,
		Selector:                 map[string]string{LabelCluster: cluster.Name},
		Ports: []corev1.ServicePort{
			{Name: "client", Port: 2379},
			{Name: "peer", Port: 2380},
		},
	}); err != nil {
		return err
	}

	// Client service — stable endpoint for applications.
	return r.ensureService(ctx, cluster, cluster.Name+"-client", corev1.ServiceSpec{
		Selector: map[string]string{LabelCluster: cluster.Name},
		Ports: []corev1.ServicePort{
			{Name: "client", Port: 2379},
		},
	})
}

func (r *EtcdClusterReconciler) ensureService(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	name string,
	spec corev1.ServiceSpec,
) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, svc)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	svc = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels:    clusterLabels(cluster.Name),
		},
		Spec: spec,
	}
	if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, svc)
}

// ── Manager wiring ───────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lll.EtcdCluster{}).
		Owns(&lll.EtcdMember{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
