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
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// ClusterSize is the number of peers, including any preconfigured imported
	// peers that will form the etcd cluster
	ClusterSize int `json:"clusterSize,omitempty"`
}

// EtcdClusterStatus defines the observed state of EtcdCluster
type EtcdClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// HealthyManagedPeers represents the number of etcd cluster members that have
	// been created by the controller and are in a healthy state
	HealthyManagedPeers int `json:"healthyManagedPeers"`

	// TODO: type def here
	HealthyImportedPeers int `json:"healthyImportedPeers"`

	// TODO: type def here
	ImportedPeers int `json:"importedPeers"`

	// TODO: type def here
	ManagedPeers int `json:"managedPeers"`

	// Phase
	// +kubebuilder:default:=New
	Phase Phase `json:"phase"`

	// Peers are the total number of EtcdPeers targeted by this cluster
	Peers int `json:"peers,omitempty"`
}

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
