package controllers

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

const (
	// LabelCluster is the label key used to associate resources with an EtcdCluster.
	LabelCluster = "etcd.lllamnyp.su/cluster"

	// LabelRole identifies the etcd-side raft role of a member's Pod. The
	// only value the operator emits today is RoleVoter; learners carry no
	// LabelRole at all so the per-cluster PodDisruptionBudget can select
	// voters exclusively (its selector requires LabelRole=RoleVoter).
	LabelRole = "etcd.lllamnyp.su/role"
	RoleVoter = "voter"

	// EtcdImage is the container image repository for etcd.
	EtcdImage = "quay.io/coreos/etcd"

	// MemberFinalizer is placed on EtcdMember resources to ensure
	// graceful removal from the etcd cluster before deletion.
	MemberFinalizer = "etcd.lllamnyp.su/member-cleanup"
)

// peerURL returns the etcd peer URL for a member, using the headless Service DNS.
func peerURL(member, cluster, namespace string) string {
	return fmt.Sprintf("http://%s.%s.%s.svc:2380", member, cluster, namespace)
}

// clientURL returns the etcd client URL for a member.
func clientURL(member, cluster, namespace string) string {
	return fmt.Sprintf("http://%s.%s.%s.svc:2379", member, cluster, namespace)
}

// buildInitialCluster builds the --initial-cluster flag value from member names.
func buildInitialCluster(names []string, cluster, namespace string) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = name + "=" + peerURL(name, cluster, namespace)
	}
	return strings.Join(parts, ",")
}

// memberEndpoints returns etcd client endpoints for the subset of
// `members` we can safely dial cluster-management RPCs against — i.e.
// members that are not currently learners.
//
// Why filter: etcd refuses several RPCs (MemberList, MemberAdd,
// MemberPromote, ...) on learner endpoints with
// "rpc not supported for learner". clientv3's balancer round-robins
// through whatever endpoints we hand it, so an unfiltered list lets
// reconcile calls land on the learner intermittently and fail. The
// failure is not just noisy — when no voter is reachable in the retry
// budget (5–10s context), the operator wedges: scaleUp's promote step
// can't see the cluster, allMembersReady gate never opens, and the
// pod that needs promoting never gets a memberID populated.
//
// "Ready=True" is the right proxy for "voter": the member-controller
// sets Ready=True only after MemberID is populated via discoverMemberID
// — which itself needs MemberList to succeed against a voter. So
// Ready=True transitively implies the member has been observed as a
// non-learner. Members still in the learner state (no MemberID yet,
// Ready=False) get filtered out.
//
// Falls back to the unfiltered list when no member is yet Ready, which
// covers (a) bootstrap discovery (only the seed exists, its Ready
// status doesn't matter for this dialer), and (b) a single fresh
// learner's own discoverMemberID call where the peer list is just
// "self" — letting the dialer try anyway is no worse than silently
// returning [].
func memberEndpoints(members []lll.EtcdMember, cluster, namespace string) []string {
	voters := make([]string, 0, len(members))
	for _, m := range members {
		if m.Status.IsVoter {
			voters = append(voters, clientURL(m.Name, cluster, namespace))
		}
	}
	if len(voters) > 0 {
		return voters
	}
	eps := make([]string, len(members))
	for i, m := range members {
		eps[i] = clientURL(m.Name, cluster, namespace)
	}
	return eps
}

// clusterLabels returns the standard labels for cluster-level resources.
func clusterLabels(cluster string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "etcd",
		"app.kubernetes.io/instance":   cluster,
		"app.kubernetes.io/managed-by": "etcd-operator",
		LabelCluster:                   cluster,
	}
}

// memberLabels returns the standard labels for member-level resources.
func memberLabels(cluster, member string) map[string]string {
	l := clusterLabels(cluster)
	l["app.kubernetes.io/component"] = member
	return l
}

// filterActiveMembers returns members that are not being deleted.
func filterActiveMembers(members []lll.EtcdMember) []lll.EtcdMember {
	var active []lll.EtcdMember
	for i := range members {
		if members[i].DeletionTimestamp.IsZero() {
			active = append(active, members[i])
		}
	}
	return active
}

