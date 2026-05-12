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
	"strings"
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

	// If the cluster is being deleted, don't keep reconciling it. Owned
	// resources are cascaded out via owner refs; recreating a Service for a
	// Terminating cluster races against the GC and pollutes logs.
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
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
		// Bootstrap path also runs when an existing seed CR is pending its
		// InitialCluster patch (recovery from a crash between Create and
		// Update). Without that, the seed sits forever with empty spec and
		// the member controller refuses to start its pod.
		if current == 0 || hasPendingBootstrap(active) {
			log.Info("bootstrapping single-node cluster")
			return r.bootstrap(ctx, cluster, active)
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
		// Don't add another member while existing ones aren't Ready yet.
		// Even with learner-mode adds (which don't shift voting quorum until
		// promotion), driving a sequence of MemberAddAsLearner without
		// waiting for the previous pod to come up makes the cluster
		// progressively more confused — the new pod has to fetch state from
		// peers, and stacking joiners against a half-empty cluster is at
		// best slow, at worst stalls indefinitely. Wait for everyone Ready
		// before adding the next.
		if !allMembersReady(active) {
			log.Info("waiting for existing members to become Ready before next scale-up step")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		log.Info("scaling up", "current", current, "desired", desired)
		return r.scaleUp(ctx, cluster, active)
	}
	if current > desired {
		log.Info("scaling down", "current", current, "desired", desired)
		return r.scaleDown(ctx, cluster, active)
	}

	// ── current == desired ────────────────────────────────────────────
	// One subtlety: the iteration that added the *last* learner returns
	// before that learner gets promoted (current is now equal to desired
	// so scaleUp won't run again on its own). We need a promote attempt
	// here too. Cheap: list etcd once and try to promote any learner;
	// no-op if none.
	if cluster.Status.ClusterID != "" && len(active) > 0 {
		endpoints := memberEndpoints(active, cluster.Name, cluster.Namespace)
		etcdClient, err := r.EtcdClientFactory(ctx, endpoints)
		if err == nil {
			defer etcdClient.Close()
			_, res, perr := r.promotePendingLearner(ctx, etcdClient, endpoints)
			if perr != nil {
				return ctrl.Result{}, perr
			}
			if res != nil {
				return *res, nil
			}
		}
		// If we couldn't build a client, fall through to updateStatus —
		// the next reconcile will retry.
	}

	// ── Steady state ───────────────────────────────────────────────────
	return r.updateStatus(ctx, cluster, active)
}

// ── Bootstrap ────────────────────────────────────────────────────────────

