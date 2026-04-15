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
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE
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

	// ConcurrentCalls sets the number of concurrent backend calls for this endpoint.
	// When specified, it must be a positive integer.
	// +kubebuilder:validation:Minimum=1
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

	// SD selects the service discovery provider (e.g. "static", "dns").
	SD string `json:"sd,omitempty"`

	// SDScheme sets the service discovery scheme (e.g. "http", "https").
	SDScheme string `json:"sdScheme,omitempty"`

	// DisableHostSanitize skips host protocol validation. Required for non-HTTP
	// protocols (amqp://, nats://, kafka://) or when using sd=dns.
	DisableHostSanitize *bool `json:"disableHostSanitize,omitempty"`

	// InputHeaders is the list of headers forwarded to this backend.
	InputHeaders []string `json:"inputHeaders,omitempty"`

	// InputQueryStrings is the list of query parameters forwarded to this backend.
	InputQueryStrings []string `json:"inputQueryStrings,omitempty"`

	// Allow is the allowlist of response fields to keep.
	Allow []string `json:"allow,omitempty"`

	// Deny is the denylist of response fields to remove.
	Deny []string `json:"deny,omitempty"`

	// Group wraps the backend response under a named key to avoid collisions.
	Group string `json:"group,omitempty"`

	// Target extracts a nested object and returns only its contents.
	Target string `json:"target,omitempty"`

	// IsCollection marks that the backend returns an array instead of an object.
	IsCollection *bool `json:"isCollection,omitempty"`

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
	Methods            string             `json:"methods,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoints",type=integer,JSONPath=`.status.endpointCount`
// +kubebuilder:printcolumn:name="Methods",type=string,JSONPath=`.status.methods`,priority=0
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
