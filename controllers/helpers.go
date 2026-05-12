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

	// EtcdImage is the container image repository for etcd.
	EtcdImage = "quay.io/coreos/etcd"

	// MemberFinalizer is placed on EtcdMember resources to ensure
	// graceful removal from the etcd cluster before deletion.
	MemberFinalizer = "etcd.lllamnyp.su/member-cleanup"

	// PauseAnnotation is set on an EtcdMember by the cluster controller
	// just before issuing Delete on the 1→0 scale-down step. The member
	// controller's finalizer keys off this annotation to take the
	// scale-to-zero "pause" path (reparent PVC to cluster, skip
	// MemberRemove).
	//
	// Why an annotation on the member rather than reading cluster
	// status: controller-runtime caches the EtcdCluster and EtcdMember
	// informers independently. When the member-controller's finalizer
	// fires on the Delete event, the EtcdCluster cache may not yet
	// reflect the status.DormantMember Status() update written by the
	// cluster controller a few microseconds earlier — that would cause
	// the finalizer to fall through to the normal MemberRemove path,
	// then silently no-op for the last member (no peers to dial), and
	// cascade GC would take the PVC. By stamping the signal onto the
	// member CR itself, we read it from the same object whose Delete
	// event triggered the reconcile — guaranteed cache-coherent.
	PauseAnnotation = "etcd.lllamnyp.su/pause"
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

// memberEndpoints returns etcd client endpoints for a set of members.
func memberEndpoints(members []lll.EtcdMember, cluster, namespace string) []string {
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

// isBroken decides whether a member should be treated as broken (and replaced).
// Stub: always false. Filled in once auto-replacement policy is decided.
func (r *EtcdClusterReconciler) isBroken(_ lll.EtcdMember) bool {
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
