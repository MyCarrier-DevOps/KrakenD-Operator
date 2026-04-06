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

	// Defaults sets default values for generated endpoints.
	Defaults *EndpointDefaults `json:"defaults,omitempty"`

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

// EndpointDefaults sets default values for generated endpoints.
type EndpointDefaults struct {
	Timeout           *metav1.Duration `json:"timeout,omitempty"`
	CacheTTL          *metav1.Duration `json:"cacheTTL,omitempty"`
	OutputEncoding    string           `json:"outputEncoding,omitempty"`
	ConcurrentCalls   *int32           `json:"concurrentCalls,omitempty"`
	InputHeaders      []string         `json:"inputHeaders,omitempty"`
	InputQueryStrings []string         `json:"inputQueryStrings,omitempty"`
	PolicyRef         *PolicyRef       `json:"policyRef,omitempty"`
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

func init() {
	SchemeBuilder.Register(&KrakenDAutoConfig{}, &KrakenDAutoConfigList{})
}
