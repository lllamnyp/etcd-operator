package controllers

import (
	"fmt"
	"strconv"
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

// nextMemberName returns the lowest unused ordinal name for a cluster.
func nextMemberName(cluster string, members []lll.EtcdMember) string {
	used := make(map[int]bool, len(members))
	for _, m := range members {
		if ord := memberOrdinal(m.Name); ord >= 0 {
			used[ord] = true
		}
	}
	for i := 0; ; i++ {
		if !used[i] {
			return fmt.Sprintf("%s-%d", cluster, i)
		}
	}
}

// memberOrdinal extracts the trailing integer from a member name like "cluster-2".
func memberOrdinal(name string) int {
	idx := strings.LastIndex(name, "-")
	if idx < 0 {
		return -1
	}
	n, err := strconv.Atoi(name[idx+1:])
	if err != nil {
		return -1
	}
	return n
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
// current Generation as ObservedGeneration. Mirrors setClusterCondition.
func setMemberCondition(member *lll.EtcdMember, condType string, status metav1.ConditionStatus, reason, msg string) {
	setCondition(&member.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: member.Generation,
	})
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
