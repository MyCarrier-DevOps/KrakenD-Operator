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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:validation:Enum=json;yaml
type SpecFormat string

const (
	SpecFormatJSON SpecFormat = "json"
	SpecFormatYAML SpecFormat = "yaml"
)

// +kubebuilder:validation:Enum=OnChange;Periodic
type TriggerType string

const (
	TriggerOnChange TriggerType = "OnChange"
	TriggerPeriodic TriggerType = "Periodic"
)

// +kubebuilder:validation:Enum=Pending;Fetching;Rendering;Synced;Error
type AutoConfigPhase string

const (
	AutoConfigPhasePending   AutoConfigPhase = "Pending"
	AutoConfigPhaseFetching  AutoConfigPhase = "Fetching"
	AutoConfigPhaseRendering AutoConfigPhase = "Rendering"
	AutoConfigPhaseSynced    AutoConfigPhase = "Synced"
	AutoConfigPhaseError     AutoConfigPhase = "Error"
)

// KrakenDAutoConfigSpec defines the desired state of KrakenDAutoConfig.
type KrakenDAutoConfigSpec struct {
	// GatewayRef references the KrakenDGateway that generated endpoints belong to.
	GatewayRef GatewayRef `json:"gatewayRef"`

	// OpenAPI defines the OpenAPI spec source.
	OpenAPI OpenAPISource `json:"openapi"`

	// CUE configures optional custom CUE definitions for endpoint generation.
	CUE *CUESpec `json:"cue,omitempty"`

	// URLTransform configures host mapping, path stripping, and path prefixing.
	URLTransform *URLTransformSpec `json:"urlTransform,omitempty"`

	// Defaults sets default values for generated endpoints and backends.
	Defaults *Defaults `json:"defaults,omitempty"`

	// Overrides applies per-operation overrides to generated endpoints.
	Overrides []OperationOverride `json:"overrides,omitempty"`

	// Filter restricts which OpenAPI operations are converted to endpoints.
	Filter *FilterSpec `json:"filter,omitempty"`

	// Trigger selects the reconciliation trigger mode.
	Trigger TriggerType `json:"trigger"`

	// Periodic configures the polling interval when trigger is "Periodic".
	Periodic *PeriodicSpec `json:"periodic,omitempty"`
}

