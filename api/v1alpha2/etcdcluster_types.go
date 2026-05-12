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

package v1alpha2

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types for EtcdCluster.
const (
	// ClusterAvailable indicates the cluster has a healthy quorum.
	ClusterAvailable = "Available"
	// ClusterProgressing indicates a scaling or version change is in progress.
	ClusterProgressing = "Progressing"
	// ClusterDegraded indicates some members are unhealthy but quorum holds.
	ClusterDegraded = "Degraded"
)

// EtcdClusterSpec defines the desired state of an etcd cluster.
type EtcdClusterSpec struct {
	// Replicas is the desired number of cluster members. Should be odd.
	// A value of 0 parks the cluster ("scale to zero"): the last member's
	// name is latched in status.dormantMember and its PVC is preserved by
	// being re-parented to the EtcdCluster, so a later scale-up to >=1
	// resurrects the same member with the same ClusterID and data.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Version is the desired etcd version (e.g. "3.5.17").
	// +kubebuilder:validation:Pattern=`^\d+\.\d+\.\d+$`
	Version string `json:"version"`

	// Storage is the requested PVC size per member for the etcd data directory.
	// Uses the default StorageClass.
	// +kubebuilder:default="1Gi"
	// +optional
	Storage resource.Quantity `json:"storage,omitempty"`

	// ProgressDeadlineSeconds bounds the time the operator spends trying to
	// reach a desired state before abandoning the in-flight target and
	// adopting whatever the user has set as the new spec. Defaults to 600
	// (10 minutes). A patch to status.progressDeadline can shorten this for
	// a stuck reconcile (set it to "now" to abort immediately).
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +optional
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`
}

// ObservedClusterSpec is the locked-in target the controller is currently
// reconciling toward. It is updated from Spec only when the previous target
// has been reached (or its deadline has expired). Reconciliation logic uses
// these fields, not Spec — that's how the controller "ignores" further spec
// changes mid-flight.
type ObservedClusterSpec struct {
	// Replicas is the locked target replica count.
	Replicas int32 `json:"replicas"`

	// Version is the locked target etcd version.
	Version string `json:"version"`

	// Storage is the locked target PVC size.
	Storage resource.Quantity `json:"storage"`
}

// EtcdClusterStatus defines the observed state of an etcd cluster.
type EtcdClusterStatus struct {
	// ReadyMembers is the count of members that are healthy and serving.
	ReadyMembers int32 `json:"readyMembers,omitempty"`

	// BrokenMembers is the count of members the operator considers broken.
	// While the auto-replacement predicate is a stub it is always 0; surfaced
	// here so the predicate has a tested call site and the field already
	// exists when the policy lands.
	BrokenMembers int32 `json:"brokenMembers,omitempty"`

	// ClusterID is the etcd cluster ID in hex (e.g. "769f1c9e0d723d0b"),
	// set after initial bootstrap. Stored as a string because uint64 values
	// can exceed JSON's safe integer range.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// ClusterToken is the value passed to etcd's --initial-cluster-token,
	// recorded at bootstrap. Reused for all subsequent scale-up operations
	// so existing clusters keep their original token even if the derivation
	// rule changes in a later release.
	// +optional
	ClusterToken string `json:"clusterToken,omitempty"`

	// Observed is the locked-in desired state the operator is currently
	// reconciling toward. The reconciler ignores spec changes while a target
	// is in flight; Observed is only updated from spec when the current
	// target is met or its deadline has expired. nil before the first
	// reconcile.
	// +optional
	Observed *ObservedClusterSpec `json:"observed,omitempty"`

	// ProgressDeadline is the time at which the in-flight reconciliation
	// will be abandoned in favor of the latest spec. Cleared when the
	// cluster reaches Observed. Patch this to a time in the past to abort
	// a stuck reconcile.
	// +optional
	ProgressDeadline *metav1.Time `json:"progressDeadline,omitempty"`

	// Conditions represent the latest available observations of the cluster's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// DormantMember is the name of the last EtcdMember that existed before
	// the cluster scaled to zero replicas. Its PVC has been re-parented to
	// this EtcdCluster so cascading GC leaves it in place. When the cluster
	// is scaled back up to >=1 replica, the operator recreates an
	// EtcdMember with this exact name (not via GenerateName), the
	// pre-existing PVC is adopted, and the etcd process resumes from its
	// existing data dir — preserving ClusterID, member IDs, and the raft
	// log. Empty in the normal (non-dormant) state.
	// +optional
	DormantMember string `json:"dormantMember,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyMembers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EtcdCluster is the Schema for the etcdclusters API.
type EtcdCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdClusterSpec   `json:"spec,omitempty"`
	Status EtcdClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdClusterList contains a list of EtcdCluster.
type EtcdClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdCluster{}, &EtcdClusterList{})
}