// filterRunningMembers returns active (non-deleting) members that are not
// dormant. The cluster controller's replica accounting (`current`),
// readiness gating, and most scale decisions operate on this set —
// dormant members have no Pod and contribute no etcd capacity, so
// counting them would mean "1-member cluster paused via spec.replicas=0"
// looks like a 1-member cluster from the operator's perspective and
// scale-back-up could never decide to wake the dormant member.
func filterRunningMembers(members []lll.EtcdMember) []lll.EtcdMember {
	var running []lll.EtcdMember
	for i := range members {
		if !members[i].DeletionTimestamp.IsZero() {
			continue
		}
		if members[i].Spec.Dormant {
			continue
		}
		running = append(running, members[i])
	}
	return running
}

// findDormantMember returns the first non-deleting member with
// Spec.Dormant=true, or nil if none. By construction the operator never
// creates more than one dormant member at a time (only the 1→0 step
// flips dormant, and the 0→1 step flips it back before any further
// scale-up), but the helper just returns the first match.
func findDormantMember(members []lll.EtcdMember) *lll.EtcdMember {
	for i := range members {
		if !members[i].DeletionTimestamp.IsZero() {
			continue
		}
		if members[i].Spec.Dormant {
			return &members[i]
		}
	}
	return nil
}

// memberNameFromPeerURL recovers the EtcdMember name from a peer URL of the
// shape http://<member>.<cluster>.<namespace>.svc:2380. Used during scale-up
// when etcd's MemberList may report a member with Name=="" — the window
// between MemberAddAsLearner and the new pod reporting its identity. Returns
// "" if the URL doesn't match the expected shape.
func memberNameFromPeerURL(u string) string {
	s := strings.TrimPrefix(u, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.LastIndex(s, ":"); i > 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "."); i > 0 {
		return s[:i]
	}
	return ""
}

func ptrBool(b bool) *bool    { return &b }
func ptrInt64(i int64) *int64 { return &i }

// deriveClusterToken returns the etcd --initial-cluster-token value for a
// cluster. Includes namespace + UID so two same-named clusters in different
// namespaces never share a token (etcd uses the token as a sanity check
// against accidental cross-cluster peer traffic). Recorded in
// EtcdCluster.status.clusterToken at bootstrap so future changes to this
// derivation rule don't break already-running clusters.
func deriveClusterToken(cluster *lll.EtcdCluster) string {
	return fmt.Sprintf("%s-%s-%s", cluster.Namespace, cluster.Name, cluster.UID)
}

// isBroken decides whether a member should be treated as broken. In the
// current implementation the field it drives — EtcdCluster.status.broken
// Members — stays at 0 in practice, because the member controller
// detects memory-backed Pod loss and self-deletes the EtcdMember in the
// same reconcile pass; by the time updateStatus runs over the running
// set the lost member is already Terminating and filtered out.
//
// The predicate is left in place as a hook for future broken-member
// detection policies that don't tear the member down immediately (e.g.
// PVC corruption with a grace period, irrecoverable crashloop with a
// retry budget). The "memory member with PodUID recorded but PodName
// empty" condition would only arise if the member-controller's loss
// path is delayed or fails after a partial Status write — defensive
// rather than expected. For PVC-backed members the predicate stays a
// stub.
func (r *EtcdClusterReconciler) isBroken(m lll.EtcdMember) bool {
	if m.Spec.StorageMedium == lll.StorageMediumMemory {
		return m.Status.PodUID != "" && m.Status.PodName == ""
	}
	return false
}

// setMemberCondition stamps an EtcdMember condition with the resource's
// current Generation as ObservedGeneration. Returns true when something
// actually changed; callers skip the Status().Update if nothing did, so the
// member controller doesn't write the same status back every 30 seconds
// just because of a periodic reconcile.
func setMemberCondition(member *lll.EtcdMember, condType string, status metav1.ConditionStatus, reason, msg string) bool {
	want := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: member.Generation,
	}
	for _, existing := range member.Status.Conditions {
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
	setCondition(&member.Status.Conditions, want)
	return true
}

// setCondition inserts or updates a condition, preserving LastTransitionTime
// when the status has not changed.
func setCondition(conditions *[]metav1.Condition, c metav1.Condition) {
	now := metav1.Now()
	for i, existing := range *conditions {
		if existing.Type == c.Type {
			if existing.Status == c.Status {
				c.LastTransitionTime = existing.LastTransitionTime
			} else {
				c.LastTransitionTime = now
			}
			(*conditions)[i] = c
			return
		}
	}
	c.LastTransitionTime = now
	*conditions = append(*conditions, c)
}
