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

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

// DefaultProgressDeadlineSeconds is used when the user doesn't set one.
const DefaultProgressDeadlineSeconds = int32(600)

// EtcdClusterReconciler reconciles an EtcdCluster object.
type EtcdClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EtcdClientFactory builds an etcd client. Tests inject a fake;
	// production wiring uses DefaultEtcdClientFactory.
	EtcdClientFactory EtcdClientFactory
}

//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=lllamnyp.su,resources=etcdmembers,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch

func (r *EtcdClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	cluster := &lll.EtcdCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.ensureServices(ctx, cluster); err != nil {
		log.Error(err, "failed to ensure services")
		return ctrl.Result{}, err
	}

	memberList := &lll.EtcdMemberList{}
	if err := r.List(ctx, memberList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{LabelCluster: cluster.Name},
	); err != nil {
		return ctrl.Result{}, err
	}
	active := filterActiveMembers(memberList.Items)

	// ── Lock in the cluster token before any member references it ─────
	if cluster.Status.ClusterToken == "" {
		cluster.Status.ClusterToken = deriveClusterToken(cluster)
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// ── Snapshot spec → Observed and (re)set the deadline as needed ───
	// Three triggers cause us to copy spec into Observed:
	//   1. First reconcile (Observed is nil).
	//   2. Reconciliation against current Observed has just completed and
	//      spec has since changed.
	//   3. The deadline has elapsed without convergence — we abandon the
	//      in-flight target and pick up the latest spec.
	current := int32(len(active))
	now := metav1.Now()

	if cluster.Status.Observed == nil {
		log.Info("snapshotting initial spec into observed")
		snapshotSpecIntoObserved(cluster)
		setProgressDeadline(cluster, now)
		setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionTrue, "InitialSnapshot",
			"snapshotted initial spec into status.observed")
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	complete := reconciliationComplete(cluster, active)

	if !complete && deadlineExpired(cluster, now) {
		return r.handleDeadlineExceeded(ctx, cluster, now)
	}

	if complete && !specEqualsObserved(cluster) {
		log.Info("previous target reached; adopting new spec",
			"observed", cluster.Status.Observed, "spec", cluster.Spec)
		snapshotSpecIntoObserved(cluster)
		setProgressDeadline(cluster, now)
		setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionTrue, "SpecChanged",
			"adopting new spec after previous target reached")
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	desired := cluster.Status.Observed.Replicas

	// ── Bootstrap ──────────────────────────────────────────────────────
	if cluster.Status.ClusterID == "" {
		if current < desired {
			log.Info("bootstrapping cluster", "current", current, "desired", desired)
			return r.bootstrap(ctx, cluster, active, desired)
		}
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
				Version:        cluster.Status.Observed.Version,
				Storage:        cluster.Status.Observed.Storage,
				Bootstrap:      true,
				InitialCluster: initialCluster,
				ClusterToken:   cluster.Status.ClusterToken,
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
	log := log.FromContext(ctx)

	endpoints := memberEndpoints(members, cluster.Name, cluster.Namespace)
	if len(endpoints) == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	etcdClient, err := r.EtcdClientFactory(ctx, endpoints)
	if err != nil {
		log.Error(err, "etcd client construction failed", "endpoints", endpoints)
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "ClusterUnreachable",
			fmt.Sprintf("etcd client construction failed: %v", err))
		_ = r.Status().Update(ctx, cluster)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer etcdClient.Close()

	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := etcdClient.MemberList(listCtx)
	if err != nil {
		log.Error(err, "MemberList failed during cluster discovery", "endpoints", endpoints)
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "ClusterUnreachable",
			fmt.Sprintf("MemberList failed: %v", err))
		_ = r.Status().Update(ctx, cluster)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	cluster.Status.ClusterID = fmt.Sprintf("%x", resp.Header.ClusterId)
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ── Scale up ─────────────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) scaleUp(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	endpoints := memberEndpoints(members, cluster.Name, cluster.Namespace)
	etcdClient, err := r.EtcdClientFactory(ctx, endpoints)
	if err != nil {
		log.Error(err, "cannot connect to etcd for scale-up", "endpoints", endpoints)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer etcdClient.Close()

	newName := nextMemberName(cluster.Name, members)
	newPeerURL := peerURL(newName, cluster.Name, cluster.Namespace)

	addCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err = etcdClient.MemberAdd(addCtx, []string{newPeerURL}); err != nil {
		log.Error(err, "MemberAdd failed", "endpoints", endpoints)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

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
			Version:        cluster.Status.Observed.Version,
			Storage:        cluster.Status.Observed.Storage,
			Bootstrap:      false,
			InitialCluster: initialCluster,
			ClusterToken:   cluster.Status.ClusterToken,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, member, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, member); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// ── Scale down ───────────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) scaleDown(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
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
	desired := cluster.Status.Observed.Replicas

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
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "QuorumHealthy", "All members are ready")
		setClusterCondition(cluster, lll.ClusterDegraded, metav1.ConditionFalse, "QuorumHealthy", "")
	case ready > desired/2:
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "QuorumAvailable", fmt.Sprintf("%d/%d members ready", ready, desired))
		setClusterCondition(cluster, lll.ClusterDegraded, metav1.ConditionTrue, "MembersUnhealthy", fmt.Sprintf("%d/%d members ready", ready, desired))
	default:
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "QuorumLost", fmt.Sprintf("%d/%d members ready, quorum lost", ready, desired))
		setClusterCondition(cluster, lll.ClusterDegraded, metav1.ConditionTrue, "QuorumLost", "")
	}

	// Steady state: clear progressing flags.
	if reconciliationComplete(cluster, members) {
		setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionFalse, "Reconciled",
			"actual state matches status.observed")
		cluster.Status.ProgressDeadline = nil
	}

	// Surface broken members so future auto-replacement has a tested call site.
	brokenCount := 0
	for _, m := range members {
		if r.isBroken(m) {
			brokenCount++
		}
	}
	cluster.Status.BrokenMembers = int32(brokenCount)

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

