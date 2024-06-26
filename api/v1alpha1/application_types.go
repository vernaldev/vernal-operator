/*
Copyright 2024.

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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type ApplicationSpecComponent struct {
	Name          string `json:"name"`
	Image         string `json:"image"`
	ContainerPort uint16 `json:"containerPort"`
	MinReplicas   int32  `json:"minReplicas"`
	MaxReplicas   int32  `json:"maxReplicas"`
	// +optional
	EnvVars *[]v1.EnvVar `json:"env,omitempty"`
}

type ApplicationSpecRepo struct {
	Url string `json:"url"`

	// +kubebuilder:default:=main
	Revision string `json:"revision,omitempty"`

	// +kubebuilder:default:=vernal.yaml
	Path string `json:"path,omitempty"`
}

type ApplicationSpecPostgres struct {
	Enabled   bool   `json:"enabled"`
	UrlEnvVar string `json:"urlEnvVar"`
}

type ApplicationSpecRedis struct {
	Enabled   bool   `json:"enabled"`
	UrlEnvVar string `json:"urlEnvVar"`
}

// ApplicationSpec defines the desired state of Application
type ApplicationSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of Application. Edit application_types.go to remove/update
	Owner      string                     `json:"owner"`
	Repo       ApplicationSpecRepo        `json:"repo"`
	Components []ApplicationSpecComponent `json:"components"`
	Postgres   ApplicationSpecPostgres    `json:"postgres"`
	Redis      ApplicationSpecRedis       `json:"redis"`
}

// ApplicationStatus defines the observed state of Application
type ApplicationStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:subresource:status

// Application is the Schema for the applications API
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplicationSpec   `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:scope=Cluster

// ApplicationList contains a list of Application
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Application{}, &ApplicationList{})
}