// OpenAPISource defines the location of an OpenAPI spec.
type OpenAPISource struct {
	// URL is the HTTP(S) URL to fetch the OpenAPI spec from.
	URL string `json:"url,omitempty"`

	// ConfigMapRef references a ConfigMap key containing the OpenAPI spec.
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`

	// Auth configures authentication for HTTP fetching.
	Auth *AuthConfig `json:"auth,omitempty"`

	// AllowClusterLocal permits fetching from cluster-local addresses.
	AllowClusterLocal bool `json:"allowClusterLocal,omitempty"`

	// Format is the spec format: json or yaml. Auto-detected if omitted.
	Format SpecFormat `json:"format,omitempty"`
}

// AuthConfig configures authentication for OpenAPI spec fetching.
type AuthConfig struct {
	// BearerTokenSecret references a Secret key containing a bearer token.
	BearerTokenSecret *corev1.SecretKeySelector `json:"bearerTokenSecret,omitempty"`

	// BasicAuthSecret references a Secret containing basic auth credentials.
	BasicAuthSecret *BasicAuthSecretRef `json:"basicAuthSecret,omitempty"`
}

// BasicAuthSecretRef references a Secret containing username/password keys.
type BasicAuthSecretRef struct {
	Name        string `json:"name"`
	UsernameKey string `json:"usernameKey,omitempty"`
	PasswordKey string `json:"passwordKey,omitempty"`
}

// CUESpec configures CUE evaluation for endpoint generation.
type CUESpec struct {
	// DefinitionsConfigMapRef references a ConfigMap containing custom CUE definitions.
	// When omitted, only the operator's default CUE definitions are used.
	// When provided, custom definitions are unified with defaults.
	DefinitionsConfigMapRef *ConfigMapKeyRef `json:"definitionsConfigMapRef,omitempty"`

	// Environment is injected into CUE evaluation via FillPath("_env", ...).
	// Controls per-environment host resolution and other env-specific CUE branches.
	Environment string `json:"environment,omitempty"`
}

// URLTransformSpec configures URL transformations for generated endpoints.
type URLTransformSpec struct {
	HostMapping     []HostMappingEntry `json:"hostMapping,omitempty"`
	StripPathPrefix string             `json:"stripPathPrefix,omitempty"`
	AddPathPrefix   string             `json:"addPathPrefix,omitempty"`
}

// HostMappingEntry maps an OpenAPI server URL to a backend host.
type HostMappingEntry struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Defaults groups endpoint-level and backend-level default values.
type Defaults struct {
	// Endpoint sets default values applied to all generated endpoints.
	// Valid fields correspond to KrakenD v2.13 endpoint schema properties.
	Endpoint *EndpointDefaults `json:"endpoint,omitempty"`

	// Backend sets default values applied to all backends within generated endpoints.
	// Valid fields correspond to KrakenD v2.13 backend schema properties.
	Backend *BackendDefaults `json:"backend,omitempty"`

	// PolicyRef sets the default KrakenDBackendPolicy applied to all backends.
	PolicyRef *PolicyRef `json:"policyRef,omitempty"`
}

// EndpointDefaults sets default values for generated endpoints.
type EndpointDefaults struct {
	// Timeout sets the default endpoint timeout.
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// CacheTTL sets the default endpoint cache TTL.
	CacheTTL *metav1.Duration `json:"cacheTTL,omitempty"`

	// OutputEncoding sets the default response encoding (e.g. "json", "no-op").
	OutputEncoding string `json:"outputEncoding,omitempty"`

	// ConcurrentCalls sets the default number of concurrent backend calls.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	ConcurrentCalls *int32 `json:"concurrentCalls,omitempty"`

	// InputHeaders sets the default list of headers forwarded to backends.
	InputHeaders []string `json:"inputHeaders,omitempty"`

	// InputQueryStrings sets the default list of query parameters forwarded.
	InputQueryStrings []string `json:"inputQueryStrings,omitempty"`

	// ExtraConfig holds arbitrary endpoint-level extra_config JSON.
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}

// BackendDefaults sets default values applied to all backends within
// generated endpoints. Fields correspond to KrakenD v2.13 backend schema.
// Only non-per-backend-specific fields are included; Host, URLPattern,
// Method, Allow, and Mapping are per-backend and set via overrides.
type BackendDefaults struct {
	// Encoding sets the default backend response encoding (e.g. "json", "safejson", "no-op").
	Encoding string `json:"encoding,omitempty"`

	// SD sets the default service discovery provider (e.g. "static", "dns").
	SD string `json:"sd,omitempty"`

	// SDScheme sets the default service discovery scheme (e.g. "http", "https").
	SDScheme string `json:"sdScheme,omitempty"`

	// DisableHostSanitize skips host protocol validation for all backends.
	DisableHostSanitize *bool `json:"disableHostSanitize,omitempty"`

	// InputHeaders sets the default list of headers forwarded to all backends.
	InputHeaders []string `json:"inputHeaders,omitempty"`

	// InputQueryStrings sets the default list of query parameters forwarded to all backends.
	InputQueryStrings []string `json:"inputQueryStrings,omitempty"`

	// ExtraConfig holds arbitrary backend-level extra_config JSON
	// (e.g. backend/http, qos/circuit-breaker, qos/ratelimit/proxy).
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}

// OperationOverride applies per-operation overrides to generated endpoints.
type OperationOverride struct {
	// OperationID identifies the OpenAPI operation to override.
	OperationID string `json:"operationId"`

	// Endpoint overrides the generated endpoint path.
	Endpoint string `json:"endpoint,omitempty"`

	// Method overrides the HTTP method.
	Method string `json:"method,omitempty"`

	// Timeout overrides the endpoint timeout.
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// CacheTTL overrides the endpoint cache TTL.
	CacheTTL *metav1.Duration `json:"cacheTTL,omitempty"`

	// OutputEncoding overrides the response encoding (e.g. "no-op", "json").
	OutputEncoding string `json:"outputEncoding,omitempty"`

	// ConcurrentCalls overrides the number of concurrent backend calls.
	ConcurrentCalls *int32 `json:"concurrentCalls,omitempty"`

	// InputHeaders overrides the list of headers forwarded to backends.
	InputHeaders []string `json:"inputHeaders,omitempty"`

	// InputQueryStrings overrides the list of query parameters forwarded to backends.
	InputQueryStrings []string `json:"inputQueryStrings,omitempty"`

	// PolicyRef overrides the backend policy reference.
	PolicyRef *PolicyRef `json:"policyRef,omitempty"`

	// ExtraConfig overrides the endpoint extra_config.
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`

	// Backends applies per-backend overrides by index.
	Backends []BackendOverride `json:"backends,omitempty"`
}

// BackendOverride applies extra_config to a specific backend by index.
type BackendOverride struct {
	// Index is the 0-based backend index.
	Index int `json:"index"`

	// ExtraConfig holds arbitrary backend-level extra_config JSON.
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}

// FilterSpec restricts which OpenAPI operations are converted to endpoints.
type FilterSpec struct {
	IncludePaths        []string `json:"includePaths,omitempty"`
	ExcludePaths        []string `json:"excludePaths,omitempty"`
	IncludeMethods      []string `json:"includeMethods,omitempty"`
	ExcludeOperationIds []string `json:"excludeOperationIds,omitempty"`
	IncludeTags         []string `json:"includeTags,omitempty"`
	ExcludeTags         []string `json:"excludeTags,omitempty"`
}

// PeriodicSpec configures the polling interval for periodic triggers.
type PeriodicSpec struct {
	Interval metav1.Duration `json:"interval"`
}

// KrakenDAutoConfigStatus defines the observed state of KrakenDAutoConfig.
type KrakenDAutoConfigStatus struct {
	Phase              AutoConfigPhase    `json:"phase,omitempty"`
	LastSyncTime       *metav1.Time       `json:"lastSyncTime,omitempty"`
	SpecChecksum       string             `json:"specChecksum,omitempty"`
	GeneratedEndpoints int                `json:"generatedEndpoints,omitempty"`
	SkippedOperations  int                `json:"skippedOperations,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kac
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Generated",type=integer,JSONPath=`.status.generatedEndpoints`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KrakenDAutoConfig is the Schema for the krakendautoconfigs API.
type KrakenDAutoConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KrakenDAutoConfigSpec   `json:"spec"`
	Status KrakenDAutoConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KrakenDAutoConfigList contains a list of KrakenDAutoConfig.
type KrakenDAutoConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KrakenDAutoConfig `json:"items"`
}
