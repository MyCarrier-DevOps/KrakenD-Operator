/*
Copyright 2026 The KrakenD Operator Authors.

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

// AutoConfigPhase is derived from the Synced condition. Fetching and Rendering are never
// written; they stay in the enum so previously stored values keep validating.
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
// +kubebuilder:validation:XValidation:rule="!has(self.openapi.configMapRef) || (has(self.urlTransform) && has(self.urlTransform.hostMapping) && size(self.urlTransform.hostMapping) > 0)",message="hostMapping is required when using configMapRef",fieldPath=".urlTransform.hostMapping"
// +kubebuilder:validation:XValidation:rule="self.trigger != 'Periodic' || (has(self.periodic) && (!self.periodic.interval.matches('^(0|(([0-9]+([.][0-9]*)?|[.][0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$') || duration(self.periodic.interval) >= duration('30s')))",message="periodic.interval of at least 30s is required when trigger is Periodic",fieldPath=".periodic.interval"
// +kubebuilder:validation:XValidation:rule="!(has(self.additionalEndpointsBasePath) && size(self.additionalEndpointsBasePath) > 0 && has(self.urlTransform) && has(self.urlTransform.addPathPrefix) && size(self.urlTransform.addPathPrefix) > 0)",message="additionalEndpointsBasePath is mutually exclusive with urlTransform.addPathPrefix; set only one",fieldPath=".additionalEndpointsBasePath"
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
	// +kubebuilder:validation:MaxItems=1024
	Overrides []OperationOverride `json:"overrides,omitempty"`

	// Filter restricts which OpenAPI operations are converted to endpoints.
	Filter *FilterSpec `json:"filter,omitempty"`

	// Trigger selects the reconciliation trigger mode.
	Trigger TriggerType `json:"trigger"`

	// Periodic configures the polling interval when trigger is "Periodic".
	Periodic *PeriodicSpec `json:"periodic,omitempty"`

	// AdditionalEndpoints injects endpoints that are not present in the OpenAPI
	// spec (e.g. health/liveness probes). They are synthesized into full
	// endpoints and rendered alongside the spec-derived ones.
	// +optional
	// +listType=map
	// +listMapKey=endpoint
	// +listMapKey=method
	// +kubebuilder:validation:MaxItems=256
	AdditionalEndpoints []AdditionalEndpoint `json:"additionalEndpoints,omitempty"`

	// AdditionalEndpointsBasePath overrides the auto-derived base path used to
	// scope AdditionalEndpoints under the application. When empty, the base is
	// derived from the generated endpoints' common parent directory. Must start
	// with "/".
	// +optional
	// +kubebuilder:validation:Pattern=`^/`
	AdditionalEndpointsBasePath string `json:"additionalEndpointsBasePath,omitempty"`
}

// OpenAPISource defines the location of an OpenAPI spec.
// +kubebuilder:validation:XValidation:rule="(has(self.url) && size(self.url) > 0) != has(self.configMapRef)",message="exactly one of url or configMapRef is required"
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
// +kubebuilder:validation:XValidation:rule="!(has(self.bearerTokenSecret) && has(self.basicAuthSecret))",message="bearerTokenSecret and basicAuthSecret are mutually exclusive"
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
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(0|(([0-9]+([.][0-9]*)?|[.][0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$') || duration(self) >= duration('0s')",message="must be a duration that fits in 64 bits of nanoseconds"
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// CacheTTL sets the default endpoint cache TTL.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(0|(([0-9]+([.][0-9]*)?|[.][0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$') || duration(self) >= duration('0s')",message="must be a duration that fits in 64 bits of nanoseconds"
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	CacheTTL *metav1.Duration `json:"cacheTTL,omitempty"`

	// OutputEncoding sets the default response encoding (e.g. "json", "no-op").
	// +kubebuilder:validation:Enum=json;json-collection;yaml;fast-json;xml;negotiate;string;no-op
	OutputEncoding string `json:"outputEncoding,omitempty"`

	// ConcurrentCalls sets the default number of concurrent backend calls.
	// When specified, it must be a positive integer.
	// +kubebuilder:validation:Minimum=1
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
	// +kubebuilder:validation:Enum=json;safejson;fast-json;xml;rss;string;no-op;yaml
	Encoding string `json:"encoding,omitempty"`

	// SD sets the default service discovery provider (e.g. "static", "dns").
	// +kubebuilder:validation:Enum=static;dns;dns-shared
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
	// +kubebuilder:validation:Pattern=`^(/\*|/[^*?&%]*(/\*)?)$`
	Endpoint string `json:"endpoint,omitempty"`

	// Method overrides the HTTP method.
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE
	Method string `json:"method,omitempty"`

	// Timeout overrides the endpoint timeout (a Go duration).
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(0|(([0-9]+([.][0-9]*)?|[.][0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$') || duration(self) >= duration('0s')",message="must be a duration that fits in 64 bits of nanoseconds"
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// CacheTTL overrides the endpoint cache TTL.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(0|(([0-9]+([.][0-9]*)?|[.][0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$') || duration(self) >= duration('0s')",message="must be a duration that fits in 64 bits of nanoseconds"
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	CacheTTL *metav1.Duration `json:"cacheTTL,omitempty"`

	// OutputEncoding overrides the response encoding (e.g. "no-op", "json").
	// +kubebuilder:validation:Enum=json;json-collection;yaml;fast-json;xml;negotiate;string;no-op
	OutputEncoding string `json:"outputEncoding,omitempty"`

	// ConcurrentCalls overrides the number of concurrent backend calls.
	// When specified, it must be a positive integer.
	// +kubebuilder:validation:Minimum=1
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
	// +kubebuilder:validation:Minimum=0
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

// AdditionalEndpoint declares an endpoint that is not present in the OpenAPI
// document. Only Endpoint is required; everything else is optional and, when
// omitted, is synthesized or (when InheritDefaults is true) taken from
// spec.defaults.
// +kubebuilder:validation:XValidation:rule="!(has(self.backends) && size(self.backends) > 0 && ((has(self.host) && size(self.host) > 0) || (has(self.backendUrlPattern) && size(self.backendUrlPattern) > 0) || (has(self.encoding) && size(self.encoding) > 0)))",message="backends and the host/backendUrlPattern/encoding shorthand are mutually exclusive"
type AdditionalEndpoint struct {
	// Endpoint is the public path KrakenD exposes (e.g. "/liveness").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(/\*|/[^*?&%]*(/\*)?)$`
	Endpoint string `json:"endpoint"`

	// Method is the HTTP method. Defaults to GET.
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE
	// +kubebuilder:default=GET
	// +optional
	Method string `json:"method,omitempty"`

	// Host is the backend host URL for the synthesized single backend.
	// Defaults to the host derived from spec.openapi.url. Ignored when Backends is set.
	// +optional
	Host string `json:"host,omitempty"`

	// BackendURLPattern is the upstream path for the synthesized single backend.
	// Defaults to Endpoint. Ignored when Backends is set.
	// +optional
	BackendURLPattern string `json:"backendUrlPattern,omitempty"`

	// Encoding sets the synthesized backend's encoding. "no-op" also sets the
	// endpoint output encoding to no-op unless OutputEncoding is set. Ignored when Backends is set.
	// +optional
	// +kubebuilder:validation:Enum=json;safejson;fast-json;xml;rss;string;no-op;yaml
	Encoding string `json:"encoding,omitempty"`

	// Backends, when set, is used verbatim; Host/BackendURLPattern/Encoding are ignored.
	// +optional
	Backends []BackendSpec `json:"backends,omitempty"`

	// Timeout overrides the endpoint timeout.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(0|(([0-9]+([.][0-9]*)?|[.][0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$') || duration(self) >= duration('0s')",message="must be a duration that fits in 64 bits of nanoseconds"
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// CacheTTL overrides the endpoint cache TTL.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!self.matches('^(0|(([0-9]+([.][0-9]*)?|[.][0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$') || duration(self) >= duration('0s')",message="must be a duration that fits in 64 bits of nanoseconds"
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	CacheTTL *metav1.Duration `json:"cacheTTL,omitempty"`
	// InputHeaders is the list of headers forwarded to backends.
	// +optional
	InputHeaders []string `json:"inputHeaders,omitempty"`
	// InputQueryStrings is the list of query parameters forwarded to backends.
	// +optional
	InputQueryStrings []string `json:"inputQueryStrings,omitempty"`
	// OutputEncoding overrides the endpoint response encoding.
	// +optional
	// +kubebuilder:validation:Enum=json;json-collection;yaml;fast-json;xml;negotiate;string;no-op
	OutputEncoding string `json:"outputEncoding,omitempty"`
	// ConcurrentCalls sets the number of concurrent backend calls.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ConcurrentCalls *int32 `json:"concurrentCalls,omitempty"`
	// ExtraConfig holds endpoint-level extra_config JSON.
	// +optional
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`

	// InheritDefaults applies spec.defaults (fill-only; explicit fields win) to
	// this endpoint. Defaults to false so a health route does not inherit a
	// default JWT validator.
	// +optional
	InheritDefaults *bool `json:"inheritDefaults,omitempty"`
}

// PeriodicSpec configures the polling interval for periodic triggers.
type PeriodicSpec struct {
	// Interval is the polling interval; at least 30s.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	Interval metav1.Duration `json:"interval"`
}

// OperationStatus reports one OpenAPI operation the AutoConfig skipped or
// could not converge.
type OperationStatus struct {
	// Method is the operation's HTTP method, upper case.
	Method string `json:"method"`
	// Path is the gateway path the operation's endpoint has, or would have.
	Path string `json:"path"`
	// OperationID is the operation's operationId, when it declares one.
	// +optional
	OperationID string `json:"operationId,omitempty"`
	// Endpoint is the KrakenDEndpoint generated for the operation, when the
	// problem concerns that object.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// Reason is a CamelCase code for why the operation is listed.
	Reason string `json:"reason"`
	// Message explains Reason, truncated to 256 bytes.
	// +optional
	Message string `json:"message,omitempty"`
}

// KrakenDAutoConfigStatus defines the observed state of KrakenDAutoConfig.
type KrakenDAutoConfigStatus struct {
	// Phase is derived from the Synced condition and kept for compatibility;
	// read the Ready condition instead.
	Phase AutoConfigPhase `json:"phase,omitempty"`
	// ObservedGeneration is the metadata.generation this status was computed for.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastSyncTime is when a sync last changed something: new inputs (a
	// different OpenAPI spec, CUE definitions or spec generation) or an
	// endpoint create, update or delete. A resync that changes nothing leaves
	// it alone, so an old value does not mean the AutoConfig is stale; use
	// the Ready condition or the krakend_operator_autoconfig_synced metric
	// for freshness.
	LastSyncTime       *metav1.Time `json:"lastSyncTime,omitempty"`
	SpecChecksum       string       `json:"specChecksum,omitempty"`
	GeneratedEndpoints int          `json:"generatedEndpoints,omitempty"`
	// ReadyEndpoints counts the KrakenDEndpoints this AutoConfig controls
	// whose Ready condition is True for their current generation.
	// +optional
	ReadyEndpoints int `json:"readyEndpoints,omitempty"`
	// SkippedOperations counts the operations the last sync generated no
	// endpoint for by rule (see skipped), including any beyond the 20 listed.
	SkippedOperations int `json:"skippedOperations,omitempty"`
	// Skipped lists up to 20 operations the last sync generated no endpoint
	// for by rule: an HTTP method KrakenDEndpoint does not accept
	// (UnsupportedMethod), or a duplicate of an earlier operation
	// (DuplicateOperationId).
	// +optional
	// +listType=atomic
	Skipped []OperationStatus `json:"skipped,omitempty"`
	// FailedOperations lists up to 20 operations the last sync could not
	// converge: they failed CUE evaluation (CUEEvaluationFailed), the gateway
	// config check (ConfigValidationFailed), or the API server rejected their
	// endpoint (EndpointRejected). Each keeps the endpoint it had, if any,
	// and while any is listed no stale endpoint is deleted. Synced is False
	// while this list is not empty; its reason is OperationsFailed unless a
	// later sync failed before its endpoint writes, which leaves this list
	// as the last sync that reached them recorded it.
	// +optional
	// +listType=atomic
	FailedOperations []OperationStatus `json:"failedOperations,omitempty"`
	// Warnings lists up to 20 problems in the OpenAPI spec or the AutoConfig
	// that do not stop a sync, such as unresolved or colliding schema
	// references, which leave the published documentation wrong.
	// +optional
	// +listType=atomic
	Warnings []string `json:"warnings,omitempty"`
	// Conditions are keyed by type. Ready is the summary condition.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kac
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Generated",type=integer,JSONPath=`.status.generatedEndpoints`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KrakenDAutoConfig is the Schema for the krakendautoconfigs API.
// +operator-sdk:csv:customresourcedefinitions:displayName="KrakenD AutoConfig"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters: it is a label value on generated endpoints"
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