// bootstrap brings up the single seed member for a fresh cluster. Its
// --initial-cluster lists only itself, so the etcd protocol cannot get
// confused by partial agreement; subsequent members join via MemberAdd
// once ClusterID is latched.
//
// Members are named via GenerateName ("<cluster>-"), not ordinal-suffixed,
// so seed identity is detected by Spec.Bootstrap=true rather than by a
// predictable name. Idempotency therefore looks for an existing
// Bootstrap=true CR before creating a new one. Cross-incarnation safety
// (a leftover member with this cluster's label but a different
// controller) is enforced explicitly — see comment below.
//
// The Create-then-Update sequence is required: the seed's --initial-cluster
// flag references its own (apiserver-assigned) name, so we can only fill
// Spec.InitialCluster *after* Create returns the assigned name. The member
// controller refuses to start a pod with an empty InitialCluster (see
// EtcdMemberReconciler.Reconcile), so the half-baked state between Create
// and Update doesn't reach the data plane.
func (r *EtcdClusterReconciler) bootstrap(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	var seed *lll.EtcdMember
	for i := range members {
		if members[i].Spec.Bootstrap {
			seed = &members[i]
			break
		}
	}

	if seed == nil {
		seed = &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: cluster.Name + "-",
				Namespace:    cluster.Namespace,
				Labels:       clusterLabels(cluster.Name),
			},
			Spec: lll.EtcdMemberSpec{
				ClusterName:  cluster.Name,
				Version:      cluster.Status.Observed.Version,
				Storage:      cluster.Status.Observed.Storage,
				Bootstrap:    true,
				ClusterToken: cluster.Status.ClusterToken,
				// InitialCluster filled in below once apiserver assigns Name.
			},
		}
		if err := controllerutil.SetControllerReference(cluster, seed, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, seed); err != nil {
			return ctrl.Result{}, err
		}
	} else if !metav1.IsControlledBy(seed, cluster) {
		// Defence-in-depth: SetControllerReference on Create would have
		// rejected any cross-controller member, but a bare label match
		// with no owner ref could in principle slip through (manual
		// kubectl creation, orphan from a botched migration). Refuse to
		// adopt rather than risk treating someone else's etcd member
		// as ours.
		return ctrl.Result{}, fmt.Errorf(
			"bootstrap-flagged EtcdMember %q in namespace %q is not controlled by this EtcdCluster (uid=%s); "+
				"refusing to adopt", seed.Name, seed.Namespace, cluster.UID)
	}

	if seed.Spec.InitialCluster == "" {
		original := seed.DeepCopy()
		seed.Spec.InitialCluster = buildInitialCluster([]string{seed.Name}, cluster.Name, cluster.Namespace)
		if err := r.Patch(ctx, seed, client.MergeFrom(original)); err != nil {
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

	// Find the seed by Spec.Bootstrap. Member names are apiserver-assigned
	// via GenerateName, so the seed cannot be located by a predictable
	// name; Spec.Bootstrap is set on (and only on) the single CR that
	// bootstrap() created. apiserver does not guarantee any List ordering,
	// so trusting members[0] would silently anchor discovery to the wrong
	// member when scale-up CRs land in front of the seed.
	var seed *lll.EtcdMember
	for i := range members {
		if members[i].Spec.Bootstrap {
			seed = &members[i]
			break
		}
	}
	if seed == nil {
		log.Info("waiting for seed member to appear before discovery")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// If the seed's Pod hasn't been created yet, dialing its DNS name just
	// times out the reconcile budget for no signal. Surface the wait as
	// Progressing=True/WaitingForSeed — distinct from
	// Available=False/ClusterUnreachable (which is reserved for "we
	// expected to dial something and couldn't"). Skips the Status write if
	// the condition didn't move (e.g. we already wrote WaitingForSeed last
	// reconcile).
	if seed.Status.PodName == "" {
		if setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionTrue, "WaitingForSeed",
			fmt.Sprintf("seed member %q has no Pod yet", seed.Name)) {
			if upErr := r.Status().Update(ctx, cluster); upErr != nil {
				if errors.IsConflict(upErr) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, upErr
			}
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	endpoints := []string{clientURL(seed.Name, cluster.Name, cluster.Namespace)}

	etcdClient, err := r.EtcdClientFactory(ctx, endpoints)
	if err != nil {
		return r.surfaceDiscoveryError(ctx, cluster, "etcd client construction failed", err)
	}
	defer etcdClient.Close()

	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := etcdClient.MemberList(listCtx)
	if err != nil {
		return r.surfaceDiscoveryError(ctx, cluster, "MemberList failed", err)
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

	// Discovery success. Latch ClusterID and clear any stale failure
	// condition from earlier retries so the cluster doesn't sit
	// Available=False until the next reconcile cycle.
	cluster.Status.ClusterID = fmt.Sprintf("%016x", resp.Header.ClusterId)
	setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "ClusterDiscovered",
		fmt.Sprintf("etcd cluster %s discovered", cluster.Status.ClusterID))
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// surfaceDiscoveryError reports that the seed pod exists but we can't talk
// to it. Gates the Status write on whether the condition actually moved so
// we don't bump resourceVersion every 10 s while waiting for the etcd
// process to come up. Errors with embedded timestamps would defeat the
// short-circuit, so we strip the variable portion of common timeouts.
func (r *EtcdClusterReconciler) surfaceDiscoveryError(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	what string,
	err error,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Error(err, what)
	msg := fmt.Sprintf("%s: %s", what, stableErrorMessage(err))
	if setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "ClusterUnreachable", msg) {
		if upErr := r.Status().Update(ctx, cluster); upErr != nil {
			if errors.IsConflict(upErr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, upErr
		}
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// stableErrorMessage strips per-call variable portions (timestamps, addresses
// embedded in DNS-lookup errors, etc.) from common etcd-client error strings.
// Keeps the kind-of-error stable across retries so the condition message
// doesn't move on every reconcile.
func stableErrorMessage(err error) string {
	s := err.Error()
	// "lookup foo on 127.0.0.53:53: server misbehaving" -> "lookup foo: server misbehaving"
	if i, j := strings.Index(s, " on "), strings.Index(s, ": "); i > 0 && j > i {
		s = s[:i] + s[j:]
	}
	return s
}

// ── Scale up ─────────────────────────────────────────────────────────────

// scaleUp adds one member to the etcd cluster. The flow is:
//
//  1. List active members. If any has empty Spec.InitialCluster, it is
//     pending from a prior reconcile that crashed between Create and the
//     InitialCluster patch — adopt it instead of creating another.
//  2. Otherwise Create a new EtcdMember with GenerateName="<cluster>-".
//     The Create response carries the apiserver-assigned Name, which we
//     use to derive the peer URL.
//  3. If the peer URL is already in etcd's member list (recovery from a
//     prior reconcile that crashed between MemberAddAsLearner and the
//     InitialCluster patch), skip the add. Otherwise MemberAddAsLearner.
//  4. Patch the CR's Spec.InitialCluster from etcd's authoritative view.
//
// Until step 4 completes, the member controller refuses to start a pod —
// see EtcdMemberReconciler.Reconcile. That keeps half-baked state from
// reaching the data plane while still letting the finalizer remove an
// already-added learner if the user deletes the CR mid-flight.
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

	listResp, res, err := r.promotePendingLearner(ctx, etcdClient, endpoints)
	if err != nil || res != nil {
		return resultOrZero(res), err
	}

	// Step 1: adopt a pending member if one exists from a prior reconcile.
	var pending *lll.EtcdMember
	for i := range members {
		if !members[i].Spec.Bootstrap && members[i].Spec.InitialCluster == "" {
			pending = &members[i]
			break
		}
	}

	// Step 2: otherwise Create a fresh CR with GenerateName.
	if pending == nil {
		newMember := &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: cluster.Name + "-",
				Namespace:    cluster.Namespace,
				Labels:       clusterLabels(cluster.Name),
			},
			Spec: lll.EtcdMemberSpec{
				ClusterName:  cluster.Name,
				Version:      cluster.Status.Observed.Version,
				Storage:      cluster.Status.Observed.Storage,
				Bootstrap:    false,
				ClusterToken: cluster.Status.ClusterToken,
				// InitialCluster filled in below.
			},
		}
		if err := controllerutil.SetControllerReference(cluster, newMember, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, newMember); err != nil {
			return ctrl.Result{}, err
		}
		pending = newMember
	} else if !metav1.IsControlledBy(pending, cluster) {
		// Defence-in-depth, same reasoning as bootstrap().
		return ctrl.Result{}, fmt.Errorf(
			"pending EtcdMember %q in namespace %q is not controlled by this EtcdCluster (uid=%s); "+
				"refusing to adopt", pending.Name, pending.Namespace, cluster.UID)
	}

	newName := pending.Name
	newPeerURL := peerURL(newName, cluster.Name, cluster.Namespace)

	// Step 3: skip MemberAddAsLearner if the peer URL is already registered.
	// This covers the crash between MemberAddAsLearner success and the
	// InitialCluster patch on a previous reconcile.
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

	// `etcdMembers` is the authoritative list of cluster members we'll use
	// to compute the new pod's --initial-cluster flag. If MemberAddAsLearner
	// has just succeeded, its response carries the updated membership; if
	// the learner was already there (crash recovery), listResp already does.
	etcdMembers := listResp.Members
	if !alreadyAdded {
		addCtx, addCancel := context.WithTimeout(ctx, 10*time.Second)
		defer addCancel()
		addResp, err := etcdClient.MemberAddAsLearner(addCtx, []string{newPeerURL})
		if err != nil {
			log.Error(err, "MemberAddAsLearner failed", "endpoints", endpoints)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Info("added member as learner", "name", newName)
		etcdMembers = addResp.Members
	}

	// Step 4: build --initial-cluster from etcd's view, not the CR list. The
	// two can diverge during crash recovery (etcd has a learner whose CR
	// hasn't been finalised) and the new pod's flags MUST match what etcd
	// will report when the pod tries to join.
	allNames := make([]string, 0, len(etcdMembers))
	for _, m := range etcdMembers {
		name := m.Name
		if name == "" && len(m.PeerURLs) > 0 {
			name = memberNameFromPeerURL(m.PeerURLs[0])
		}
		if name != "" {
			allNames = append(allNames, name)
		}
	}
	sort.Strings(allNames)
	initialCluster := buildInitialCluster(allNames, cluster.Name, cluster.Namespace)

	original := pending.DeepCopy()
	pending.Spec.InitialCluster = initialCluster
	if err := r.Patch(ctx, pending, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// promotePendingLearner runs MemberList against the etcd client, and if any
// member is still a learner, attempts MemberPromote.
//
// Returns (listResp, nil, nil) when there's nothing to promote — caller can
// use listResp for follow-up work (e.g. crash-safety peerURL lookup).
// Returns (_, &Result, nil) when a promotion happened OR a not-yet-caught-up
// error means we should wait.
// Returns (_, nil, err) only on apiserver-style failures of MemberList.
func (r *EtcdClusterReconciler) promotePendingLearner(
	ctx context.Context,
	etcdClient EtcdClusterClient,
	endpoints []string,
) (*clientv3.MemberListResponse, *ctrl.Result, error) {
	log := log.FromContext(ctx)

	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	listResp, err := etcdClient.MemberList(listCtx)
	if err != nil {
		log.Error(err, "MemberList failed", "endpoints", endpoints)
		return nil, &ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	for _, m := range listResp.Members {
		if !m.IsLearner {
			continue
		}
		promoteCtx, pCancel := context.WithTimeout(ctx, 10*time.Second)
		_, perr := etcdClient.MemberPromote(promoteCtx, m.ID)
		pCancel()
		if perr != nil {
			// ErrLearnerNotReady and friends just mean wait.
			log.Info("learner not yet promotable; will retry",
				"learner_id", fmt.Sprintf("%016x", m.ID), "err", perr.Error())
			return listResp, &ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		log.Info("promoted learner", "learner_id", fmt.Sprintf("%016x", m.ID))
		return listResp, &ctrl.Result{Requeue: true}, nil
	}
	return listResp, nil, nil
}

func resultOrZero(r *ctrl.Result) ctrl.Result {
	if r == nil {
		return ctrl.Result{}
	}
	return *r
}

// allMembersReady reports whether every member's MemberReady condition is
// True. An empty slice trivially passes — there's nothing to wait on.
func allMembersReady(members []lll.EtcdMember) bool {
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

// ── Scale down ───────────────────────────────────────────────────────────

// scaleDown removes the most-recently-created member. With apiserver-
// assigned names there is no ordinal to sort by; we pick by
// CreationTimestamp (newest first) instead, which naturally retires the
// most recently added scale-up step before touching older members.
// Tiebreak by name so two members created in the same second produce a
// deterministic choice.
func (r *EtcdClusterReconciler) scaleDown(
	ctx context.Context,
	cluster *lll.EtcdCluster,
	members []lll.EtcdMember,
) (ctrl.Result, error) {
	sort.Slice(members, func(i, j int) bool {
		ai, aj := members[i].CreationTimestamp, members[j].CreationTimestamp
		if !ai.Equal(&aj) {
			return ai.After(aj.Time)
		}
		return members[i].Name > members[j].Name
	})
	victim := members[0]

	if err := r.Delete(ctx, &victim); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// hasPendingBootstrap reports whether the bootstrap seed exists but is
// missing its InitialCluster spec — the state we land in after a crash
// between Create and the InitialCluster patch.
func hasPendingBootstrap(members []lll.EtcdMember) bool {
	for _, m := range members {
		if m.Spec.Bootstrap && m.Spec.InitialCluster == "" {
			return true
		}
	}
	return false
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

	changed := false
	if cluster.Status.ReadyMembers != ready {
		cluster.Status.ReadyMembers = ready
		changed = true
	}

	switch {
	case ready == desired:
		if setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "QuorumHealthy", "All members are ready") {
			changed = true
		}
		if setClusterCondition(cluster, lll.ClusterDegraded, metav1.ConditionFalse, "QuorumHealthy", "") {
			changed = true
		}
	case ready > desired/2:
		msg := fmt.Sprintf("%d/%d members ready", ready, desired)
		if setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "QuorumAvailable", msg) {
			changed = true
		}
		if setClusterCondition(cluster, lll.ClusterDegraded, metav1.ConditionTrue, "MembersUnhealthy", msg) {
			changed = true
		}
	default:
		if setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionFalse, "QuorumLost",
			fmt.Sprintf("%d/%d members ready, quorum lost", ready, desired)) {
			changed = true
		}
		if setClusterCondition(cluster, lll.ClusterDegraded, metav1.ConditionTrue, "QuorumLost", "") {
			changed = true
		}
	}

	if reconciliationComplete(cluster, members) {
		if setClusterCondition(cluster, lll.ClusterProgressing, metav1.ConditionFalse, "Reconciled",
			"actual state matches status.observed") {
			changed = true
		}
		if cluster.Status.ProgressDeadline != nil {
			cluster.Status.ProgressDeadline = nil
			changed = true
		}
	}

	brokenCount := int32(0)
	for _, m := range members {
		if r.isBroken(m) {
			brokenCount++
		}
	}
	if cluster.Status.BrokenMembers != brokenCount {
		cluster.Status.BrokenMembers = brokenCount
		changed = true
	}

	if changed {
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
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
