/*
Copyright 2026 gojnimer-labs.

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
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// WorkloadSpec defines the desired state of Workload
type WorkloadSpec struct {
	// templateName selects a catalog template (e.g. "nginx", "firefox",
	// "chrome") to render this workload's containers/volumes/service ports
	// from. When set, image/containerPort/env below are ignored in favor of
	// the template's own definition, and config supplies the template's
	// parameter values. When empty, image/containerPort/env are used
	// directly (the original raw-image deploy path).
	// +optional
	TemplateName string `json:"templateName,omitempty"`

	// image is the container image to run when templateName is unset.
	// Ignored when templateName is set.
	// +optional
	Image string `json:"image,omitempty"`

	// replicas is the desired replica count.
	// +optional
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// containerPort is the port the container listens on.
	// +optional
	// +kubebuilder:default=8080
	ContainerPort int32 `json:"containerPort,omitempty"`

	// env is a passthrough list of plain (non-secret) environment variables.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// subdomain is the identifying routing key for this workload. Unused for
	// actual routing in this POC (HTTPRoute/ingress exposure is deferred) but
	// kept on spec now so wiring it up later doesn't require a breaking CRD change.
	// +optional
	Subdomain string `json:"subdomain,omitempty"`

	// userID identifies the owning user for future multi-tenant reconciliation.
	// Stored as an opaque string for this POC.
	// +optional
	UserID string `json:"userId,omitempty"`

	// config is a deliberately loose passthrough bag reserved for future
	// template-specific values (volumes, HTTPRoute rules, middleware refs,
	// etc.) without requiring a CRD schema bump.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// suspended pauses this workload without destroying it: the reconciler
	// scales the backing Deployment to 0 replicas (keeping the Service in
	// place, just unrouted) while suspended is true, and scales it back to
	// the normal replicas count when flipped back to false. Config, labels,
	// and everything else about the CR stay exactly as they were.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
}

// WorkloadStatus defines the observed state of Workload.
type WorkloadStatus struct {
	// phase is a coarse human-readable status.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Deploying;Running;Failed;Stopped
	Phase string `json:"phase,omitempty"`

	// readyReplicas mirrors the backing Deployment's status.readyReplicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// observedGeneration lets clients tell if status is stale relative to spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the Workload resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Ready": the backing Deployment has its desired ready replicas
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	// - "ConvexSynced": Convex's ownership records reflect this workload's
	//   current generation (best-effort; retried until it succeeds)
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workload is the Schema for the workloads API
type Workload struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Workload
	// +required
	Spec WorkloadSpec `json:"spec"`

	// status defines the observed state of Workload
	// +optional
	Status WorkloadStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkloadList contains a list of Workload
type WorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Workload `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Workload{}, &WorkloadList{})
		return nil
	})
}
