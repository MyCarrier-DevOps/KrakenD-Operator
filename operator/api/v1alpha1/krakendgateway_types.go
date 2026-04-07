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

// +kubebuilder:validation:Enum=CE;EE
type Edition string

const (
	EditionCE Edition = "CE"
	EditionEE Edition = "EE"
)

// +kubebuilder:validation:Enum=Pending;Rendering;Validating;Deploying;Running;Degraded;Error
type GatewayPhase string

const (
	PhasePending    GatewayPhase = "Pending"
	PhaseRendering  GatewayPhase = "Rendering"
	PhaseValidating GatewayPhase = "Validating"
	PhaseDeploying  GatewayPhase = "Deploying"
	PhaseRunning    GatewayPhase = "Running"
	PhaseDegraded   GatewayPhase = "Degraded"
	PhaseError      GatewayPhase = "Error"
)

// KrakenDGatewaySpec defines the desired state of KrakenDGateway.
type KrakenDGatewaySpec struct {
	// Version is the KrakenD version to deploy (e.g. "2.13").
	Version string `json:"version"`

	// Edition selects the KrakenD edition: "CE" (Community) or "EE" (Enterprise).
	Edition Edition `json:"edition"`

	// Image overrides the default KrakenD EE image.
	Image string `json:"image,omitempty"`

	// CEImage overrides the default KrakenD CE image used as fallback.
	CEImage string `json:"ceImage,omitempty"`

	// Replicas is the desired number of KrakenD pods when autoscaling is not configured.
	Replicas *int32 `json:"replicas,omitempty"`

	// Autoscaling configures the HorizontalPodAutoscaler for KrakenD pods.
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Config holds gateway-level KrakenD configuration.
	Config GatewayConfig `json:"config"`

	// TLS configures TLS termination at the KrakenD pod level.
	TLS *TLSSpec `json:"tls,omitempty"`

	// License configures the Enterprise license source.
	License *LicenseConfig `json:"license,omitempty"`

	// Dragonfly configures an in-cluster Dragonfly instance for caching.
	Dragonfly *DragonflySpec `json:"dragonfly,omitempty"`

	// Redis configures the connection pool to an external Redis/Dragonfly.
	Redis *RedisSpec `json:"redis,omitempty"`

	// Istio configures Istio VirtualService integration.
	Istio *IstioSpec `json:"istio,omitempty"`

	// Plugins configures KrakenD plugin sources.
	Plugins *PluginsSpec `json:"plugins,omitempty"`

	// Resources defines the compute resource requirements for KrakenD pods.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// GatewayConfig holds KrakenD service-level configuration.
type GatewayConfig struct {
	// Port is the KrakenD listen port (default 8080).
	Port int32 `json:"port,omitempty"`

	// Timeout is the global request timeout (e.g. "3s").
	Timeout string `json:"timeout,omitempty"`

	// CacheTTL is the global cache TTL (e.g. "0s").
	CacheTTL string `json:"cacheTTL,omitempty"`

	// OutputEncoding selects the default response encoding: json, negotiate, no-op.
	OutputEncoding string `json:"outputEncoding,omitempty"`

	// DNSCacheTTL is the DNS lookup cache duration (e.g. "30s").
	DNSCacheTTL string `json:"dnsCacheTTL,omitempty"`

	// CORS configures Cross-Origin Resource Sharing headers.
	CORS *CORSConfig `json:"cors,omitempty"`

	// Security configures HTTP security headers.
	Security *SecurityConfig `json:"security,omitempty"`

	// Logging configures KrakenD logging behavior.
	Logging *LoggingConfig `json:"logging,omitempty"`

	// Router configures KrakenD router behavior.
	Router *RouterConfig `json:"router,omitempty"`

	// Telemetry configures observability integrations.
	Telemetry *TelemetryConfig `json:"telemetry,omitempty"`

	// ExtraConfig holds arbitrary gateway-level extra_config JSON.
	ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}

// CORSConfig configures Cross-Origin Resource Sharing headers.
type CORSConfig struct {
	AllowOrigins []string `json:"allowOrigins,omitempty"`
	AllowMethods []string `json:"allowMethods,omitempty"`
	AllowHeaders []string `json:"allowHeaders,omitempty"`
	MaxAge       string   `json:"maxAge,omitempty"`
}

// SecurityConfig configures HTTP security headers.
type SecurityConfig struct {
	SSLRedirect           bool              `json:"sslRedirect,omitempty"`
	SSLProxyHeaders       map[string]string `json:"sslProxyHeaders,omitempty"`
	FrameOptions          string            `json:"frameOptions,omitempty"`
	ContentTypeNosniff    bool              `json:"contentTypeNosniff,omitempty"`
	BrowserXSSFilter      bool              `json:"browserXssFilter,omitempty"`
	HSTSSeconds           int64             `json:"hstsSeconds,omitempty"`
	ContentSecurityPolicy string            `json:"contentSecurityPolicy,omitempty"`
}

// LoggingConfig configures KrakenD logging.
type LoggingConfig struct {
	Level  string `json:"level,omitempty"`
	Format string `json:"format,omitempty"`
	Stdout bool   `json:"stdout,omitempty"`
}

// RouterConfig configures KrakenD router behavior.
type RouterConfig struct {
	ReturnErrorMsg   bool   `json:"returnErrorMsg,omitempty"`
	HealthPath       string `json:"healthPath,omitempty"`
	AutoOptions      bool   `json:"autoOptions,omitempty"`
	DisableAccessLog bool   `json:"disableAccessLog,omitempty"`
}

// TelemetryConfig configures observability integrations.
type TelemetryConfig struct {
	OpenTelemetry *OpenTelemetryConfig `json:"openTelemetry,omitempty"`
	Prometheus    *PrometheusConfig    `json:"prometheus,omitempty"`
}

// OpenTelemetryConfig configures OpenTelemetry export.
type OpenTelemetryConfig struct {
	Exporters   *OTelExporters `json:"exporters,omitempty"`
	ServiceName string         `json:"serviceName,omitempty"`
}

// OTelExporters configures OpenTelemetry exporters.
type OTelExporters struct {
	OTLP *OTLPExporter `json:"otlp,omitempty"`
}

// OTLPExporter configures the OTLP exporter.
type OTLPExporter struct {
	Host string `json:"host"`
	Port int32  `json:"port,omitempty"`
}

// PrometheusConfig configures the Prometheus metrics exporter.
type PrometheusConfig struct {
	Enabled bool  `json:"enabled,omitempty"`
	Port    int32 `json:"port,omitempty"`
}

// AutoscalingSpec configures the HorizontalPodAutoscaler.
type AutoscalingSpec struct {
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	MaxReplicas int32  `json:"maxReplicas"`
	TargetCPU   *int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

// TLSSpec configures TLS termination at the KrakenD pod.
type TLSSpec struct {
	Enabled    bool   `json:"enabled,omitempty"`
	PublicKey  string `json:"publicKey,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	MinVersion string `json:"minVersion,omitempty"`
}

// LicenseConfig configures the Enterprise license source.
type LicenseConfig struct {
	ExternalSecret    ExternalSecretLicenseConfig `json:"externalSecret,omitempty"`
	SecretRef         *corev1.SecretKeySelector   `json:"secretRef,omitempty"`
	FallbackToCE      bool                        `json:"fallbackToCE,omitempty"`
	ExpiryWarningDays int                         `json:"expiryWarningDays,omitempty"`
}

// ExternalSecretLicenseConfig configures an ExternalSecret for the license.
type ExternalSecretLicenseConfig struct {
	Enabled        bool              `json:"enabled,omitempty"`
	SecretStoreRef SecretStoreRef    `json:"secretStoreRef,omitempty"`
	RemoteRef      ExternalRemoteRef `json:"remoteRef,omitempty"`
}

// SecretStoreRef references a SecretStore or ClusterSecretStore.
type SecretStoreRef struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// ExternalRemoteRef references a key in an external secrets store.
type ExternalRemoteRef struct {
	Key      string `json:"key"`
	Property string `json:"property,omitempty"`
}

// DragonflySpec configures an in-cluster Dragonfly instance.
type DragonflySpec struct {
	Enabled        bool                         `json:"enabled"`
	Image          string                       `json:"image,omitempty"`
	Replicas       *int32                       `json:"replicas,omitempty"`
	Resources      *corev1.ResourceRequirements `json:"resources,omitempty"`
	Snapshot       *DragonflySnapshotSpec       `json:"snapshot,omitempty"`
	Args           []string                     `json:"args,omitempty"`
	Authentication *DragonflyAuthSpec           `json:"authentication,omitempty"`
}

// DragonflySnapshotSpec configures Dragonfly snapshotting.
type DragonflySnapshotSpec struct {
	Cron                      string                            `json:"cron,omitempty"`
	PersistentVolumeClaimSpec *corev1.PersistentVolumeClaimSpec `json:"persistentVolumeClaimSpec,omitempty"`
}

// DragonflyAuthSpec configures Dragonfly authentication.
type DragonflyAuthSpec struct {
	PasswordFromSecret *corev1.SecretKeySelector `json:"passwordFromSecret,omitempty"`
}

// RedisSpec configures the Redis/Dragonfly connection pool.
type RedisSpec struct {
	ConnectionPool RedisConnectionPool `json:"connectionPool"`
}

// RedisConnectionPool holds Redis connection pool parameters.
type RedisConnectionPool struct {
	Addresses    []string                  `json:"addresses"`
	Password     *corev1.SecretKeySelector `json:"password,omitempty"`
	PoolSize     int                       `json:"poolSize,omitempty"`
	MinIdleConns int                       `json:"minIdleConns,omitempty"`
	DialTimeout  string                    `json:"dialTimeout,omitempty"`
	ReadTimeout  string                    `json:"readTimeout,omitempty"`
	WriteTimeout string                    `json:"writeTimeout,omitempty"`
	TLS          *RedisTLSConfig           `json:"tls,omitempty"`
}

// RedisTLSConfig configures TLS for Redis connections.
type RedisTLSConfig struct {
	Enabled    bool   `json:"enabled,omitempty"`
	SecretName string `json:"secretName,omitempty"`
}

// IstioSpec configures Istio VirtualService integration.
type IstioSpec struct {
	Enabled  bool     `json:"enabled"`
	Hosts    []string `json:"hosts,omitempty"`
	Gateways []string `json:"gateways,omitempty"`
}

// PluginsSpec configures KrakenD plugin sources.
type PluginsSpec struct {
	Sources []PluginSource `json:"sources"`
}

// PluginSource defines a single plugin source (one of imageRef, configMapRef, or persistentVolumeClaimRef).
type PluginSource struct {
	ImageRef                 *OCIImageRef                              `json:"imageRef,omitempty"`
	ConfigMapRef             *ConfigMapKeyRef                          `json:"configMapRef,omitempty"`
	PersistentVolumeClaimRef *corev1.PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaimRef,omitempty"`
}

// OCIImageRef references a plugin container image.
type OCIImageRef struct {
	Image            string                        `json:"image"`
	PullPolicy       corev1.PullPolicy             `json:"pullPolicy,omitempty"`
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// KrakenDGatewayStatus defines the observed state of KrakenDGateway.
type KrakenDGatewayStatus struct {
	Phase              GatewayPhase       `json:"phase,omitempty"`
	ConfigChecksum     string             `json:"configChecksum,omitempty"`
	PluginChecksum     string             `json:"pluginChecksum,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	Replicas           int32              `json:"replicas,omitempty"`
	ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
	LicenseExpiry      *metav1.Time       `json:"licenseExpiry,omitempty"`
	ActiveImage        string             `json:"activeImage,omitempty"`
	EndpointCount      int32              `json:"endpointCount,omitempty"`
	DragonflyAddress   string             `json:"dragonflyAddress,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kgw
// +kubebuilder:printcolumn:name="Edition",type=string,JSONPath=`.spec.edition`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KrakenDGateway is the Schema for the krakendgateways API.
type KrakenDGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KrakenDGatewaySpec   `json:"spec"`
	Status KrakenDGatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KrakenDGatewayList contains a list of KrakenDGateway.
type KrakenDGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KrakenDGateway `json:"items"`
}

func init() { //nolint:gochecknoinits // required by controller-runtime scheme registration
	SchemeBuilder.Register(&KrakenDGateway{}, &KrakenDGatewayList{})
}
