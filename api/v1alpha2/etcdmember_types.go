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

// Condition types for EtcdMember.
const (
	// MemberJoined indicates the member has been added to the etcd cluster.
	MemberJoined = "Joined"
	// MemberReady indicates the member is healthy and serving requests.
	MemberReady = "Ready"
)

// EtcdMemberSpec defines the desired state of a single etcd member.
// Created and managed by the EtcdCluster controller.
type EtcdMemberSpec struct {
	// ClusterName is the name of the owning EtcdCluster.
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`

	// Version is the etcd version for this member.
	// +kubebuilder:validation:Pattern=`^\d+\.\d+\.\d+$`
	Version string `json:"version"`

	// Storage is the requested size for this member's data directory. For
	// StorageMedium="" this is the PVC's requested capacity; for
	// StorageMedium="Memory" this is the tmpfs emptyDir's SizeLimit.
	Storage resource.Quantity `json:"storage"`

	// StorageMedium selects the volume backend for this member's data
	// directory. Mirrors EtcdCluster.spec.storageMedium at the time this
	// member was created and is not edited afterwards.
	// +optional
	StorageMedium StorageMedium `json:"storageMedium,omitempty"`

	// Bootstrap indicates this member is part of the initial cluster formation.
	// When true the member starts with --initial-cluster-state=new.
	// +optional
	Bootstrap bool `json:"bootstrap,omitempty"`

	// InitialCluster is the value passed to etcd's --initial-cluster flag.
	// Set by the cluster controller at creation time.
	InitialCluster string `json:"initialCluster"`

	// ClusterToken is the value passed to etcd's --initial-cluster-token.
	// Copied from EtcdCluster.status.clusterToken so all members of a cluster
	// agree, and so changes to the cluster's token derivation rule don't
	// affect already-running members.
	ClusterToken string `json:"clusterToken"`

	// Dormant marks the member as paused. While dormant, the member
	// controller deletes the member's Pod but leaves the PVC in place
	// (the PVC stays owned by this EtcdMember). The cluster controller
	// flips Dormant=true on the surviving member when the user sets
	// EtcdCluster.spec.replicas=0 on a 1-member cluster, and flips it
	// back to false when the user scales up. Re-creating the Pod against
	// the existing PVC lets etcd resume from the existing data dir with
	// the same ClusterID and member ID. While dormant, the member does
	// not count toward the EtcdCluster's `current` replica accounting.
	// +optional
	Dormant bool `json:"dormant,omitempty"`
}

// EtcdMemberStatus defines the observed state of a single etcd member.
type EtcdMemberStatus struct {
	// MemberID is the etcd-assigned member ID in hex (e.g. "ae36f238164a08ad"),
	// set once the member joins the cluster. Stored as a string because uint64
	// values can exceed JSON's safe integer range.
	// +optional
	MemberID string `json:"memberID,omitempty"`

	// PodName is the name of the Pod running this member.
	// +optional
	PodName string `json:"podName,omitempty"`

	// PodUID is the UID of the Pod most recently observed for this member.
	// Set when the Pod is created or found; cleared when the Pod is gone
	// and the member controller intentionally removed it (e.g. dormant).
	// For memory-backed members the operator compares the live Pod's UID
	// against this value to detect Pod loss: a stored UID with no live
	// matching Pod means the tmpfs is gone and the member must be replaced.
	// +optional
	PodUID string `json:"podUID,omitempty"`

	// IsVoter is true when etcd's MemberList reports this member with
	// IsLearner=false — i.e. it counts toward quorum. Written by the
	// cluster controller during its MemberList processing and pre-stamped
	// true at seed creation (the seed is never a learner). Read by the
	// member controller to apply the role=voter Pod label that the
	// per-cluster PodDisruptionBudget selects on. Default value false is
	// the safe-but-temporary state for a freshly-added learner before
	// MemberPromote runs.
	// +optional
	IsVoter bool `json:"isVoter,omitempty"`

	// PVCName is the name of the PersistentVolumeClaim for this member's data.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// Conditions represent the latest available observations of the member's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EtcdMember represents a single member of an etcd cluster.
// EtcdMember resources are created and deleted by the EtcdCluster controller.
// Users should not create these directly.
type EtcdMember struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdMemberSpec   `json:"spec,omitempty"`
	Status EtcdMemberStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdMemberList contains a list of EtcdMember.
type EtcdMemberList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdMember `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdMember{}, &EtcdMemberList{})
}
