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

// StorageMedium selects the volume backend for each member's etcd data
// directory. The values mirror corev1.StorageMedium semantics: the empty
// string is the default (a PVC backed by the namespace's default
// StorageClass) and "Memory" is a tmpfs emptyDir whose lifetime is bound
// to the Pod.
//
// Memory-backed clusters trade durability for speed: a Pod that loses its
// tmpfs (eviction, node failure, deletion) loses its data and the member
// must be replaced. The operator detects this and removes the member via
// the existing finalizer flow; the cluster controller's scale-up gap-fill
// then creates a replacement member. This works only when quorum holds
// across the loss, so a single-replica memory cluster cannot survive a
// Pod eviction.
//
// Production memory clusters should also have a PodDisruptionBudget, hard
// pod-anti-affinity, and a container memory limit covering the tmpfs size
// plus etcd's own headroom. None of that is auto-emitted by the operator
// yet — see https://github.com/lllamnyp/etcd-operator/issues/16.
//
// +kubebuilder:validation:Enum="";Memory
type StorageMedium string

const (
	// StorageMediumDefault uses a PersistentVolumeClaim per member.
	StorageMediumDefault StorageMedium = ""
	// StorageMediumMemory uses a tmpfs emptyDir per member.
	StorageMediumMemory StorageMedium = "Memory"
)

// EtcdClusterSpec defines the desired state of an etcd cluster.
//
// CEL validation rules (k8s 1.28+ apiserver-enforced):
//
//   - StorageMedium=Memory + Replicas=0 wedges the cluster on resume
//     (the dormant flip deletes the Pod and the tmpfs goes with it but
//     the resume path treats the member as if its data were preserved).
//     Reject the combination outright; recreate is the only safe path.
//
//   - StorageMedium=Memory requires Storage > 0. Without a SizeLimit the
//     tmpfs is unbounded against node memory, which defeats the whole
//     point of opting into memory backing.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.replicas) && self.replicas == 0 && has(self.storageMedium) && self.storageMedium == 'Memory')",message="spec.replicas=0 with spec.storageMedium=Memory is unsupported: pausing a memory-backed cluster wedges on resume. Delete and recreate the cluster instead."
// +kubebuilder:validation:XValidation:rule="!(has(self.storageMedium) && self.storageMedium == 'Memory') || quantity(self.storage).isGreaterThan(quantity('0'))",message="spec.storage must be > 0 when spec.storageMedium=Memory (the tmpfs sizeLimit cannot be zero)."
type EtcdClusterSpec struct {
	// Replicas is the desired number of cluster members. Should be odd.
	// A value of 0 parks the cluster ("scale to zero"): the operator
	// flips spec.dormant=true on the surviving EtcdMember, which causes
	// the member controller to delete that member's Pod. The EtcdMember
	// CR and its PVC are preserved (the PVC stays owned by the same
	// EtcdMember across the pause). Scaling back up to >=1 flips
	// spec.dormant=false on the same member; etcd resumes from its
	// existing data dir with the same ClusterID and member ID.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Version is the desired etcd version (e.g. "3.5.17").
	// +kubebuilder:validation:Pattern=`^\d+\.\d+\.\d+$`
	Version string `json:"version"`

	// Storage is the requested size per member for the etcd data directory.
	// For StorageMedium="" (PVC) this is the PVC's requested capacity backed by
	// the default StorageClass. For StorageMedium="Memory" this is the tmpfs
	// emptyDir's SizeLimit.
	//
	// Shrinking is rejected on UPDATE: PVCs cannot shrink and tmpfs SizeLimit
	// reduction does not free already-allocated memory.
	// +kubebuilder:default="1Gi"
	// +kubebuilder:validation:XValidation:rule="quantity(self).compareTo(quantity(oldSelf)) >= 0",message="spec.storage cannot be shrunk"
	// +optional
	Storage resource.Quantity `json:"storage,omitempty"`

	// StorageMedium selects the volume backend for each member's data
	// directory. Empty string (the default) means a PVC; "Memory" means a
	// tmpfs emptyDir whose lifetime is bound to the Pod. See the
	// StorageMedium type doc for the operational trade-offs.
	//
	// Immutable: changing the medium on an existing cluster would orphan the
	// previous PVC (or tmpfs) and the rolling-migrate path is not implemented.
	// Pausing a memory-backed cluster (Replicas=0 + Memory) is rejected by
	// the EtcdClusterSpec-level CEL rules above — the tmpfs evaporates with
	// the Pod and resume would silently produce an empty data dir.
	//
	// The default ("") is set explicitly (rather than relying on Go's zero
	// value) so the apiserver always stores the field on Create. This makes
	// `oldSelf` present on Update, which the CEL transition rule below
	// requires — without the default, a first-time set from absent → Memory
	// would slip past the immutability check.
	// +kubebuilder:default=""
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.storageMedium is immutable; delete and recreate the cluster to change the storage backend"
	// +optional
	StorageMedium StorageMedium `json:"storageMedium,omitempty"`

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

	// Storage is the locked target data-directory size.
	Storage resource.Quantity `json:"storage"`

	// StorageMedium is the locked target storage backend. The locking
	// pattern prevents a mid-flight medium flip from being honoured until
	// the current target is reached or its deadline expires.
	// +optional
	StorageMedium StorageMedium `json:"storageMedium,omitempty"`
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