// ensureService creates the Service if absent. It does not reconcile drift on
// existing Services — selectors and ports are immutable from the operator's
// perspective. A user editing them manually is going off-script and the
// breakage will be visible (members lose connectivity); we don't paper over it.
func (r *EtcdClusterReconciler) ensureService(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	name string,
	spec corev1.ServiceSpec,
) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, svc)
	if err == nil {
		return nil
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
	if r.EtcdClientFactory == nil {
		r.EtcdClientFactory = DefaultEtcdClientFactory
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&lll.EtcdCluster{}).
		Owns(&lll.EtcdMember{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// ── Deadline handling ────────────────────────────────────────────────────

// handleDeadlineExceeded is the terminal-error path. The operator stops acting
// and waits for user intervention. The shape of "intervention" depends on
// whether the cluster ever bootstrapped:
//
//  1. Before the cluster has formed (clusterID==""): the existing
//     EtcdMembers' pods have an --initial-cluster flag baked into them, and
//     etcd refuses to form unless every bootstrapping member shares an
//     identical value. There's no safe in-place recovery — the only way out
//     is to delete the EtcdCluster and recreate it. This branch surfaces a
//     BootstrapFailed condition and stops.
//
//  2. After bootstrap: the cluster is healthy, the failed reconcile only
//     left some half-done state (e.g. a scale-up that couldn't schedule).
//     The user's spec edit is treated as the intervention — when spec
//     stops matching observed, we snapshot the new spec and resume. Until
//     that happens we sit in DeadlineExceeded.
//
// We never auto-pivot during bootstrap, and never silently in steady state.
func (r *EtcdClusterReconciler) handleDeadlineExceeded(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	now metav1.Time,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if cluster.Status.ClusterID == "" {
		log.Error(nil, "bootstrap deadline exceeded; delete and recreate to recover",
			"observed", cluster.Status.Observed)
		setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionFalse, "BootstrapFailed",
			"bootstrap deadline exceeded; delete the cluster and recreate to recover")
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "BootstrapFailed",
			fmt.Sprintf("could not bootstrap %d-member cluster within deadline", cluster.Status.Observed.Replicas))
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	if specEqualsObserved(cluster) {
		log.Error(nil, "deadline exceeded; awaiting spec update",
			"observed", cluster.Status.Observed)
		setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionFalse, "DeadlineExceeded",
			"deadline exceeded; edit spec to retry, or delete the cluster")
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "DeadlineExceeded",
			fmt.Sprintf("could not reach target %+v within deadline", cluster.Status.Observed))
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Spec has been edited since the deadline expired — treat that as the
	// user's intervention and resume.
	log.Info("deadline exceeded but spec was updated; retrying with new target",
		"observed", cluster.Status.Observed, "spec", cluster.Spec)
	snapshotSpecIntoObserved(cluster)
	setProgressDeadline(cluster, now)
	setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionTrue, "RetryAfterDeadline",
		"adopting updated spec after previous deadline")
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// ── Locking-pattern helpers ──────────────────────────────────────────────

func snapshotSpecIntoObserved(cluster *lll.EtcdCluster) {
	replicas := int32(3)
	if cluster.Spec.Replicas != nil {
		replicas = *cluster.Spec.Replicas
	}
	cluster.Status.Observed = &lll.ObservedClusterSpec{
		Replicas: replicas,
		Version:  cluster.Spec.Version,
		Storage:  cluster.Spec.Storage,
	}
}

func specEqualsObserved(cluster *lll.EtcdCluster) bool {
	if cluster.Status.Observed == nil {
		return false
	}
	specReplicas := int32(3)
	if cluster.Spec.Replicas != nil {
		specReplicas = *cluster.Spec.Replicas
	}
	o := cluster.Status.Observed
	return o.Replicas == specReplicas &&
		o.Version == cluster.Spec.Version &&
		o.Storage.Cmp(cluster.Spec.Storage) == 0
}

func reconciliationComplete(cluster *lll.EtcdCluster, members []lll.EtcdMember) bool {
	if cluster.Status.Observed == nil {
		return false
	}
	if int32(len(members)) != cluster.Status.Observed.Replicas {
		return false
	}
	if cluster.Status.ClusterID == "" {
		return false
	}
	for _, m := range members {
		ready := false
		for _, c := range m.Status.Conditions {
			if c.Type == lll.MemberReady && c.Status == metav1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			return false
		}
	}
	return true
}

func setProgressDeadline(cluster *lll.EtcdCluster, now metav1.Time) {
	secs := DefaultProgressDeadlineSeconds
	if cluster.Spec.ProgressDeadlineSeconds != nil {
		secs = *cluster.Spec.ProgressDeadlineSeconds
	}
	deadline := metav1.NewTime(now.Add(time.Duration(secs) * time.Second))
	cluster.Status.ProgressDeadline = &deadline
}

func deadlineExpired(cluster *lll.EtcdCluster, now metav1.Time) bool {
	if cluster.Status.ProgressDeadline == nil {
		return false
	}
	return !now.Before(cluster.Status.ProgressDeadline)
}

func setClusterCondition(cluster *lll.EtcdCluster, condType string, status metav1.ConditionStatus, reason, msg string) {
	setCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  status,
		Reason:  reason,
		Message: msg,
	})
}
