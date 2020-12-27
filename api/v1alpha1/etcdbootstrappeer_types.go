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

// EtcdBootstrapPeerSpec defines the desired state of EtcdBootstrapPeer
type EtcdBootstrapPeerSpec struct {

	// Foo is an example field of EtcdBootstrapPeer. Edit EtcdBootstrapPeer_types.go to remove/update
	Foo string `json:"foo,omitempty"`
}

// EtcdBootstrapPeerStatus defines the observed state of EtcdBootstrapPeer
type EtcdBootstrapPeerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true

// EtcdBootstrapPeer is the Schema for the etcdbootstrappeers API
type EtcdBootstrapPeer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdBootstrapPeerSpec   `json:"spec,omitempty"`
	Status EtcdBootstrapPeerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdBootstrapPeerList contains a list of EtcdBootstrapPeer
type EtcdBootstrapPeerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdBootstrapPeer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdBootstrapPeer{}, &EtcdBootstrapPeerList{})
}
