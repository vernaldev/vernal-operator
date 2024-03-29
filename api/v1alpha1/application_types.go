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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CNPG StorageConfiguration struct
type StorageConfiguration struct {
	// StorageClass to use for PVCs. Applied after
	// evaluating the PVC template, if available.
	// If not specified, the generated PVCs will use the
	// default storage class
	// +optional
	StorageClass *string `json:"storageClass,omitempty"`

	// Size of the storage. Required if not already specified in the PVC template.
	// Changes to this field are automatically reapplied to the created PVCs.
	// Size cannot be decreased.
	// +optional
	Size string `json:"size,omitempty"`
}

// CPNG BootstrapInitDB struct
type BootstrapInitDB struct {
	// Name of the database used by the application. Default: `app`.
	// +optional
	Database string `json:"database,omitempty"`

	// Name of the owner of the database in the instance to be used
	// by applications. Defaults to the value of the `database` key.
	// +optional
	Owner string `json:"owner,omitempty"`
}

// CPNG bootstrapconfig struct
type BootstrapConfiguration struct {
	// Bootstrap the cluster via initdb
	// +optional
	InitDB *BootstrapInitDB `json:"initdb,omitempty"`
}

type PostgreSQLSpec struct {
	Database       string `json:"database"`
	User           string `json:"user"`
	Password       string `json:"password"`
	Version        string `json:"version,omitempty"`
	Replicas       *int32 `json:"replicas,omitempty"`
	PoolerReplicas *int32 `json:"PoolerReplicas,omitempty"`
	Instances      int    `json:"instances"`
	Enabled        bool   `json:"enabled"`
	Url            string `json:"url"`

	// Instructions to bootstrap this cluster
	// +optional
	Bootstrap *BootstrapConfiguration `json:"bootstrap,omitempty"`

	// Configuration of the storage of the instances
	// +optional
	StorageConfiguration StorageConfiguration `json:"storage,omitempty"`
}

type ApplicationSpecComponent struct {
	Name          string `json:"name"`
	Image         string `json:"image"`
	ContainerPort uint16 `json:"containerPort"`
}

type ApplicationSpecRepo struct {
	Url string `json:"url"`

	// +kubebuilder:default:=main
	Revision string `json:"revision,omitempty"`

	// +kubebuilder:default:=vernal.yaml
	Path string `json:"path,omitempty"`
}

// ApplicationSpec defines the desired state of Application
type ApplicationSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of Application. Edit application_types.go to remove/update
	Owner      string                     `json:"owner"`
	Repo       ApplicationSpecRepo        `json:"repo"`
	Components []ApplicationSpecComponent `json:"components"`
	PostgreSQL *PostgreSQLSpec            `json:"postgresql,omitempty"`
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
