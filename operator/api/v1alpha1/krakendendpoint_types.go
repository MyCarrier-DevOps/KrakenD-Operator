/*
Copyright 2026.

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
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:validation:Enum=Pending;Active;Invalid;Conflicted;Detached
type EndpointPhase string

const (
	EndpointPhasePending    EndpointPhase = "Pending"
	EndpointPhaseActive     EndpointPhase = "Active"
	EndpointPhaseInvalid    EndpointPhase = "Invalid"
	EndpointPhaseConflicted EndpointPhase = "Conflicted"
	EndpointPhaseDetached   EndpointPhase = "Detached"
)

// KrakenDEndpointSpec defines the desired state of KrakenDEndpoint.
type KrakenDEndpointSpec struct {
	// GatewayRef references the KrakenDGateway this endpoint belongs to.
	GatewayRef GatewayRef `json:"gatewayRef"`

	// Endpoints is the list of endpoint definitions.
	Endpoints []EndpointEntry `json:"endpoints"`
}

// EndpointEntry defines a single KrakenD endpoint.
type EndpointEntry struct {
	// Endpoint is the public path exposed by KrakenD (e.g. "/api/v1/users").
	Endpoint string `json:"endpoint"`

	// Method is the HTTP method for this endpoint.
	// +kubebuilder:validation:Enum=GET;POST;PUT;DELETE;PATCH;HEAD;OPTIONS;CONNECT;TRACE
	Method string `json:"method"`

	// Backends is the list of backend services for this endpoint.
	Backends []BackendSpec `json:"backends"`

	// Timeout overrides the global endpoint timeout.
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// CacheTTL overrides the global cache TTL.
	CacheTTL *metav1.Duration `json:"cacheTTL,omitempty"`

	// InputHeaders is the list of headers forwarded to backends.
	InputHeaders []string `json:"inputHeaders,omitempty"`

	// InputQueryStrings is the list of query string parameters forwarded to backends.
	InputQueryStrings []string `json:"inputQueryStrings,omitempty"`

	// OutputEncoding overrides the default response encoding.
	OutputEncoding string `json:"outputEncoding,omitempty"`

	// ConcurrentCalls sets concurrent backend calls for this endpoint.
	ConcurrentCalls *int32 `json:"concurrentCalls,omitempty"`

	// ExtraConfig holds arbitrary endpoint-level extra_config JSON.
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}

// BackendSpec defines a backend service target.
type BackendSpec struct {
	// Host is the list of backend host URLs.
	Host []string `json:"host"`

	// URLPattern is the backend URL path pattern.
	URLPattern string `json:"urlPattern"`

	// Method overrides the endpoint method for this backend.
	Method string `json:"method,omitempty"`

	// Encoding selects the backend response encoding.
	Encoding string `json:"encoding,omitempty"`

	// Allow is the allowlist of response fields to keep.
	Allow []string `json:"allow,omitempty"`

	// Mapping renames response fields.
	Mapping map[string]string `json:"mapping,omitempty"`

	// PolicyRef references a KrakenDBackendPolicy to apply.
	PolicyRef *PolicyRef `json:"policyRef,omitempty"`

	// ExtraConfig holds arbitrary backend-level extra_config JSON.
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}

// KrakenDEndpointStatus defines the observed state of KrakenDEndpoint.
type KrakenDEndpointStatus struct {
	Phase              EndpointPhase      `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	EndpointCount      int32              `json:"endpointCount,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoints",type=integer,JSONPath=`.status.endpointCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KrakenDEndpoint is the Schema for the krakendendpoints API.
type KrakenDEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KrakenDEndpointSpec   `json:"spec"`
	Status KrakenDEndpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KrakenDEndpointList contains a list of KrakenDEndpoint.
type KrakenDEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KrakenDEndpoint `json:"items"`
}
