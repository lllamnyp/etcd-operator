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

	// ── First-reconcile init ──────────────────────────────────────────
	// Combine the ClusterToken latch, initial Observed snapshot, and
	// progress deadline into a single Status write so a brand-new cluster
	// settles into a reconcile-ready shape in one pass.
	current := int32(len(active))
	now := metav1.Now()

	if cluster.Status.ClusterToken == "" || cluster.Status.Observed == nil {
		log.Info("initialising cluster status (token + observed snapshot)")
		if cluster.Status.ClusterToken == "" {
			cluster.Status.ClusterToken = deriveClusterToken(cluster)
		}
		if cluster.Status.Observed == nil {
			snapshotSpecIntoObserved(cluster)
			setProgressDeadline(cluster, now)
			setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionTrue, "InitialSnapshot",
				"snapshotted initial spec into status.observed")
		}
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// ── Subsequent spec-change handling ───────────────────────────────
	// Triggers that update Observed from the latest spec:
	//   - the previous Observed target is complete and spec has changed
	//   - the deadline has elapsed (handled separately)

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
	// Bootstrap creates a single-member etcd cluster (member -0 only) with
	// --initial-cluster-state=new. Once ClusterID is latched, scale-up adds
	// the remaining members via MemberAdd. This avoids the historical bug
	// where multiple bootstrapping members had to agree on a single
	// --initial-cluster value and any mid-flight replica change would
	// corrupt that consensus.
	if cluster.Status.ClusterID == "" {
		if current == 0 {
			log.Info("bootstrapping single-node cluster")
			return r.bootstrap(ctx, cluster)
		}
		log.Info("waiting for bootstrap member to form cluster")
		return r.tryDiscoverCluster(ctx, cluster, active)
	}

	// ── Scale ──────────────────────────────────────────────────────────
	// Before deciding to scale either direction, wait for any in-flight
	// EtcdMember deletion to finish. The EtcdMember finalizer calls
	// MemberRemove against the etcd cluster; running that concurrently
	// with a MemberAdd from scale-up, or with another MemberRemove from
	// scale-down, races past quorum because each goroutine works from its
	// own snapshot of the etcd member list. One mutation at a time.
	for _, m := range memberList.Items {
		if !m.DeletionTimestamp.IsZero() {
			log.Info("waiting for in-flight member deletion before further scaling", "deleting", m.Name)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

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

// bootstrap creates the single seed member (member-0) for a fresh cluster.
// Its --initial-cluster lists only itself, so the etcd protocol cannot get
// confused by partial agreement. Subsequent members join via MemberAdd once
// ClusterID is latched.
func (r *EtcdClusterReconciler) bootstrap(
	ctx context.Context,
	cluster *lll.EtcdCluster,
) (ctrl.Result, error) {
	name := fmt.Sprintf("%s-0", cluster.Name)
	initialCluster := buildInitialCluster([]string{name}, cluster.Name, cluster.Namespace)

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

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// ── Cluster discovery ────────────────────────────────────────────────────

func (r *EtcdClusterReconciler) tryDiscoverCluster(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Find the seed explicitly by name rather than trusting list order.
	// apiserver does not guarantee any ordering on List responses; relying
	// on `members[0]` would silently anchor discovery to the wrong member
	// (or panic on an empty slice from a future caller).
	seedName := fmt.Sprintf("%s-0", cluster.Name)
	var seed *lll.EtcdMember
	for i := range members {
		if members[i].Name == seedName {
			seed = &members[i]
			break
		}
	}
	if seed == nil {
		log.Info("waiting for seed member to appear before discovery", "seed", seedName)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	endpoints := []string{clientURL(seed.Name, cluster.Name, cluster.Namespace)}

	etcdClient, err := r.EtcdClientFactory(ctx, endpoints)
	if err != nil {
		log.Error(err, "etcd client construction failed", "endpoints", endpoints)
		setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "ClusterUnreachable",
			fmt.Sprintf("etcd client construction failed: %v", err))
		if upErr := r.Status().Update(ctx, cluster); upErr != nil {
			if errors.IsConflict(upErr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, upErr
		}
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
		if upErr := r.Status().Update(ctx, cluster); upErr != nil {
			if errors.IsConflict(upErr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, upErr
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Only latch ClusterID when the response unambiguously describes the
	// bootstrap state. With single-member bootstrap that means: exactly
	// one member, and its name matches the seed (or its peer URL does, if
	// etcd hasn't yet propagated the name).
	if len(resp.Members) != 1 {
		log.Info("waiting for definitive single-member response", "members", len(resp.Members))
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	expectedPeer := peerURL(seed.Name, cluster.Name, cluster.Namespace)
	matched := resp.Members[0].Name == seed.Name
	if !matched {
		for _, p := range resp.Members[0].PeerURLs {
			if p == expectedPeer {
				matched = true
				break
			}
		}
	}
	if !matched {
		log.Info("MemberList returned an unexpected member; retrying",
			"got_name", resp.Members[0].Name, "got_urls", resp.Members[0].PeerURLs)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	cluster.Status.ClusterID = fmt.Sprintf("%016x", resp.Header.ClusterId)
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

	// Crash-safety: a previous reconcile may have called MemberAdd
	// successfully and then failed before creating the EtcdMember CR. Check
	// etcd's current members; if our peerURL is already registered, skip
	// MemberAdd and proceed to CR creation. This lets us recover from a
	// partial scale-up without orphaning an etcd member or wedging the
	// reconcile on a duplicate-peerURL error.
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	listResp, err := etcdClient.MemberList(listCtx)
	if err != nil {
		log.Error(err, "MemberList failed during scale-up", "endpoints", endpoints)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	alreadyAdded := false
	for _, m := range listResp.Members {
		for _, p := range m.PeerURLs {
			if p == newPeerURL {
				alreadyAdded = true
				break
			}
		}
		if alreadyAdded {
			break
		}
	}

	if !alreadyAdded {
		addCtx, addCancel := context.WithTimeout(ctx, 10*time.Second)
		defer addCancel()
		if _, err = etcdClient.MemberAdd(addCtx, []string{newPeerURL}); err != nil {
			log.Error(err, "MemberAdd failed", "endpoints", endpoints)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
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

// ensureService creates the Service if absent and reconciles drift on the
// fields the operator actually owns: Selector, PublishNotReadyAddresses,
// and the named Ports it requires. Everything else (labels, annotations,
// user-added ports, type) is preserved — admission webhooks routinely
// inject these and we don't want to fight them.
func (r *EtcdClusterReconciler) ensureService(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	name string,
	desired corev1.ServiceSpec,
) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, svc)
	if errors.IsNotFound(err) {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
				Labels:    clusterLabels(cluster.Name),
			},
			Spec: desired,
		}
		if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}

	// Drift reconcile on owned fields only.
	original := svc.DeepCopy()
	changed := false

	if !equalSelectors(svc.Spec.Selector, desired.Selector) {
		svc.Spec.Selector = desired.Selector
		changed = true
	}
	if svc.Spec.PublishNotReadyAddresses != desired.PublishNotReadyAddresses {
		svc.Spec.PublishNotReadyAddresses = desired.PublishNotReadyAddresses
		changed = true
	}
	// Required ports must be present with correct values. Extra ports the
	// user has added stay.
	for _, want := range desired.Ports {
		if upsertPortByName(&svc.Spec.Ports, want) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, svc, client.MergeFrom(original))
}

func equalSelectors(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// upsertPortByName ensures `want` is present in `ports`, identified by Name.
// Existing entries with the same Name are replaced if any of Port, Protocol,
// or TargetPort differ. Returns true when the slice was modified.
func upsertPortByName(ports *[]corev1.ServicePort, want corev1.ServicePort) bool {
	for i, p := range *ports {
		if p.Name != want.Name {
			continue
		}
		if p.Port == want.Port && p.Protocol == want.Protocol && p.TargetPort == want.TargetPort {
			return false
		}
		(*ports)[i] = want
		return true
	}
	*ports = append(*ports, want)
	return true
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
		// Bootstrap terminal state. Idempotent: only persist if conditions
		// actually changed; once parked, return ctrl.Result{} and let the
		// watch wake us up if the user deletes (or edits, which won't
		// recover but will at least re-trigger reconcile).
		changed := setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionFalse, "BootstrapFailed",
			"bootstrap deadline exceeded; delete the cluster and recreate to recover")
		changed = setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "BootstrapFailed",
			fmt.Sprintf("could not bootstrap %d-member cluster within deadline", cluster.Status.Observed.Replicas)) || changed
		if changed {
			log.Error(nil, "bootstrap deadline exceeded; delete and recreate to recover",
				"observed", cluster.Status.Observed)
			if err := r.Status().Update(ctx, cluster); err != nil {
				if errors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if specEqualsObserved(cluster) {
		// Steady-state terminal state. Same idempotency: write only if
		// changed, no requeue.
		changed := setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionFalse, "DeadlineExceeded",
			"deadline exceeded; edit spec to retry, or delete the cluster")
		changed = setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "DeadlineExceeded",
			fmt.Sprintf("could not reach target %+v within deadline", cluster.Status.Observed)) || changed
		if changed {
			log.Error(nil, "deadline exceeded; awaiting spec update",
				"observed", cluster.Status.Observed)
			if err := r.Status().Update(ctx, cluster); err != nil {
				if errors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
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

// setClusterCondition writes a condition stamped with the cluster's current
// Generation as ObservedGeneration. Returns true if anything actually
// changed; callers can skip the Status().Update when nothing did.
func setClusterCondition(cluster *lll.EtcdCluster, condType string, status metav1.ConditionStatus, reason, msg string) bool {
	want := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cluster.Generation,
	}
	for _, existing := range cluster.Status.Conditions {
		if existing.Type == want.Type {
			if existing.Status == want.Status &&
				existing.Reason == want.Reason &&
				existing.Message == want.Message &&
				existing.ObservedGeneration == want.ObservedGeneration {
				return false
			}
			break
		}
	}
	setCondition(&cluster.Status.Conditions, want)
	return true
}
