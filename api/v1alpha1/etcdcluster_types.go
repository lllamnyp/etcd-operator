/*
Copyright 2020 Timofey Larkin.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// EtcdClusterSpec defines the desired state of EtcdCluster
type EtcdClusterSpec struct {

	// Selector is the set of labels to match managed EtcdPeers against.
	Selector map[string]string `json:"selector"`

	// ClusterSize is the number of peers, excluding bootstrap peers.
	// +kubebuilder:default:=1
	// TODO: expect problems here as omitempty and zero value of int interact in weird ways
	ClusterSize int `json:"clusterSize,omitempty"`

	// BootstrapSpec describes the rules by which the EtcdCluster attaches
	// existing members of an etcd cluster.
	BootstrapSpec BootstrapSpec `json:"bootstrapSpec"`

	// TrustedCA is the name of the TLS-secret holding the CA key pair
	// which must sign client certificates used for client authentication
	// when attempting to connect to the etcd cluster. The secret can
	// be automatically generated, but must already exist and correspond
	// to the key pair of the bootstrap cluster when attaching to one.
	TrustedCA string `json:"trustedCA"`

	// PeerTrustedCA is the name of the TLS-secret holding the CA key pair
	// which must sign certificates used for authentication during peer-
	// to-peer communication within the etcd cluster. The secret can
	// be automatically generated, but must already exist and correspond
	// to the key pair of the bootstrap cluster when attaching to one.
	PeerTrustedCA string `json:"peerTrustedCA"`

	// HostNetwork determines whether etcd pods are launched in the host
	// network namespace or usage of the in-cluster CNI is attempted.
	// +kubebuilder:default:=true
	HostNetwork bool `json:"hostNetwork"`

	// AntiAffinityMode determines the type of affinity rules applied to
	// the etcd pods. Valid values are `Required´, `Preferred´, or `None´.
	// By default, the etcd pods of a single cluster are created with an
	// inter-pod anti-affinity rule preventing them from being scheduled
	// on one node. This restriction can be softened to the equivalent of
	// `preferredDuringSchedulingIgnoredDuringExecution´.
	// When hostNetwork == true, the behavior is always equivalent to
	// `Required´ since the pods are claiming the same host port.
	AntiAffinityMode AntiAffinityMode `json:"antiAffinityMode"`

	// StorageSpec describes the underlying storage for the managed peers.
	// Possible types of storage are `hostPath´ and `emptyDir´. The
	// emptyDir can be backed by memory.
	StorageSpec StorageSpec `json:"storageSpec"`
}

// EtcdClusterStatus defines the observed state of EtcdCluster
type EtcdClusterStatus struct {

	// TODO: type def here
	Peers int `json:"peers"`

	// TODO: type def here
	BootstrapPeers int `json:"bootstrapPeers"`

	// Phase
	// +kubebuilder:default:=New
	Phase Phase `json:"phase"`
}

type BootstrapSpec struct {

	// Enabled shows if this EtcdCluster attaches itself to existing etcd peers.
	// +kubebuilder:default:=false
	Enabled bool `json:"enabled"`

	// Selector contains a set of labels matching existing EtcdBootstrapPeers
	Selector map[string]string `json:"selector,omitempty"`
}

type StorageSpec struct {

	// TODO: description
	Type StorageType `json:"type"`

	// TODO: description
	HostPath string `json:"hostPath"`

	// TODO: description
	Backend BackendType `json:"backend"`
}

// TODO: correct spec for enum
type StorageType string

const (
	StorageTypeHostPath StorageType = "HostPath"
	StorageTypeEmptyDir StorageType = "EmptyDir"
)

// TODO: correct spec for enum
type BackendType string

const (
	BackendTypeDisk   BackendType = "Disk"
	BackendTypeMemory BackendType = "Memory"
)

// TODO: correct spec for enum
// +kubebuilder:validation:Enum=Required
type AntiAffinityMode string

const (
	AFMRequired  AntiAffinityMode = "Required"
	AFMPreferred AntiAffinityMode = "Preferred"
	AFMNone      AntiAffinityMode = "None"
)

// +kubebuilder:validation:Enum=New
type Phase string

const (
	PhaseNew Phase = "New"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.clusterSize,statuspath=.status.peers

// EtcdCluster is the Schema for the etcdclusters API
type EtcdCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdClusterSpec   `json:"spec,omitempty"`
	Status EtcdClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdClusterList contains a list of EtcdCluster
type EtcdClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdCluster{}, &EtcdClusterList{})
}
