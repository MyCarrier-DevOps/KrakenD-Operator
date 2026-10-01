# KrakenD Operator — Application Architecture

> **Version:** 0.1.0-draft
> **Date:** 2026-04-03
> **Status:** Proposal
> **Language:** Go 1.26+
> **Framework:** controller-runtime v0.19+, Kubebuilder v4

This document describes the Go application architecture for the KrakenD Operator. It specifies package structure, interfaces, type hierarchies, reconciliation logic, and testing strategy at the implementation level. Every system, controller, status condition, event, and webhook rule described in the [operator architecture](../operator-architecture.md) is mapped to concrete Go code.

> **Source Root:** All file paths in this document are relative to the `operator/` directory, which is the Go module root. The repository root contains `operator/`, `architecture/`, and `.github/` at the top level.

---

## Table of Contents

1. [Design Principles](#1-design-principles)
2. [Entrypoint](#2-entrypoint)
3. [API Types](#3-api-types)
4. [Controller Architecture](#4-controller-architecture)
5. [Gateway Controller](#5-gateway-controller)
6. [Endpoint Controller](#6-endpoint-controller)
7. [Policy Controller](#7-policy-controller)
8. [AutoConfig Controller](#8-autoconfig-controller)
9. [License Evaluation](#9-license-evaluation)
10. [Configuration Rendering Pipeline](#10-configuration-rendering-pipeline)
11. [Resource Builders](#11-resource-builders)
12. [Webhook Validation](#12-webhook-validation)
13. [AutoConfig Subsystem](#13-autoconfig-subsystem)
14. [Utility Packages](#14-utility-packages)
15. [Dependency Injection and Interfaces](#15-dependency-injection-and-interfaces)
16. [Error Handling Strategy](#16-error-handling-strategy)
17. [Metrics Implementation](#17-metrics-implementation)
18. [Testing Strategy](#18-testing-strategy)
19. [Build and Packaging](#19-build-and-packaging)

---

## 1. Design Principles

These principles govern all application code. They complement the Go coding standards in `.github/instructions/go.instructions.md`.

| Principle | Application |
|---|---|
| Accept interfaces, return concrete types | All external dependencies (Kubernetes client, HTTP client, command executor, clock) are injected as interfaces. Constructors return concrete structs. |
| Composition over inheritance | Controllers compose renderer, resource builders, and utility packages — no embedded controller base types. |
| Single responsibility | Each package owns one concern: `renderer` builds JSON, `resources` builds Kubernetes objects, `autoconfig` evaluates CUE definitions against OpenAPI specs to produce `KrakenDEndpointSpec` CRDs. |
| Deterministic output | The rendering pipeline produces byte-identical JSON for identical CRD state. Maps are serialized with sorted keys, slices are sorted by defined criteria. |
| Fail fast at boundaries | Webhook validation rejects invalid CRs before they reach etcd. Controllers validate preconditions at the top of `Reconcile()` before mutating cluster state. |
| No global state | No `init()` functions except for scheme registration (standard Kubebuilder convention). All other state is owned by structs wired in `main.go`. |
| Testable by default | Every function that performs I/O accepts an interface parameter. Integration tests use envtest; unit tests use fakes and mocks. |

---

## 2. Entrypoint

**File:** `cmd/main.go`

The entrypoint is responsible for wiring dependencies, registering controllers with the manager, and starting the controller-runtime manager. It contains no business logic.

```mermaid
flowchart TD
    A[main] --> B[Parse flags and load config]
    B --> C[Create controller-runtime Manager]
    C --> D[Register API scheme]
    D --> E[Create shared dependencies]
    E --> F[Register Gateway Controller]
    E --> G[Register Endpoint Controller]
    E --> H[Register Policy Controller]
    E --> I[Register AutoConfig Controller]
    E --> K[Register Webhooks]
    F & G & H & I & K --> L[manager.Start context]
```

### Manager Configuration

```go
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
    Scheme:                  scheme,
    Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
    HealthProbeBindAddress:  probeAddr,
    LeaderElection:          true,
    LeaderElectionID:        "krakend-operator-leader",
    LeaderElectionNamespace: leaderElectionNamespace,
})
```

### Shared Dependency Wiring

All controllers receive their dependencies via struct fields set in `main.go`. There is no service locator or dependency injection container.

```go
clock := utilclock.RealClock{}
recorder := mgr.GetEventRecorderFor("krakend-operator")
httpClient := buildSafeHTTPClient()

fetcher := autoconfig.NewHTTPFetcher(httpClient)
cueEval := autoconfig.NewCUEEvaluator()
filter := autoconfig.NewFilter()
generator := autoconfig.NewGenerator()

rend := renderer.New(renderer.Options{})
val := renderer.NewValidator(renderer.ValidatorOptions{
    Executor:   renderer.NewKrakenDExecutor("/usr/local/bin/krakend"),
    BinaryPath: "/usr/local/bin/krakend",
})

gatewayCtrl := &controller.GatewayReconciler{
    Client:    mgr.GetClient(),
    Scheme:    mgr.GetScheme(),
    Recorder:  recorder,
    Renderer:  rend,
    Validator: val,
    Clock:     clock,
}

autoconfigCtrl := &controller.KrakenDAutoConfigReconciler{
    Client:       mgr.GetClient(),
    Scheme:       mgr.GetScheme(),
    Recorder:     recorder,
    Fetcher:      fetcher,
    CUEEvaluator: cueEval,
    Filter:       filter,
    Generator:    generator,
    Clock:        clock,
}

policyCtrl := &controller.PolicyReconciler{
    Client:   mgr.GetClient(),
    Scheme:   mgr.GetScheme(),
    Recorder: recorder,
}

endpointCtrl := &controller.EndpointReconciler{
    Client:   mgr.GetClient(),
    Scheme:   mgr.GetScheme(),
    Recorder: recorder,
}

// Register all controllers
for _, ctrl := range []interface{ SetupWithManager(ctrl.Manager) error }{
    gatewayCtrl, endpointCtrl, policyCtrl, autoconfigCtrl,
} {
    if err := ctrl.SetupWithManager(mgr); err != nil {
        setupLog.Error(err, "unable to create controller")
        os.Exit(1)
    }
}
```

### Scheme Registration

```go
import (
    gatewayv1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
    dragonflyv1alpha1 "github.com/dragonflydb/dragonfly-operator/api/v1alpha1"
    esv1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
    istiov1 "istio.io/client-go/pkg/apis/networking/v1"
)

func init() {
    utilruntime.Must(gatewayv1alpha1.AddToScheme(scheme))
    utilruntime.Must(dragonflyv1alpha1.AddToScheme(scheme))
    utilruntime.Must(esv1.AddToScheme(scheme))
    utilruntime.Must(istiov1.AddToScheme(scheme))
}
```

> **Note on `init()`:** Scheme registration in `init()` is the standard Kubebuilder convention and is the only accepted use of `init()` in this project. It registers types with the runtime scheme before `main()` executes. All other initialization happens explicitly in `main()`.

---

## 3. API Types

**Package:** `api/v1alpha1/`
**API Group:** `gateway.krakend.io/v1alpha1`

### Type Files

| File | CRD | Root Type |
|---|---|---|
| `krakendgateway_types.go` | KrakenDGateway | `KrakenDGatewaySpec`, `KrakenDGatewayStatus` |
| `krakendendpoint_types.go` | KrakenDEndpoint | `KrakenDEndpointSpec`, `KrakenDEndpointStatus` |
| `krakendbackendpolicy_types.go` | KrakenDBackendPolicy | `KrakenDBackendPolicySpec`, `KrakenDBackendPolicyStatus` |
| `krakendautoconfig_types.go` | KrakenDAutoConfig | `KrakenDAutoConfigSpec`, `KrakenDAutoConfigStatus` |
| `groupversion_info.go` | — | `SchemeBuilder`, `GroupVersion` |
| `zz_generated.deepcopy.go` | — | Generated `DeepCopyObject()` implementations |

### KrakenDGateway Type Hierarchy

```go
type KrakenDGateway struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec              KrakenDGatewaySpec   `json:"spec"`
    Status            KrakenDGatewayStatus `json:"status,omitempty"`
}

type KrakenDGatewaySpec struct {
    Version     string         `json:"version"`
    Edition     Edition        `json:"edition"`               // "CE" or "EE"
    Image       string         `json:"image,omitempty"`        // EE image override
    CEImage     string         `json:"ceImage,omitempty"`      // CE fallback image override
    Replicas    *int32         `json:"replicas,omitempty"`
    Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
    Config      GatewayConfig  `json:"config"`
    TLS         *TLSSpec       `json:"tls,omitempty"`
    License     *LicenseConfig `json:"license,omitempty"`
    Dragonfly   *DragonflySpec `json:"dragonfly,omitempty"`
    Redis       *RedisSpec     `json:"redis,omitempty"`
    Istio       *IstioSpec     `json:"istio,omitempty"`
    Plugins     *PluginsSpec   `json:"plugins,omitempty"`
    Resources   *corev1.ResourceRequirements `json:"resources,omitempty"`
}
```

### Edition Type

```go
// +kubebuilder:validation:Enum=CE;EE
type Edition string

const (
    EditionCE Edition = "CE"
    EditionEE Edition = "EE"
)
```

### Nested Spec Types

Types referenced by `KrakenDGatewaySpec`:

```go
type GatewayConfig struct {
    Port           int32                 `json:"port,omitempty"`           // default 8080
    Timeout        string                `json:"timeout,omitempty"`        // e.g. "3s"
    CacheTTL       string                `json:"cacheTTL,omitempty"`
    OutputEncoding string                `json:"outputEncoding,omitempty"` // json, negotiate, no-op
    DNSCacheTTL    string                `json:"dnsCacheTTL,omitempty"`
    CORS           *CORSConfig           `json:"cors,omitempty"`
    Security       *SecurityConfig       `json:"security,omitempty"`
    Logging        *LoggingConfig        `json:"logging,omitempty"`
    Router         *RouterConfig         `json:"router,omitempty"`
    Telemetry      *TelemetryConfig      `json:"telemetry,omitempty"`
    ExtraConfig    *runtime.RawExtension `json:"extraConfig,omitempty"` // gateway-level extra_config
}

type RouterConfig struct {
    ReturnErrorMsg  bool   `json:"returnErrorMsg,omitempty"`
    HealthPath      string `json:"healthPath,omitempty"` // default "/health"
    AutoOptions     bool   `json:"autoOptions,omitempty"`
    DisableAccessLog bool  `json:"disableAccessLog,omitempty"`
}

// CORSConfig, SecurityConfig, LoggingConfig, TelemetryConfig follow the same pattern.
// Their fields map 1:1 to operator architecture §3.1. Only RouterConfig is shown in full
// here because it is referenced by resource builders (health probes use router.healthPath).

type AutoscalingSpec struct {
    MinReplicas *int32 `json:"minReplicas,omitempty"`
    MaxReplicas int32  `json:"maxReplicas"`
    TargetCPU   *int32 `json:"targetCPUUtilizationPercentage,omitempty"`
}

type TLSSpec struct {
    Enabled    bool   `json:"enabled,omitempty"`
    PublicKey  string `json:"publicKey,omitempty"`  // path to cert PEM
    PrivateKey string `json:"privateKey,omitempty"` // path to key PEM
    MinVersion string `json:"minVersion,omitempty"` // e.g. "TLS13"
}

type LicenseConfig struct {
    ExternalSecret    ExternalSecretLicenseConfig `json:"externalSecret,omitempty"`
    SecretRef         *corev1.SecretKeySelector   `json:"secretRef,omitempty"`
    FallbackToCE      bool                        `json:"fallbackToCE,omitempty"`
    ExpiryWarningDays int                         `json:"expiryWarningDays,omitempty"` // default 30
}

type ExternalSecretLicenseConfig struct {
    Enabled        bool              `json:"enabled,omitempty"`
    SecretStoreRef SecretStoreRef    `json:"secretStoreRef,omitempty"`
    RemoteRef      ExternalRemoteRef `json:"remoteRef,omitempty"`
}

type SecretStoreRef struct {
    Name string `json:"name"`
    Kind string `json:"kind,omitempty"` // SecretStore or ClusterSecretStore
}

type ExternalRemoteRef struct {
    Key      string `json:"key"`
    Property string `json:"property,omitempty"`
}

type DragonflySpec struct {
    Enabled        bool                                        `json:"enabled"`
    Image          string                                      `json:"image,omitempty"`
    Replicas       *int32                                      `json:"replicas,omitempty"`
    Resources      *corev1.ResourceRequirements                `json:"resources,omitempty"`
    Snapshot       *DragonflySnapshotSpec                      `json:"snapshot,omitempty"`
    Args           []string                                    `json:"args,omitempty"`
    Authentication *DragonflyAuthSpec                          `json:"authentication,omitempty"`
}

type DragonflySnapshotSpec struct {
    Cron                      string                                     `json:"cron,omitempty"`
    PersistentVolumeClaimSpec *corev1.PersistentVolumeClaimSpec          `json:"persistentVolumeClaimSpec,omitempty"`
}

type DragonflyAuthSpec struct {
    PasswordFromSecret *corev1.SecretKeySelector `json:"passwordFromSecret,omitempty"`
}

type RedisSpec struct {
    ConnectionPool RedisConnectionPool `json:"connectionPool"`
}

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

type RedisTLSConfig struct {
    Enabled    bool   `json:"enabled,omitempty"`
    SecretName string `json:"secretName,omitempty"`
}

type IstioSpec struct {
    Enabled     bool              `json:"enabled"`
    Hosts       []string          `json:"hosts,omitempty"`
    Gateways    []string          `json:"gateways,omitempty"`
    Annotations map[string]string `json:"annotations,omitempty"` // added to the generated VirtualService metadata
}

type PluginsSpec struct {
    Sources []PluginSource `json:"sources"`
}

type PluginSource struct {
    ImageRef                 *OCIImageRef                               `json:"imageRef,omitempty"`
    ConfigMapRef             *ConfigMapKeyRef                           `json:"configMapRef,omitempty"`
    PersistentVolumeClaimRef *corev1.PersistentVolumeClaimVolumeSource  `json:"persistentVolumeClaimRef,omitempty"`
}

type OCIImageRef struct {
    Image            string                          `json:"image"`
    PullPolicy       corev1.PullPolicy               `json:"pullPolicy,omitempty"`
    ImagePullSecrets []corev1.LocalObjectReference    `json:"imagePullSecrets,omitempty"`
}
```

Types referenced by `KrakenDBackendPolicySpec`:

```go
type CircuitBreakerSpec struct {
    Interval        int  `json:"interval"`
    Timeout         int  `json:"timeout"`
    MaxErrors       int  `json:"maxErrors"`
    LogStatusChange bool `json:"logStatusChange,omitempty"`
}

type RateLimitSpec struct {
    MaxRate  int `json:"maxRate"`
    Capacity int `json:"capacity,omitempty"`
}

type CacheSpec struct {
    Shared bool `json:"shared,omitempty"`
}
```

Types referenced by `KrakenDAutoConfigSpec`:

```go
type OpenAPISource struct {
    URL               string           `json:"url,omitempty"`
    ConfigMapRef      *ConfigMapKeyRef `json:"configMapRef,omitempty"`
    Auth              *AuthConfig      `json:"auth,omitempty"`
    AllowClusterLocal bool             `json:"allowClusterLocal,omitempty"`
    Format            SpecFormat       `json:"format,omitempty"` // json, yaml; auto-detected if omitted
}

// +kubebuilder:validation:Enum=json;yaml
type SpecFormat string

const (
    SpecFormatJSON SpecFormat = "json"
    SpecFormatYAML SpecFormat = "yaml"
)

type AuthConfig struct {
    BearerTokenSecret *corev1.SecretKeySelector `json:"bearerTokenSecret,omitempty"`
    BasicAuthSecret   *BasicAuthSecretRef       `json:"basicAuthSecret,omitempty"`
}

type BasicAuthSecretRef struct {
    Name        string `json:"name"`
    UsernameKey string `json:"usernameKey,omitempty"` // default "username"
    PasswordKey string `json:"passwordKey,omitempty"` // default "password"
}

type URLTransformSpec struct {
    HostMapping     []HostMappingEntry `json:"hostMapping,omitempty"`
    StripPathPrefix string             `json:"stripPathPrefix,omitempty"`
    AddPathPrefix   string             `json:"addPathPrefix,omitempty"`
}

type HostMappingEntry struct {
    From string `json:"from"`
    To   string `json:"to"`
}

type EndpointDefaults struct {
    Timeout           *metav1.Duration `json:"timeout,omitempty"`
    CacheTTL          *metav1.Duration `json:"cacheTTL,omitempty"`
    OutputEncoding    string           `json:"outputEncoding,omitempty"`
    ConcurrentCalls   *int32           `json:"concurrentCalls,omitempty"`
    InputHeaders      []string         `json:"inputHeaders,omitempty"`
    InputQueryStrings []string         `json:"inputQueryStrings,omitempty"`
    PolicyRef         *PolicyRef       `json:"policyRef,omitempty"`
}

type OperationOverride struct {
    OperationID string                `json:"operationId"`
    Endpoint    string                `json:"endpoint,omitempty"`
    Method      string                `json:"method,omitempty"`
    Timeout     *metav1.Duration      `json:"timeout,omitempty"`
    CacheTTL    *metav1.Duration      `json:"cacheTTL,omitempty"`
    PolicyRef   *PolicyRef            `json:"policyRef,omitempty"`
    ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
    Backends    []BackendOverride     `json:"backends,omitempty"`
}

type BackendOverride struct {
    Index       int                   `json:"index"` // 0-based backend index
    ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}

type FilterSpec struct {
    IncludePaths        []string `json:"includePaths,omitempty"`
    ExcludePaths        []string `json:"excludePaths,omitempty"`
    IncludeMethods      []string `json:"includeMethods,omitempty"`
    ExcludeOperationIds []string `json:"excludeOperationIds,omitempty"`
    IncludeTags         []string `json:"includeTags,omitempty"`
    ExcludeTags         []string `json:"excludeTags,omitempty"`
}

// +kubebuilder:validation:Enum=OnChange;Periodic
type TriggerType string

const (
    TriggerOnChange TriggerType = "OnChange"
    TriggerPeriodic TriggerType = "Periodic"
)

type PeriodicSpec struct {
    Interval metav1.Duration `json:"interval"`
}
```

### Status Types

Status types use Kubernetes `metav1.Condition` for all conditions described in the operator architecture §15:

```go
type KrakenDGatewayStatus struct {
    Phase              GatewayPhase       `json:"phase,omitempty"`
    ConfigChecksum     string             `json:"configChecksum,omitempty"`
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
    Replicas           int32              `json:"replicas,omitempty"`
    ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
    LicenseExpiry      *metav1.Time       `json:"licenseExpiry,omitempty"`
    ActiveImage        string             `json:"activeImage,omitempty"`
    EndpointCount      int32              `json:"endpointCount,omitempty"`
    DragonflyAddress   string             `json:"dragonflyAddress,omitempty"`
}

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

// +kubebuilder:validation:Enum=Pending;Active;Invalid;Conflicted;Detached
type EndpointPhase string

const (
    EndpointPhasePending    EndpointPhase = "Pending"
    EndpointPhaseActive     EndpointPhase = "Active"
    EndpointPhaseInvalid    EndpointPhase = "Invalid"
    EndpointPhaseConflicted EndpointPhase = "Conflicted"
    EndpointPhaseDetached   EndpointPhase = "Detached"
)

// +kubebuilder:validation:Enum=Pending;Fetching;Rendering;Synced;Error
type AutoConfigPhase string

// The phase is derived from the Synced condition and is empty before the
// first sync; the controller writes only Synced and Error. Pending, Fetching
// and Rendering remain in the enum for compatibility.
const (
    AutoConfigPhasePending   AutoConfigPhase = "Pending"
    AutoConfigPhaseFetching  AutoConfigPhase = "Fetching"
    AutoConfigPhaseRendering AutoConfigPhase = "Rendering"
    AutoConfigPhaseSynced    AutoConfigPhase = "Synced"
    AutoConfigPhaseError     AutoConfigPhase = "Error"
)
```

### Condition Types (Constants)

```go
const (
    ConditionConfigValid              = "ConfigValid"
    ConditionAvailable                = "Available"
    ConditionLicenseValid             = "LicenseValid"
    ConditionLicenseDegraded          = "LicenseDegraded"
    ConditionDragonflyReady           = "DragonflyReady"
    ConditionIstioConfigured          = "IstioConfigured"
    ConditionLicenseSecretUnavailable = "LicenseSecretUnavailable"
    ConditionLicenseExpired           = "LicenseExpired"
    ConditionProgressing              = "Progressing"
    ConditionSpecAvailable            = "SpecAvailable"
    ConditionSynced                   = "Synced"
)
```

### Event Reason Constants

This is the set of reasons the controllers actually emit — `internal/webhook`
validation errors are reported through the admission response, not events, so
they carry no reason constant.

```go
const (
    ReasonConfigDeployed                = "ConfigDeployed"
    ReasonConfigValidationFailed        = "ConfigValidationFailed"
    ReasonLicenseExpiringSoon           = "LicenseExpiringSoon"
    ReasonLicenseFallbackCE             = "LicenseFallbackCE"
    ReasonLicenseExpiredNoFallback      = "LicenseExpiredNoFallback"
    ReasonLicenseRestored               = "LicenseRestored"
    ReasonDragonflyNotReady             = "DragonflyNotReady"
    ReasonIstioVSCreated                = "IstioVirtualServiceCreated"
    ReasonEndpointConflict              = "EndpointConflict"
    ReasonPartiallyAccepted             = "PartiallyAccepted"
    ReasonLicenseSecretMissing          = "LicenseSecretMissing"
    ReasonSpecFetched                   = "SpecFetched"
    ReasonSpecFetchFailed               = "SpecFetchFailed"
    ReasonEndpointsGenerated            = "EndpointsGenerated"
    ReasonDuplicateOperationId          = "DuplicateOperationId"
    ReasonRolloutFailed                 = "RolloutFailed"
    ReasonCUEEvaluationFailed           = "CUEEvaluationFailed"
    ReasonCUEEvaluationWarning          = "CUEEvaluationWarning"
    ReasonAdditionalEndpointOverride    = "AdditionalEndpointOverride"
    ReasonAdditionalEndpointScopeFailed = "AdditionalEndpointScopeFailed"
    ReasonUnmatchedOverride             = "UnmatchedOverride"
    ReasonEndpointReconcileFailed       = "EndpointReconcileFailed"
    ReasonPostRestartJobAlreadyRun      = "PostRestartJobAlreadyRun"
    ReasonPostRestartJobCreated         = "PostRestartJobCreated"
    ReasonPostRestartJobAdopted         = "PostRestartJobAdopted"
    ReasonPostRestartJobROFSEnabled     = "ReadOnlyRootFilesystemEnabled"
    ReasonPostRestartJobROFSDisabled    = "ReadOnlyRootFilesystemDisabled"
    ReasonDragonflyRunAsRootUnacknowledged = "RunAsRootUnacknowledged"
    ReasonDragonflyRunAsRootAcknowledged   = "RunAsRootAcknowledged"
    ReasonDragonflyRunAsRootNoRequest      = "NoRunAsRootRequest"
)
```

`ReasonLicenseSecretSyncFailed`, `ReasonOperationFiltered`, and
`ReasonMissingOperationId` are declared in `shared_types.go` but never
emitted by any controller, so they are left out of this list.

### KrakenDEndpoint Type

```go
type KrakenDEndpointSpec struct {
    GatewayRef GatewayRef     `json:"gatewayRef"`
    Endpoints  []EndpointEntry `json:"endpoints"`
}

type EndpointEntry struct {
    Endpoint     string                  `json:"endpoint"`
    Method       string                  `json:"method"`
    Backends     []BackendSpec           `json:"backends"`
    Timeout      *metav1.Duration        `json:"timeout,omitempty"`
    CacheTTL     *metav1.Duration        `json:"cacheTTL,omitempty"`
    InputHeaders []string                `json:"inputHeaders,omitempty"`
    InputQueryStrings []string           `json:"inputQueryStrings,omitempty"`
    OutputEncoding    string             `json:"outputEncoding,omitempty"`
    ConcurrentCalls   *int32             `json:"concurrentCalls,omitempty"`
    ExtraConfig  *runtime.RawExtension   `json:"extraConfig,omitempty"`
}

type KrakenDEndpointStatus struct {
    Phase              EndpointPhase      `json:"phase,omitempty"`
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    EndpointCount      int32              `json:"endpointCount,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
```

### KrakenDBackendPolicy Type

```go
type KrakenDBackendPolicySpec struct {
    CircuitBreaker *CircuitBreakerSpec   `json:"circuitBreaker,omitempty"`
    RateLimit      *RateLimitSpec        `json:"rateLimit,omitempty"`
    Cache          *CacheSpec            `json:"cache,omitempty"`
    Raw            *runtime.RawExtension `json:"raw,omitempty"`
}

type KrakenDBackendPolicyStatus struct {
    ReferencedBy int                `json:"referencedBy,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}
```

### KrakenDAutoConfig Type

```go
type KrakenDAutoConfigSpec struct {
    GatewayRef          GatewayRef           `json:"gatewayRef"`
    OpenAPI             OpenAPISource        `json:"openapi"`
    CUE                 *CUESpec             `json:"cue,omitempty"`
    URLTransform        *URLTransformSpec    `json:"urlTransform,omitempty"`
    Defaults            *Defaults            `json:"defaults,omitempty"`
    Overrides           []OperationOverride  `json:"overrides,omitempty"`
    Filter              *FilterSpec          `json:"filter,omitempty"`
    Trigger             TriggerType          `json:"trigger"`
    Periodic            *PeriodicSpec        `json:"periodic,omitempty"`
    AdditionalEndpoints     []AdditionalEndpoint `json:"additionalEndpoints,omitempty"`
    AdditionalEndpointsBasePath string            `json:"additionalEndpointsBasePath,omitempty"` // must start with "/"
}

// AdditionalEndpoint declares an endpoint that is not present in the OpenAPI
// document (e.g. health/liveness probes). Only Endpoint is required.
type AdditionalEndpoint struct {
    // Required. Public path KrakenD exposes (must start with "/").
    Endpoint string `json:"endpoint"`

    // HTTP method. Defaults to GET.
    Method string `json:"method,omitempty"`

    // Shorthand: backend host URL. Defaults to host derived from spec.openapi.url.
    // Ignored when Backends is set.
    Host string `json:"host,omitempty"`

    // Shorthand: upstream path for the synthesized backend.
    // Defaults to Endpoint. Ignored when Backends is set.
    BackendURLPattern string `json:"backendUrlPattern,omitempty"`

    // Shorthand: synthesized backend encoding. "no-op" also sets the endpoint
    // output encoding to no-op unless OutputEncoding is explicitly set.
    // Ignored when Backends is set.
    Encoding string `json:"encoding,omitempty"`

    // Full backends; mutually exclusive with Host/BackendURLPattern/Encoding.
    Backends []BackendSpec `json:"backends,omitempty"`

    Timeout           *metav1.Duration      `json:"timeout,omitempty"`
    CacheTTL          *metav1.Duration      `json:"cacheTTL,omitempty"`
    InputHeaders      []string              `json:"inputHeaders,omitempty"`
    InputQueryStrings []string              `json:"inputQueryStrings,omitempty"`
    OutputEncoding    string                `json:"outputEncoding,omitempty"`
    ConcurrentCalls   *int32                `json:"concurrentCalls,omitempty"`
    ExtraConfig       *runtime.RawExtension `json:"extraConfig,omitempty"`

    // InheritDefaults applies spec.defaults fill-only (explicit fields win).
    // Defaults to false so health routes do not inherit e.g. a JWT validator.
    InheritDefaults *bool `json:"inheritDefaults,omitempty"`
}

type CUESpec struct {
    // Optional: custom CUE definitions ConfigMap. When omitted, only the operator's
    // default CUE definitions are used. When provided, custom definitions are unified
    // with defaults (CUE unification, not replacement).
    DefinitionsConfigMapRef *ConfigMapKeyRef `json:"definitionsConfigMapRef,omitempty"`
    // Environment value injected into CUE evaluation via FillPath("_env", ...).
    // Controls per-environment host resolution and other env-specific CUE branches.
    Environment string `json:"environment,omitempty"`
}

type KrakenDAutoConfigStatus struct {
    Phase              AutoConfigPhase    `json:"phase,omitempty"`
    LastSyncTime       *metav1.Time       `json:"lastSyncTime,omitempty"`
    SpecChecksum       string             `json:"specChecksum,omitempty"`
    GeneratedEndpoints int                `json:"generatedEndpoints,omitempty"`
    SkippedOperations  int                `json:"skippedOperations,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
```

### Shared Reference Types

```go
type GatewayRef struct {
    Name      string `json:"name"`
    Namespace string `json:"namespace,omitempty"` // defaults to referencing resource's namespace
}

type PolicyRef struct {
    Name      string `json:"name"`
    Namespace string `json:"namespace,omitempty"` // defaults to referencing resource's namespace
}

type ConfigMapKeyRef struct {
    Name string `json:"name"`
    Key  string `json:"key,omitempty"`
}

type BackendSpec struct {
    Host        []string              `json:"host"`
    URLPattern  string                `json:"urlPattern"`
    Method      string                `json:"method,omitempty"`
    Encoding    string                `json:"encoding,omitempty"`
    Allow       []string              `json:"allow,omitempty"`
    Mapping     map[string]string     `json:"mapping,omitempty"`
    PolicyRef   *PolicyRef            `json:"policyRef,omitempty"`
    ExtraConfig *runtime.RawExtension `json:"extraConfig,omitempty"`
}
```

### Kubebuilder Markers

CRD generation markers are placed on the root types:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kgw
// +kubebuilder:printcolumn:name="Edition",type=string,JSONPath=`.spec.edition`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type KrakenDGateway struct { ... }
```

---

## 4. Controller Architecture

**Package:** `internal/controller/`

All controllers implement the `reconcile.Reconciler` interface from controller-runtime:

```go
type Reconciler interface {
    Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
}
```

### Controller Registration Pattern

Each controller exposes a `SetupWithManager` method that configures watches:

```go
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.KrakenDGateway{}).
        Owns(&appsv1.Deployment{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ConfigMap{}).
        Owns(&corev1.ServiceAccount{}).
        Owns(&policyv1.PodDisruptionBudget{}).
        Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
        Owns(&dragonflyv1alpha1.Dragonfly{}).
        Owns(&esv1.ExternalSecret{}).
        Owns(&istiov1.VirtualService{}).
        Watches(
            &v1alpha1.KrakenDEndpoint{},
            handler.EnqueueRequestsFromMapFunc(r.endpointToGateway),
        ).
        Watches(
            &v1alpha1.KrakenDBackendPolicy{},
            handler.EnqueueRequestsFromMapFunc(r.policyToGateways),
        ).
        Watches(
            &corev1.Secret{},
            handler.EnqueueRequestsFromMapFunc(r.licenseSecretToGateway),
        ).
        Complete(r)
}
```

### Controller–Dependency Map

```mermaid
graph TB
    subgraph "Controllers"
        GC[GatewayReconciler]
        EC[EndpointReconciler]
        PC[PolicyReconciler]
        ACC[KrakenDAutoConfigReconciler]
    end

    subgraph "Shared Dependencies"
        R[Renderer]
        V[Validator]
        RB[Resource Builders]
        CL[Clock]
        REC[EventRecorder]
        HC[HTTP Client]
    end

    subgraph "AutoConfig Subsystem"
        FE[Fetcher]
        CUE_E[CUE Evaluator]
        FI[Filter]
        GEN[Generator]
    end

    GC --> R
    GC --> V
    GC --> RB
    GC --> CL
    GC --> REC
    ACC --> FE
    ACC --> CUE_E
    ACC --> FI
    ACC --> GEN
    ACC --> REC
    FE --> HC
    EC --> REC
    PC --> REC
```

---

## 5. Gateway Controller

**File:** `internal/controller/gateway_controller.go`

The gateway controller is the primary reconciler. It orchestrates the full rendering pipeline (operator architecture §10), manages all owned Kubernetes resources, and handles edition-specific logic.

### Reconciler Struct

```go
type GatewayReconciler struct {
    client.Client
    Scheme    *runtime.Scheme
    Recorder  record.EventRecorder
    Renderer  renderer.Renderer
    Validator renderer.Validator
    Clock     clock.Clock
}
```

### Reconcile Flow

The `Reconcile` method follows the pipeline described in operator architecture §10:

```mermaid
flowchart TD
    A[Fetch KrakenDGateway] --> B{Found and<br/>not terminating?}
    B -->|No| Z[Forget the gateway<br/>metrics and rejection memo<br/>Return]
    B -->|Yes| C[List KrakenDEndpoints by gatewayRef]
    C --> C1[Fetch referenced KrakenDBackendPolicies]
    C1 --> C2[Determine CEFallback from<br/>status conditions LicenseDegraded]
    C2 --> F[Call Renderer.Render<br/>passes endpoints + policies +<br/>CEFallback. Renderer handles<br/>conflict detection internally]
    F --> G{Checksum changed?}
    G -->|No| G1{Image drift?}
    G1 -->|No| G2{Plugin checksum changed?}
    G2 -->|No| H[Record Accepted on each endpoint,<br/>reconcile owned resources]
    H --> R
    G2 -->|Yes| G5[Progressing=True<br/>reason DeploymentUpdated]
    G5 --> I[Patch pod annotation]
    G1 -->|Yes| G4[Progressing=True<br/>reason DeploymentUpdated]
    G4 --> I
    G -->|Yes| J0[PrepareValidationCopy:<br/>strip wildcards if EE<br/>and CE fallback not active]
    J0 --> J1{Same copy already<br/>rejected?}
    J1 -->|Yes| L[Re-apply the remembered rejection:<br/>ConfigValid=False,<br/>Warning event only if the verdict changed, return]
    J1 -->|No| J[Validate via krakend check -t -n -c]
    J --> K{Verdict?}
    K -->|Rejected| L
    K -->|Unavailable| L2[Set ConfigValid=Unknown<br/>reason ValidatorUnavailable,<br/>Ready Unknown, keep applied config,<br/>one Warning event,<br/>return error: retry with backoff]
    K -->|Valid| M[Update ConfigMap, set ConfigValid=True,<br/>Progressing=True reason ConfigDeployed]
    M --> I
    I --> N[Record Accepted on each endpoint,<br/>reconcile Deployment, Service, SA, PDB, HPA]
    N --> O[Reconcile Dragonfly CR if enabled]
    O --> P[Reconcile ExternalSecret if enabled]
    P --> Q[Reconcile VirtualService if Istio enabled]
    Q --> R[Inspect the Deployment status, then derive Ready and<br/>the phase from the conditions via gatewayReadinessFor<br/>and update the gateway status only if it changed]
```

### Key Implementation Details

**Endpoint conflict detection** — The renderer (§10) iterates all `KrakenDEndpoint` resources for the gateway and flattens their `spec.endpoints[]` arrays. It groups entries by `(endpoint, method)` tuples across all CRs. When multiple entries from different `KrakenDEndpoint` resources share the same path and method, the conflicting entries of all `KrakenDEndpoint` resources except the oldest (by `creationTimestamp`) are excluded from the rendered config. The renderer returns `ConflictedEndpoints` and `InvalidEndpoints` in `RenderOutput`. The gateway controller then writes its `Accepted` condition on each endpoint of the render, but only for a render that is the gateway's applied configuration (validated now, or unchanged since) and only when the verdict changes: `True` (`Accepted`) for an included endpoint, `True` (`PartiallyAccepted`) for one that lost some but not all of its path and method pairs, `False` (`EndpointConflict`) for one that lost all of them, and no `Accepted` condition for one excluded by a missing policy. `RenderOutput.EntryConflicts` names each lost entry and the `KrakenDEndpoint` that serves it, and the gateway controller writes them to `status.conflicts` in the same optimistic-lock patch as `Accepted`. Only the conflicting entries are dropped; the losing endpoint's other entries are still rendered. A `Warning` event with reason `EndpointConflict` is emitted when an endpoint becomes fully conflicted, a `Warning` event with reason `PartiallyAccepted` when it becomes partly conflicted (from `Accepted`, from no condition, or from `EndpointConflict`), and a `Normal` `Accepted` event when it is served whole again.

**Policy resolution** — The controller fetches all referenced `KrakenDBackendPolicy` resources before calling `Renderer.Render`, populating `RenderInput.Policies`. The renderer itself has no Kubernetes client dependency — all inputs are passed as parameters. If a policy referenced by a `policyRef` does not exist in the map, the renderer reports the owning endpoint in `InvalidEndpoints` and excludes it from the rendered config; the endpoint controller reports the cause through `ResolvedRefs`.

**CE fallback determination** — Before calling `Renderer.Render`, the controller calls `reconcileLicense`, which evaluates the license stage and returns the `ceFallback` verdict (the stage decision; while the license is unreadable, the stage judged from the last known expiry in `status.licenseExpiry` once that is inside the safety buffer or past, otherwise the last recorded decision). The verdict is passed as `RenderInput.CEFallback`, controlling image selection and wildcard endpoint stripping.

**Checksum comparison** — After rendering, the controller compares the new SHA-256 checksum against `status.configChecksum`. If unchanged, it skips ConfigMap update and validation. It still reconciles owned resources (Deployment, Service, etc.) to handle drift.

**Image drift detection** — Even when the config checksum is unchanged, the controller compares the desired container image (determined by edition, CE fallback state, and user overrides) against the current Deployment's container image. A mismatch triggers a Deployment patch to correct the image. This is the mechanism by which CE fallback and EE recovery change the running image.

**Plugin checksum** — Computed from ConfigMap data hashes and OCI image tags. Changes trigger a rolling restart via pod annotation patch, independent of config checksum.

**Phase transitions** — The controller does not latch `status.phase` at points in the pipeline. At the end of each reconcile it derives `Ready` and the phase from the gateway's conditions (`ConfigValid`, `Progressing`, `Available`, `LicenseExpired`, `LicenseDegraded`) with `gatewayReadinessFor`, and writes status only when something changed. The gateway reconcile evaluates the license itself, writes the `License*` conditions, and turns them into `Ready` and the phase. The phase is the compatibility view of `Ready`:

| Phase | Derived When |
|---|---|
| `Pending` | No configuration has been validated or rolled out yet |
| `Rendering` | No longer written by the operator; kept only for status values persisted by older versions |
| `Validating` | No longer written by the operator; kept only for status values persisted by older versions |
| `Deploying` | A rollout is in progress (`Progressing=True`), or the Deployment is not yet available |
| `Running` | Config valid, the applied config rolled out to every replica, the Deployment available, and no license condition degrading it |
| `Degraded` | CE fallback is active (`LicenseDegraded=True`) |
| `Error` | Config validation failed, the rollout failed or the Deployment lost availability (`Available=False`), or the license expired with `fallbackToCE=false` |

A configuration that could not be validated because the validator was unavailable leaves `Ready` Unknown with reason `ValidatorUnavailable` and keeps the serving phase.

### Watch Triggers

| Source | Event | Controller Action |
|---|---|---|
| KrakenDGateway | Create/Update/Delete | Full reconcile |
| Owned Deployment | Update (status change) | Update replicas/readyReplicas. A rollout counts as converged only when the Deployment has observed its latest generation, its pod template carries the applied config checksum, and replicas, updated replicas and available replicas all equal the desired count; then `Progressing=False` and `Available=True`. A Deployment `Available=False` outside a rollout is mirrored into the gateway's `Available` condition. On `ProgressDeadlineExceeded`: `Progressing=False`, `Available=False`, emit `RolloutFailed`. `Ready` and the phase are re-derived from the conditions |
| Owned Service | Update | Reconcile to correct drift |
| Owned ConfigMap | Update | Reconcile to correct drift |
| Owned Dragonfly CR | Status update | Update `DragonflyReady` condition on gateway; emit `DragonflyNotReady` Warning event on phase regression |
| Owned HPA | Update | Reconcile to correct drift |
| Owned ExternalSecret | Update | Reconcile to correct drift |
| Owned VirtualService | Update | Reconcile to correct drift |
| KrakenDEndpoint (via mapper) | Create/Update/Delete | Enqueue owning gateway — re-render config |
| KrakenDBackendPolicy (via mapper) | Update/Delete | Enqueue all gateways whose endpoints reference this policy |
| License Secret (via mapper) | Update | Enqueue gateway — the license is re-evaluated in the reconcile and may trigger CE fallback/recovery |

### Mapper Functions

```go
func (r *GatewayReconciler) endpointToGateway(
    ctx context.Context, obj client.Object,
) []reconcile.Request {
    ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
    if !ok {
        return nil
    }
    return []reconcile.Request{{
        NamespacedName: types.NamespacedName{
            Name:      ep.Spec.GatewayRef.Name,
            Namespace: ep.Namespace,
        },
    }}
}

func (r *GatewayReconciler) policyToGateways(
    ctx context.Context, obj client.Object,
) []reconcile.Request {
    // List all endpoints in the same namespace as the policy
    var endpoints v1alpha1.KrakenDEndpointList
    if err := r.List(ctx, &endpoints, client.InNamespace(obj.GetNamespace())); err != nil {
        return nil
    }
    seen := map[types.NamespacedName]struct{}{}
    var requests []reconcile.Request
    for i := range endpoints.Items {
        ep := &endpoints.Items[i]
        for _, entry := range ep.Spec.Endpoints {
            for _, be := range entry.Backends {
                if be.PolicyRef != nil && be.PolicyRef.Name == obj.GetName() {
                    nn := types.NamespacedName{
                        Name:      ep.Spec.GatewayRef.Name,
                        Namespace: ep.Namespace,
                    }
                    if _, ok := seen[nn]; !ok {
                        seen[nn] = struct{}{}
                        requests = append(requests, reconcile.Request{NamespacedName: nn})
                    }
                }
            }
        }
    }
    return requests
}
```

### licenseSecretToGateway Mapper

The `licenseSecretToGateway` mapper maps Secret changes to the gateways that reference them:

```go
func (r *GatewayReconciler) licenseSecretToGateway(
    ctx context.Context, obj client.Object,
) []reconcile.Request {
    // List all KrakenDGateways in the Secret's namespace
    // Return reconcile requests for any gateway whose license.secretRef
    // references this Secret's name, or whose ExternalSecret would
    // produce a Secret with this name
    var gateways v1alpha1.KrakenDGatewayList
    if err := r.List(ctx, &gateways, client.InNamespace(obj.GetNamespace())); err != nil {
        return nil
    }
    var requests []reconcile.Request
    for i := range gateways.Items {
        gw := &gateways.Items[i]
        if gw.Spec.License == nil {
            continue
        }
        // Match direct secretRef
        if gw.Spec.License.SecretRef != nil &&
            gw.Spec.License.SecretRef.Name == obj.GetName() {
            requests = append(requests, reconcile.Request{
                NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace},
            })
            continue
        }
        // Match ExternalSecret-generated Secret (convention: {gateway-name}-license)
        if gw.Spec.License.ExternalSecret.Enabled &&
            obj.GetName() == gw.Name+"-license" {
            requests = append(requests, reconcile.Request{
                NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace},
            })
        }
    }
    return requests
}
```

### Owned Resource Reconciliation

For each owned resource, the controller follows the **create-or-update** pattern using `controllerutil.CreateOrUpdate`:

```go
dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
    Name:      gw.Name,
    Namespace: gw.Namespace,
}}
op, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
    resources.BuildDeployment(dep, gw, configChecksum, pluginChecksum, desiredImage)
    return controllerutil.SetControllerReference(gw, dep, r.Scheme)
})
```

This ensures idempotent reconciliation: the same `Reconcile` call can be retried safely.

---

## 6. Endpoint Controller

**File:** `internal/controller/endpoint_controller.go`

The endpoint controller is lightweight. It resolves the endpoint's gateway and policy references into the `ResolvedRefs` condition and derives `Ready` and the phase. The `Accepted` condition belongs to the gateway controller, which this controller never overwrites; status is patched with an optimistic lock, and only when it changed.

### Reconciler Struct

```go
type KrakenDEndpointReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder
}
```

### Reconcile Flow

```mermaid
flowchart TD
    A[Fetch KrakenDEndpoint] --> B{Found?}
    B -->|No| Z[Return]
    B -->|Yes| C[Validate gatewayRef exists]
    C --> D{Gateway exists?}
    D -->|No| E[ResolvedRefs=False<br/>reason GatewayNotFound]
    D -->|Yes| F[Validate policyRef exists<br/>for each backend in each<br/>endpoints entry]
    F --> G{All policies exist?}
    G -->|No| H[ResolvedRefs=False<br/>reason PolicyNotFound]
    G -->|Yes| I[ResolvedRefs=True<br/>reason RefsResolved]
    E --> J
    H --> J
    I --> J[Drop the legacy Available condition,<br/>derive Ready and the phase from<br/>ResolvedRefs and Accepted<br/>via v1alpha1.EndpointReady]
    J --> K{Status changed?}
    K -->|Yes| L[Patch status with optimistic lock,<br/>emit an event on a ResolvedRefs transition]
    K -->|No| Z
```

`Ready` is `False` with the failing condition's reason when `ResolvedRefs` is `False` (`ResolvedRefs` is checked before `Accepted`), `Unknown` with reason `Pending` until the gateway has reported `Accepted` for the endpoint's current generation, and otherwise follows `Accepted`. The phase is the compatibility view of `Ready`: `Active` when `Ready` is `True`, `Pending` when it is `Unknown`, `Detached` for `GatewayNotFound`, `Conflicted` for `EndpointConflict` or `PartiallyAccepted`, and `Invalid` for any other failure. `ResolvedRefs` events fire on transitions only: a `Warning` with reason `GatewayNotFound` or `PolicyNotFound` when the references stop resolving, and a `Normal` `RefsResolved` when they resolve again.
The endpoint controller does NOT render config or manage Kubernetes resources. Config rendering is exclusively the gateway controller's responsibility, triggered when the gateway controller's endpoint watch fires.

### SetupWithManager

```go
func (r *EndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.KrakenDEndpoint{}, builder.WithPredicates(endpointPredicate())).
        Watches(
            &v1alpha1.KrakenDGateway{},
            handler.EnqueueRequestsFromMapFunc(r.gatewayToEndpoints),
            builder.WithPredicates(existencePredicate()),
        ).
        Watches(
            &v1alpha1.KrakenDBackendPolicy{},
            handler.EnqueueRequestsFromMapFunc(r.policyToEndpoints),
            builder.WithPredicates(existencePredicate()),
        ).
        Named("krakendendpoint").
        Complete(r)
}
```

Each watch carries a predicate so the controller's own writes do not enqueue it again:

- `endpointPredicate` gates the primary watch. It passes spec changes (generation bumps) and changes to the gateway-owned `Accepted` condition, ignoring its `lastTransitionTime`. The controller's own status writes change neither.
- `existencePredicate` gates the gateway and policy watches. It passes create and delete events only, because an update never changes whether the referenced object exists, which is all `ResolvedRefs` depends on.

The `gatewayToEndpoints` mapper re-queues all endpoints targeting a gateway through the gateway field index when the gateway is created or deleted (e.g., so endpoints can transition to `Detached` if the gateway is removed). The `policyToEndpoints` mapper does the same for the endpoints that reference a created or deleted policy, through the policy field index:

```go
func (r *KrakenDEndpointReconciler) gatewayToEndpoints(
    ctx context.Context, obj client.Object,
) []reconcile.Request {
    var endpoints v1alpha1.KrakenDEndpointList
    if err := r.List(ctx, &endpoints,
        client.MatchingFields{EndpointGatewayIndex: obj.GetNamespace() + "/" + obj.GetName()},
    ); err != nil {
        return nil
    }
    requests := make([]reconcile.Request, 0, len(endpoints.Items))
    for i := range endpoints.Items {
        requests = append(requests, reconcile.Request{
            NamespacedName: types.NamespacedName{
                Name:      endpoints.Items[i].Name,
                Namespace: endpoints.Items[i].Namespace,
            },
        })
    }
    return requests
}
```

---

## 7. Policy Controller

**File:** `internal/controller/policy_controller.go`

The policy controller maintains the `referencedBy` count in policy status and triggers gateway re-renders when policies change.

### Reconciler Struct

```go
type KrakenDBackendPolicyReconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder record.EventRecorder
}
```

### Reconcile Flow

```mermaid
flowchart TD
    A[Fetch KrakenDBackendPolicy] --> B{Found?}
    B -->|No| Z[Return]
    B -->|Yes| C[List KrakenDEndpoints<br/>in namespace]
    C --> D[Count endpoints where<br/>any backend references this policy]
    D --> E[Update status.referencedBy]
    E --> F{Validate policy fields}
    F -->|Invalid| G[Set Ready=False with the<br/>InvalidCircuitBreaker or<br/>InvalidRateLimit reason]
    F -->|Valid| H[Set Ready=True<br/>reason Ready]
    G --> J[Drop the legacy PolicyValid condition,<br/>write status only if it changed]
    H --> J
    J --> K[Emit an event on a Ready transition]
```

`Ready` replaces the earlier `PolicyValid` condition, which is removed from policies written by earlier versions. It is `False` when `circuitBreaker.maxErrors`, `interval` or `timeout` is not positive (`InvalidCircuitBreaker`) or `rateLimit.maxRate` is not positive (`InvalidRateLimit`), and `True` otherwise. Events fire on transitions only: a `Warning` with the invalid reason when `Ready` becomes `False` or changes reason, and a `Normal` `Ready` when it recovers.

The policy controller's reconciliation is straightforward. The `referencedBy` count scans all `KrakenDEndpoint` resources in the namespace and counts how many have at least one `backend[].policyRef.name` matching this policy. The important cross-controller interaction is through the gateway controller's `policyToGateways` mapper: when a policy is updated, all gateways with endpoints referencing that policy are re-queued for re-rendering.

### SetupWithManager

```go
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.KrakenDBackendPolicy{}).
        Watches(
            &v1alpha1.KrakenDEndpoint{},
            handler.EnqueueRequestsFromMapFunc(r.endpointToReferencedPolicies),
        ).
        Complete(r)
}
```

The `Watches(&v1alpha1.KrakenDEndpoint{})` ensures that when an endpoint is created, updated, or deleted, the policies it references are re-reconciled to update their `referencedBy` counts:

```go
func (r *PolicyReconciler) endpointToReferencedPolicies(
    ctx context.Context, obj client.Object,
) []reconcile.Request {
    ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
    if !ok {
        return nil
    }
    seen := map[string]struct{}{}
    var requests []reconcile.Request
    for _, entry := range ep.Spec.Endpoints {
        for _, be := range entry.Backends {
            if be.PolicyRef != nil {
                if _, ok := seen[be.PolicyRef.Name]; !ok {
                    seen[be.PolicyRef.Name] = struct{}{}
                    requests = append(requests, reconcile.Request{
                        NamespacedName: types.NamespacedName{
                            Name:      be.PolicyRef.Name,
                            Namespace: ep.Namespace,
                        },
                    })
                }
            }
        }
    }
    return requests
}
```

---

## 8. AutoConfig Controller

**File:** `internal/controller/krakendautoconfig_controller.go`

The autoconfig controller watches `KrakenDAutoConfig` resources and orchestrates the OpenAPI-to-endpoint pipeline described in operator architecture §16. The controller uses CUE as its transformation engine: OpenAPI spec data is unified with CUE definitions to produce `KrakenDEndpointSpec` objects.

### Reconciler Struct

```go
type KrakenDAutoConfigReconciler struct {
    client.Client
    Scheme       *runtime.Scheme
    Recorder     record.EventRecorder
    Fetcher      autoconfig.Fetcher
    CUEEvaluator autoconfig.CUEEvaluator
    Filter       autoconfig.Filter
    Generator    autoconfig.Generator
    Clock        clock.Clock
}
```

### Reconcile Flow

```mermaid
flowchart TD
    A[Fetch KrakenDAutoConfig] --> B{Found?}
    B -->|No| Z[Return]
    B -->|Yes| BD{deletionTimestamp set?}
    BD -->|Yes| Z
    BD -->|No| D[Fetch OpenAPI spec<br/>via Fetcher]
    D --> E{Fetch OK?}
    E -->|No| F[Set SpecAvailable=False<br/>Fail sync:<br/>SpecFetchFailed]
    E -->|Yes| G[Resolve external $refs, strip servers<br/>Set SpecAvailable=True]
    G --> H[Combined checksum: spec checksum +<br/>CUE definitions resourceVersion + generation<br/>inputsChanged = differs from status.specChecksum]
    H --> J1[Load default CUE definitions<br/>from krakend-cue-definitions ConfigMap<br/>or the embedded defaults]
    J1 --> J2{Custom CUE ConfigMap<br/>referenced?}
    J2 -->|Yes| J3[Load custom CUE definitions]
    J2 -->|No| J4[CUE Evaluator: unify<br/>spec + defaults + CR overrides]
    J3 --> J4
    J4 --> J5{CUE evaluation OK?}
    J5 -->|No| J6[Fail sync:<br/>CUEEvaluationFailed]
    J5 -->|Yes| J7[Emit CUEEvaluationWarning per<br/>evaluator warning if inputsChanged]
    J7 --> J8{Every override<br/>matched an operationId?}
    J8 -->|No| J9[Fail sync:<br/>UnmatchedOverride]
    J8 -->|Yes| M[Apply include/exclude filters]
    M --> M2{additionalEndpoints set?}
    M2 -->|No| O
    M2 -->|Yes| M3[BuildAdditionalEntries<br/>synthesize AdditionalEndpoint specs]
    M3 --> M4[ApplyURLTransformToEntries<br/>apply same urlTransform as spec-derived]
    M4 --> MB{Resolve base path:<br/>manual → addPathPrefix → DeriveBasePath}
    MB -->|indeterminate| ME[Fail sync:<br/>AdditionalEndpointScopeFailed]
    MB -->|resolved| MS[ScopeAdditionalEntries<br/>prepend base to public paths]
    MS --> M5[MergeAdditional<br/>additional wins on endpoint:method collision]
    M5 --> M6[Emit AdditionalEndpointOverride Warning<br/>per replaced entry if inputsChanged]
    M6 --> O[Generate KrakenDEndpoints<br/>via Generator]
    O --> O1{Generate OK?}
    O1 -->|No| O2[Fail sync:<br/>CUEEvaluationFailed]
    O1 -->|Yes| O3[Emit DuplicateOperationId per<br/>duplicate if inputsChanged]
    O3 --> P[Diff against the endpoints labeled<br/>gateway.krakend.io/autoconfig=name]
    P --> Q[Delete undesired endpoints<br/>CreateOrUpdate desired: labels, spec<br/>by JSON value, controller reference]
    Q --> Q1{Every write OK?}
    Q1 -->|No| Q2[Fail sync:<br/>EndpointReconcileFailed]
    Q1 -->|Yes| R[Set Synced=True, specChecksum,<br/>endpoint counts; Ready and phase=Synced<br/>are derived when status is written]
    R --> R1{inputsChanged or<br/>any endpoint write?}
    R1 -->|Yes| R2[Set lastSyncTime]
    R1 -->|No| R3
    R2 --> R3{Status differs from the<br/>status read at the start?}
    R3 -->|Yes| R4[Write status]
    R3 -->|No| R5
    R4 --> R5[Emit EndpointsGenerated if inputsChanged<br/>or any endpoint write]
    R5 --> R6[RequeueAfter periodic.interval<br/>or 5m for OnChange]
```

Every reconcile runs the whole pipeline — there is no checksum gate — so owned endpoints converge to the desired state whatever woke the controller. **Fail sync** is `handleSyncedFailure`: the `Synced` condition `False` with that reason (so `Ready` is `False` and the derived phase is `Error`), and a Warning event with the same reason; `OnChange` returns the error so controller-runtime retries with exponential backoff, `Periodic` requeues at `spec.periodic.interval`. A fetch failure (`SpecFetchFailed`), including an external `$ref` document that can't be fetched or decoded, fails the sync the same way and also sets `SpecAvailable=False`. If the failure's status write conflicts, the reconcile still returns the failure's result (not the quiet one-second requeue, which would reset the backoff) and records no event. A failed sync leaves `status.specChecksum` at the last successful sync's value, and drift repair stops at the failure until a sync succeeds: every other failure stops the pipeline before any endpoint is touched, and an endpoint write failure stops convergence at that endpoint. `phase` never passes through `Fetching` or `Rendering`. It is derived from the `Synced` condition each time status is written, and every status write sets `Synced`, so the phase is empty before the first write and is then only `Synced` or `Error`; `Ready` is `Unknown` with reason `Pending` until the first sync.

A terminating AutoConfig (`deletionTimestamp` set) is not reconciled. Under foreground deletion it lingers while garbage collection deletes its endpoints, each delete re-enqueues it through the `Owns` watch, and converging would recreate the endpoint just collected.

**Additional endpoints pipeline note:** `spec.additionalEndpoints` entries bypass `filter` and `overrides` (they carry no `operationId`), but they DO receive `urlTransform`. After applying the URL transform, additional endpoints are scoped under the application's base path — `spec.additionalEndpointsBasePath` if set, else `urlTransform.addPathPrefix` (already applied, so no further scoping), else the common parent directory derived from the generated endpoints (`DeriveBasePath`). If none of these resolves to a non-empty base, the sync fails with `AdditionalEndpointScopeFailed`. Scoping prepends the base to the public path only; backend `urlPattern` is unchanged. `ApplyURLTransformToEntries` is called before scoping and `MergeAdditional` so that collision keys (`endpoint:method`) align with the already-transformed spec-derived entries. On collision, the additional entry wins and, when the inputs changed since the last successful sync, the controller emits an `AdditionalEndpointOverride` Warning event.

### Periodic Trigger and Resync

A successful reconcile returns `ctrl.Result{RequeueAfter: interval}`: `spec.periodic.interval` for `trigger: Periodic`, otherwise `defaultResyncInterval` (5 minutes), so `OnChange` AutoConfigs are also re-polled without a watch event and upstream spec changes are picked up. A resync runs the same full pipeline as a watch-triggered reconcile.

`status.specChecksum` records which inputs the last successful sync used; it does not gate evaluation. A sync whose combined checksum differs from it, or that wrote an endpoint, sets `lastSyncTime` and emits `EndpointsGenerated`; the `CUEEvaluationWarning`, `DuplicateOperationId`, and `AdditionalEndpointOverride` warnings are emitted only when the checksum differs. Churn is avoided by comparison instead: endpoint labels are compared with `maps.Equal` and specs by decoded JSON value (`endpointSpecEqual`, so the API server's re-encoding of raw `extraConfig` doesn't count as a change), `CreateOrUpdate` writes nothing for an endpoint already in the desired state, and status is written only when it differs semantically from the status read at the start of the reconcile. A steady-state reconcile writes nothing and emits no event.

### SetupWithManager

```go
func (r *KrakenDAutoConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&v1alpha1.KrakenDAutoConfig{}, builder.WithPredicates(predicate.Or(
            predicate.GenerationChangedPredicate{},
            predicate.LabelChangedPredicate{},
            predicate.AnnotationChangedPredicate{},
        ))).
        Owns(&v1alpha1.KrakenDEndpoint{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
        Watches(
            &corev1.ConfigMap{},
            handler.EnqueueRequestsFromMapFunc(r.configMapToAutoConfigs),
        ).
        Named("krakendautoconfig").
        Complete(r)
}
```

The `For` predicate ignores status-only updates, so the reconciler's own status writes never re-enqueue the AutoConfig. Generation covers spec edits, labels are included because application deploys relabel the AutoConfig, and annotations let `kubectl annotate` force an immediate reconcile. The `Owns(&v1alpha1.KrakenDEndpoint{})` watch re-enqueues the owning AutoConfig when a generated endpoint is deleted or its spec changes (a generation bump) — endpoint status updates alone are ignored — so a hand-edited or deleted endpoint is restored. The `Watches(&corev1.ConfigMap{})` watch maps a changed ConfigMap to every AutoConfig in its namespace that depends on it: the default `krakend-cue-definitions` ConfigMap, a custom CUE ConfigMap referenced by `cue.definitionsConfigMapRef`, or an OpenAPI spec ConfigMap referenced by `openapi.configMapRef`.

---

## 9. License Evaluation

**File:** `internal/controller/gateway_license.go`

The gateway reconcile evaluates an EE gateway's license before it renders, because the result decides whether the render is CE. There is no separate license goroutine: gateway status has a single writer, and the reconcile writes nothing to the user's KrakenDGateway object.

### Design

`reconcileLicense(ctx, gw)` returns a `licenseVerdict`:

```go
type licenseVerdict struct {
    ceFallback          bool          // render and run CE instead of EE
    requeueAfter        time.Duration // when to look at the license again
    licenseChecksum     string        // SHA-256 of the license bytes the pods must run with
    keepDeployedLicense bool          // license unreadable: keep the checksum the Deployment carries
}
```

The stage comes from `license.Window{Warning, SafetyBuffer}.StageAt(notAfter, now)`: `StageValid`, `StageExpiringSoon`, `StagePreExpiry` or `StageExpired`. `Warning` is `spec.license.expiryWarningDays` (default 30 days) and must stay longer than `SafetyBuffer` (1 hour); a compile-time constant check enforces this. The reconcile requeues at the next stage boundary (`Window.NextChange`), and at least every 5 minutes (`licenseRecheckInterval`). A Secret change enqueues the gateway through the Secret watch. If the license cannot be read, `LicenseValid` is `Unknown` unless the last known expiry in `status.licenseExpiry` is already inside the safety buffer or past, in which case the stage verdict (`False`) applies and the CE fallback happens as for a readable license; otherwise the last fallback decision is kept. The gateway controller's retry backoff is capped at the same 5 minutes (`newGatewayRateLimiter`, built with `cappedRateLimiter`). CE gateways return an empty verdict.

The license is mounted with `subPath`, so a renewed Secret never reaches running pods by itself. `licenseChecksum` is the SHA-256 of the license bytes read, set for every EE gateway whose license was read, CE fallback or not, because the license stays mounted under fallback; the gateway controller passes it through `infraInputs` to `resources.BuildDeployment`, which writes it as the `krakend.io/checksum-license` pod-template annotation. A changed value rolls the Deployment (`Progressing=True/DeploymentUpdated`, and `deploymentConverged` waits for it). When the license is unreadable, `keepDeployedLicense` makes the controller reuse the annotation the live Deployment carries, so nothing rolls. The post-restart Job identity does not include it.

### License Check Logic

```mermaid
flowchart TD
    A[Gateway reconcile, edition=EE] --> C[Read license Secret]
    C --> D{Secret readable<br/>and parseable?}
    D -->|No| E[Set LicenseSecretUnavailable=True,<br/>emit LicenseSecretMissing once;<br/>LicenseValid=Unknown unless the last known<br/>expiry is in the safety buffer or past,<br/>then apply that stage;<br/>otherwise keep the last fallback decision;<br/>keep the deployed license checksum;<br/>requeue at the next boundary, at most 5 minutes]
    D -->|Yes| F[Set licenseExpiry and the<br/>license_expiry_seconds metric]
    F --> G{Stage}
    G -->|Valid| M[LicenseValid=True LicenseOK]
    G -->|ExpiringSoon| L[LicenseValid=True LicenseExpiringSoon,<br/>emit LicenseExpiringSoon on entering]
    M --> R{LicenseExpired or<br/>LicenseDegraded True?}
    L --> R
    R -->|Yes| R1[Both False LicenseRestored,<br/>emit LicenseRestored]
    G -->|PreExpiry or Expired| H[LicenseValid=False and LicenseExpired=True]
    H --> N{fallbackToCE?}
    N -->|Yes| O[LicenseDegraded=True LicenseFallbackCE,<br/>emit LicenseFallbackCE,<br/>render CE]
    N -->|No| P[emit LicenseExpiredNoFallback;<br/>the gateway derives Ready=False and phase=Error]
```

### Events on Transitions

Every license event goes through a transition-only path (`setProblemCondition` for `LicenseSecretUnavailable` and `LicenseDegraded`, a previous-reason check for `LicenseExpiringSoon`, a previous-status check for `LicenseExpiredNoFallback` and `LicenseRestored`), so a steady state emits nothing on repeated reconciles. `LicenseExpiringSoon` fires once when the license enters the warning window.

---

## 10. Configuration Rendering Pipeline

**Package:** `internal/renderer/`

The renderer transforms CRD state into a deterministic `krakend.json` byte slice. It is a pure function with no Kubernetes client dependency — all inputs are passed as parameters.

### Interface

```go
type Renderer interface {
    Render(input RenderInput) (*RenderOutput, error)
}

type RenderInput struct {
    Gateway    *v1alpha1.KrakenDGateway
    Endpoints  []v1alpha1.KrakenDEndpoint
    Policies   map[string]*v1alpha1.KrakenDBackendPolicy // keyed by policy name
    CEFallback bool
    Dragonfly  *DragonflyState // nil if not enabled
}

type DragonflyState struct {
    Enabled     bool
    ServiceDNS  string // e.g., "production-gateway-dragonfly.api-gateway.svc.cluster.local:6379"
}

type RenderOutput struct {
    JSON             []byte
    Checksum         string   // SHA-256 hex
    DesiredImage     string
    PluginChecksum   string
    ConflictedEndpoints []types.NamespacedName
    InvalidEndpoints    []types.NamespacedName
}
```

### Renderer and Validator Construction

```go
type Options struct{} // reserved for future configuration

func New(opts Options) *krakendRenderer {
    return &krakendRenderer{}
}

type ValidatorOptions struct {
    Executor   CommandExecutor
    BinaryPath string
    Timeout    time.Duration // zero means the 30-second default
}

func NewValidator(opts ValidatorOptions) *KrakenDValidator {
    return &KrakenDValidator{
        Executor:   opts.Executor,
        BinaryPath: opts.BinaryPath,
        Timeout:    opts.Timeout,
    }
}

func NewKrakenDExecutor(binaryPath string) *KrakenDExecutor {
    return &KrakenDExecutor{BinaryPath: binaryPath}
}
```

### Validator Interface

```go
type Validator interface {
    Validate(ctx context.Context, jsonData []byte) error
    PrepareValidationCopy(jsonData []byte, eeWithoutFallback bool) ([]byte, error)
}
```

### Implementation Files

| File | Responsibility |
|---|---|
| `config.go` | Top-level config builder — assembles the root `krakend.json` object |
| `endpoints.go` | Builds the `endpoints` array by flattening all `KrakenDEndpoint.spec.endpoints[]` entries, sorts by path then method |
| `extra_config.go` | Merges `extra_config` namespaces from gateway spec, policies, and endpoint overrides |
| `plugins.go` | Builds the `plugin` root key when plugins are configured. Computes plugin checksum from ConfigMap data hashes and OCI image tags |
| `validator.go` | Wraps `krakend check -t -n -c` execution via the `CommandExecutor` interface |

### Deterministic Serialization

```go
func serializeJSON(config map[string]any) ([]byte, error) {
    // json.Marshal produces sorted map keys by default in Go
    data, err := json.Marshal(config)
    if err != nil {
        return nil, fmt.Errorf("marshaling config to JSON: %w", err)
    }
    // Pretty-print for readability in ConfigMap
    var buf bytes.Buffer
    if err := json.Indent(&buf, data, "", "  "); err != nil {
        return nil, fmt.Errorf("indenting JSON: %w", err)
    }
    return buf.Bytes(), nil
}
```

Go's `encoding/json` package serializes map keys in sorted order by default (`encoding/json` sorts map keys lexically). Slices (endpoints, hosts, `extra_config` keys) are explicitly sorted before serialization.

### Validation Execution

```go
type CommandExecutor interface {
    Execute(ctx context.Context, name string, args ...string) ([]byte, error)
}

type KrakenDExecutor struct {
    BinaryPath string
}

func (e *KrakenDExecutor) Execute(
    ctx context.Context, name string, args ...string,
) ([]byte, error) {
    cmd := exec.CommandContext(ctx, name, args...)
    return cmd.CombinedOutput()
}

type KrakenDValidator struct {
    Executor   CommandExecutor
    BinaryPath string
    Timeout    time.Duration // zero means the 30-second default
}
```

The validator writes the rendered JSON to a temporary file, runs `krakend check -t -n -c <path>`, and returns the result:

```go
func (v *KrakenDValidator) Validate(ctx context.Context, jsonData []byte) error {
    ctx, cancel := context.WithTimeout(ctx, v.timeout())
    defer cancel()

    tmpFile, err := os.CreateTemp("", "krakend-config-*.json")
    if err != nil {
        return fmt.Errorf("creating temp file: %w", err)
    }
    tmpName := tmpFile.Name()
    defer os.Remove(tmpName)

    if _, err := tmpFile.Write(jsonData); err != nil {
        tmpFile.Close()
        return fmt.Errorf("writing config to temp file: %w", err)
    }
    if err := tmpFile.Close(); err != nil {
        return fmt.Errorf("closing temp file: %w", err)
    }

    output, err := v.Executor.Execute(ctx, v.BinaryPath, "check", "-t", "-n", "-c", tmpName)
    if err != nil {
        return classifyCheckError(ctx, output, err)
    }
    return nil
}
```

`classifyCheckError` returns a `*ValidationError` only when the process exited with a status above zero before the timeout (30 seconds by default). A missing binary, a deadline overrun or a signal kill comes back as a plain wrapped error: the config was not judged and the caller retries.

### EE Wildcard Handling

Per operator architecture §10, EE configurations containing wildcard endpoints (`/*`) require special handling during validation:

```go
func (v *KrakenDValidator) PrepareValidationCopy(jsonData []byte, eeWithoutFallback bool) ([]byte, error) {
    if !eeWithoutFallback {
        return jsonData, nil
    }
    // Strip wildcard endpoints from the validation copy
    // The CE validator rejects /* patterns
    var config map[string]any
    if err := json.Unmarshal(jsonData, &config); err != nil {
        return nil, fmt.Errorf("unmarshaling config for validation copy: %w", err)
    }
    endpoints, ok := config["endpoints"].([]any)
    if !ok {
        return jsonData, nil
    }
    var filtered []any
    for _, ep := range endpoints {
        epMap, ok := ep.(map[string]any)
        if !ok {
            continue
        }
        if path, ok := epMap["endpoint"].(string); ok && path == "/*" {
            continue // strip wildcard
        }
        filtered = append(filtered, ep)
    }
    config["endpoints"] = filtered
    return serializeJSON(config)
}
```

### Extra Config Merge Order

**Backend-level `extra_config`** — When multiple sources provide the same `extra_config` namespace key at the backend level:

1. **Inline backend `extraConfig`** (highest precedence — on `KrakenDEndpoint.spec.endpoints[].backends[].extraConfig`)
2. **Policy typed fields** (`circuitBreaker`, `rateLimit`, `cache`) — serialized to their corresponding KrakenD `extra_config` namespace keys (`qos/circuit-breaker`, `qos/ratelimit/router`, etc.)
3. **Policy `raw`** (from referenced `KrakenDBackendPolicy.spec.raw`)

Merge is per-key at the top level of each namespace. Inline backend keys overwrite policy keys (both typed and raw) with the same namespace. This matches the behavior described in operator architecture §3.2.

**Endpoint-level `extra_config`** — Each `EndpointEntry.ExtraConfig` maps directly to the endpoint-level `extra_config` key in the rendered KrakenD JSON. This covers endpoint-scoped features such as `auth/validator` (JWT validation), `qos/ratelimit/router` (per-endpoint rate limiting), and `validation/cel` (CEL request validation). These are rendered as-is from the `ExtraConfig` field — no merge with policy fields occurs at the endpoint level.

---

## 11. Resource Builders

**Package:** `internal/resources/`

Resource builders are pure functions that construct Kubernetes object specs from CRD state. They take a target object pointer and mutate it in place, following the `controllerutil.CreateOrUpdate` mutate-function pattern.

### Builder Functions

| File | Function | Output Resource |
|---|---|---|
| `deployment.go` | `BuildDeployment(dep, gw, configChecksum, pluginChecksum, image)` | `appsv1.Deployment` |
| `service.go` | `BuildService(svc, gw)` | `corev1.Service` |
| `configmap.go` | `BuildConfigMap(cm, gw, jsonData)` | `corev1.ConfigMap` |
| `serviceaccount.go` | `BuildServiceAccount(sa, gw)` | `corev1.ServiceAccount` |
| `pdb.go` | `BuildPDB(pdb, gw)` | `policyv1.PodDisruptionBudget` |
| `hpa.go` | `BuildHPA(hpa, gw)` | `autoscalingv2.HorizontalPodAutoscaler` |
| `dragonfly.go` | `BuildDragonfly(df, gw)` | `dragonflyv1alpha1.Dragonfly` |
| `virtualservice.go` | `BuildVirtualService(vs, gw)` | `istiov1.VirtualService` |
| `externalsecret.go` | `BuildExternalSecret(es, gw)` | `esv1.ExternalSecret` |

### Deployment Builder Detail

The Deployment builder is the most complex resource builder. It assembles:

```mermaid
flowchart TD
    A[Deployment Builder] --> B[Pod Template]
    B --> C[Container Spec]
    B --> D[Init Containers<br/>for OCI plugin images]
    B --> E[Volume Mounts]
    B --> F[Security Context]
    B --> G[Health Probes]
    B --> H[Pod Annotations<br/>checksum/config<br/>checksum/plugins]

    E --> E1["ConfigMap volume<br/>/etc/krakend/krakend.json"]
    E --> E2["Secret volume<br/>/etc/krakend/LICENSE<br/>if EE"]
    E --> E3["Plugin volume<br/>/opt/krakend/plugins<br/>if plugins configured"]
    E --> E4["emptyDir /tmp"]

    C --> C1[Image selection<br/>edition + fallback logic]
    C --> C2[Resource limits]
    C --> C3[Security context<br/>readOnlyRootFilesystem: true]
```

**Image selection logic:**

```go
func ResolveImage(gw *v1alpha1.KrakenDGateway, ceFallback bool) string {
    if ceFallback {
        if gw.Spec.CEImage != "" {
            return gw.Spec.CEImage
        }
        return fmt.Sprintf("krakend/krakend:%s", gw.Spec.Version)
    }
    if gw.Spec.Image != "" {
        return gw.Spec.Image
    }
    switch gw.Spec.Edition {
    case v1alpha1.EditionEE:
        return fmt.Sprintf("krakend/krakend-ee:%s", gw.Spec.Version)
    default:
        return fmt.Sprintf("krakend/krakend:%s", gw.Spec.Version)
    }
}
```

**Plugin volume assembly:**

```go
func buildPluginVolumes(
    gw *v1alpha1.KrakenDGateway,
) ([]corev1.Volume, []corev1.VolumeMount, []corev1.Container) {
    if gw.Spec.Plugins == nil || len(gw.Spec.Plugins.Sources) == 0 {
        return nil, nil, nil
    }

    sources := gw.Spec.Plugins.Sources
    var hasConfigMap, hasPVC, hasOCI bool
    for _, src := range sources {
        if src.ConfigMapRef != nil {
            hasConfigMap = true
        }
        if src.PersistentVolumeClaimRef != nil {
            hasPVC = true
        }
        if src.ImageRef != nil {
            hasOCI = true
        }
    }

    needsMultiSource := (hasConfigMap && hasPVC) ||
        (hasConfigMap && hasOCI) ||
        (hasPVC && hasOCI) ||
        hasOCI

    if needsMultiSource {
        return buildMultiSourcePluginVolumes(gw)
    }
    return buildSingleSourcePluginVolumes(gw)
}
```

### Rolling Update Strategy

Every Deployment is configured with the zero-downtime strategy from operator architecture §12:

```go
dep.Spec.Strategy = appsv1.DeploymentStrategy{
    Type: appsv1.RollingUpdateDeploymentStrategyType,
    RollingUpdate: &appsv1.RollingUpdateDeployment{
        MaxSurge:       &intstr.IntOrString{Type: intstr.Int, IntVal: 1},
        MaxUnavailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 0},
    },
}
```

### Label Convention

All managed KrakenD gateway resources carry consistent labels:

```go
func StandardLabels(gw *v1alpha1.KrakenDGateway) map[string]string {
    return map[string]string{
        "app.kubernetes.io/name":       "krakend",
        "app.kubernetes.io/instance":   gw.Name,
        "app.kubernetes.io/version":    gw.Spec.Version,
        "app.kubernetes.io/component":  "gateway",
        "app.kubernetes.io/part-of":    "krakend-operator",
        "app.kubernetes.io/managed-by": "krakend-operator",
    }
}
```

The Dragonfly CR uses its own label set matching operator architecture §6:

```go
func DragonflyLabels(gw *v1alpha1.KrakenDGateway) map[string]string {
    return map[string]string{
        "app.kubernetes.io/name":       "dragonfly",
        "app.kubernetes.io/instance":   gw.Name + "-dragonfly",
        "app.kubernetes.io/part-of":    "krakend-operator",
        "app.kubernetes.io/managed-by": "krakend-operator",
    }
}
```

`BuildDragonfly` must use `DragonflyLabels`, not `StandardLabels`, to avoid incorrect `name: krakend` and `component: gateway` labels on the Dragonfly CR.
```

---

## 12. Webhook Validation

**Package:** `internal/webhook/`

The operator deploys a `ValidatingAdmissionWebhook` implementing all rules from operator architecture §15.

### Handler Structure

```go
type GatewayValidator struct {
    client.Client
}

type EndpointValidator struct {
    client.Client
}

type PolicyValidator struct {
    client.Client
}

type AutoConfigValidator struct {
    client.Client
}
```

Each validator implements the `webhook.CustomValidator[T]` generic interface (controller-runtime v0.19+):

```go
// Example: GatewayValidator implements CustomValidator[*v1alpha1.KrakenDGateway]
type CustomValidator[T client.Object] interface {
    ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error)
    ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error)
    ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error)
}
```

All validators implement `ValidateUpdate` by delegating to the same structural checks as `ValidateCreate` (applied to `newObj`). This ensures updates cannot bypass validation (e.g., changing `gatewayRef` to a non-existent gateway, or adding an invalid `policyRef`).

### Validation Rules

**KrakenDGateway:**

```go
func (v *GatewayValidator) ValidateCreate(
    ctx context.Context, gw *v1alpha1.KrakenDGateway,
) (admission.Warnings, error) {
    var errs field.ErrorList

    // EE requires license configuration
    if gw.Spec.Edition == v1alpha1.EditionEE {
        if gw.Spec.License == nil ||
            (!gw.Spec.License.ExternalSecret.Enabled && gw.Spec.License.SecretRef == nil) {
            errs = append(errs, field.Required(
                field.NewPath("spec", "license"),
                "edition EE requires license.externalSecret.enabled or license.secretRef",
            ))
        }
    }

    // CE must not have license configuration
    if gw.Spec.Edition == v1alpha1.EditionCE && gw.Spec.License != nil {
        if gw.Spec.License.ExternalSecret.Enabled || gw.Spec.License.SecretRef != nil {
            errs = append(errs, field.Forbidden(
                field.NewPath("spec", "license"),
                "CE edition does not require license configuration",
            ))
        }
    }

    // Mutually exclusive license sources
    if gw.Spec.License != nil &&
        gw.Spec.License.ExternalSecret.Enabled && gw.Spec.License.SecretRef != nil {
        errs = append(errs, field.Invalid(
            field.NewPath("spec", "license"),
            "both",
            "externalSecret and secretRef are mutually exclusive",
        ))
    }

    // Only one PVC plugin source allowed
    if gw.Spec.Plugins != nil {
        pvcCount := 0
        for _, src := range gw.Spec.Plugins.Sources {
            if src.PersistentVolumeClaimRef != nil {
                pvcCount++
            }
        }
        if pvcCount > 1 {
            errs = append(errs, field.Invalid(
                field.NewPath("spec", "plugins", "sources"),
                pvcCount,
                "only one PVC plugin source is supported",
            ))
        }
    }

    return nil, errs.ToAggregate()
}
```

**KrakenDEndpoint:**

```go
func (v *EndpointValidator) ValidateCreate(
    ctx context.Context, ep *v1alpha1.KrakenDEndpoint,
) (admission.Warnings, error) {
    var warnings admission.Warnings
    var errs field.ErrorList

    // Detect intra-CR duplicate (endpoint, method) pairs
    seenPaths := make(map[string]struct{})
    for i, entry := range ep.Spec.Endpoints {
        key := entry.Method + " " + entry.Endpoint
        if _, exists := seenPaths[key]; exists {
            errs = append(errs, field.Duplicate(
                field.NewPath("spec", "endpoints").Index(i),
                key,
            ))
        }
        seenPaths[key] = struct{}{}
    }

    // gatewayRef must exist (namespace-aware)
    gwNS := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace)
    gw := &v1alpha1.KrakenDGateway{}
    if err := v.Get(ctx, types.NamespacedName{
        Name:      ep.Spec.GatewayRef.Name,
        Namespace: gwNS,
    }, gw); err != nil {
        errs = append(errs, field.NotFound(
            field.NewPath("spec", "gatewayRef", "name"),
            ep.Spec.GatewayRef.Name,
        ))
    }

    // policyRef must exist for each backend (namespace-aware)
    for i, entry := range ep.Spec.Endpoints {
        for j, be := range entry.Backends {
            if be.PolicyRef != nil {
                policy := &v1alpha1.KrakenDBackendPolicy{}
                if err := v.Get(ctx, types.NamespacedName{
                    Name:      be.PolicyRef.Name,
                    Namespace: be.PolicyRef.ResolvedNamespace(ep.Namespace),
                }, policy); err != nil {
                    errs = append(errs, field.NotFound(
                        field.NewPath("spec", "endpoints").Index(i).Child("backends").Index(j).Child("policyRef", "name"),
                        be.PolicyRef.Name,
                    ))
                }
            }
        }
    }

    // Warn on conflict via field index (cluster-wide, not namespace-scoped)
    gwKey := gwNS + "/" + ep.Spec.GatewayRef.Name
    var existing v1alpha1.KrakenDEndpointList
    if err := v.List(ctx, &existing,
        client.MatchingFields{controller.EndpointGatewayIndex: gwKey},
    ); err != nil {
        errs = append(errs, field.InternalError(
            field.NewPath("spec", "gatewayRef"),
            fmt.Errorf("listing endpoints for conflict check: %w", err),
        ))
        return errs, warnings
    }
    for _, newEntry := range ep.Spec.Endpoints {
        for _, other := range existing.Items {
            if other.Name == ep.Name && other.Namespace == ep.Namespace {
                continue
            }
            for _, otherEntry := range other.Spec.Endpoints {
                if otherEntry.Endpoint == newEntry.Endpoint &&
                    otherEntry.Method == newEntry.Method {
                    warnings = append(warnings, fmt.Sprintf(
                        "endpoint %s %s already exists on gateway %s "+
                            "(defined by %s/%s) — conflict resolved by creationTimestamp",
                        newEntry.Method, newEntry.Endpoint,
                        gwKey, other.Namespace, other.Name,
                    ))
                }
            }
        }
    }

    return warnings, errs.ToAggregate()
}
```

**KrakenDBackendPolicy (CREATE/UPDATE):**

```go
func (v *PolicyValidator) ValidateCreate(
    ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy,
) (admission.Warnings, error) {
    var errs field.ErrorList

    if policy.Spec.CircuitBreaker != nil {
        if policy.Spec.CircuitBreaker.MaxErrors <= 0 {
            errs = append(errs, field.Invalid(
                field.NewPath("spec", "circuitBreaker", "maxErrors"),
                policy.Spec.CircuitBreaker.MaxErrors,
                "must be greater than 0",
            ))
        }
    }
    if policy.Spec.RateLimit != nil {
        if policy.Spec.RateLimit.MaxRate <= 0 {
            errs = append(errs, field.Invalid(
                field.NewPath("spec", "rateLimit", "maxRate"),
                policy.Spec.RateLimit.MaxRate,
                "must be greater than 0",
            ))
        }
    }
    return nil, errs.ToAggregate()
}
```

**KrakenDBackendPolicy (DELETE):**

```go
func (v *PolicyValidator) ValidateDelete(
    ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy,
) (admission.Warnings, error) {
    var endpoints v1alpha1.KrakenDEndpointList
    if err := v.List(ctx, &endpoints, client.InNamespace(policy.Namespace)); err != nil {
        return nil, fmt.Errorf("listing endpoints: %w", err)
    }

    var references []string
    for _, ep := range endpoints.Items {
        for _, entry := range ep.Spec.Endpoints {
            for _, be := range entry.Backends {
                if be.PolicyRef != nil && be.PolicyRef.Name == policy.Name {
                    references = append(references, ep.Name)
                    break
                }
            }
        }
    }

    if len(references) > 0 {
        return nil, field.Forbidden(
            field.NewPath("metadata", "name"),
            fmt.Sprintf("policy is referenced by endpoints: %s", strings.Join(references, ", ")),
        )
    }
    return nil, nil
}
```

**KrakenDAutoConfig:**

```go
func (v *AutoConfigValidator) ValidateCreate(
    ctx context.Context, ac *v1alpha1.KrakenDAutoConfig,
) (admission.Warnings, error) {
    var errs field.ErrorList

    // gatewayRef must exist
    gw := &v1alpha1.KrakenDGateway{}
    if err := v.Get(ctx, types.NamespacedName{
        Name:      ac.Spec.GatewayRef.Name,
        Namespace: ac.Namespace,
    }, gw); err != nil {
        errs = append(errs, field.NotFound(
            field.NewPath("spec", "gatewayRef", "name"),
            ac.Spec.GatewayRef.Name,
        ))
    }

    // Mutually exclusive OpenAPI sources
    hasURL := ac.Spec.OpenAPI.URL != ""
    hasCM := ac.Spec.OpenAPI.ConfigMapRef != nil
    if hasURL && hasCM {
        errs = append(errs, field.Invalid(
            field.NewPath("spec", "openapi"),
            "both",
            "url and configMapRef are mutually exclusive",
        ))
    }
    if !hasURL && !hasCM {
        errs = append(errs, field.Required(
            field.NewPath("spec", "openapi"),
            "one of url or configMapRef is required",
        ))
    }

    // configMapRef requires hostMapping
    if hasCM && !hasURL {
        if ac.Spec.URLTransform == nil || len(ac.Spec.URLTransform.HostMapping) == 0 {
            errs = append(errs, field.Required(
                field.NewPath("spec", "urlTransform", "hostMapping"),
                "hostMapping is required when using configMapRef (no URL to infer backend host from)",
            ))
        }
    }

    // Periodic trigger requires interval
    if ac.Spec.Trigger == v1alpha1.TriggerPeriodic {
        if ac.Spec.Periodic == nil || ac.Spec.Periodic.Interval.Duration == 0 {
            errs = append(errs, field.Required(
                field.NewPath("spec", "periodic", "interval"),
                "interval is required when trigger is Periodic",
            ))
        }
    }

    // Mutually exclusive auth methods
    if ac.Spec.OpenAPI.Auth != nil {
        if ac.Spec.OpenAPI.Auth.BearerTokenSecret != nil && ac.Spec.OpenAPI.Auth.BasicAuthSecret != nil {
            errs = append(errs, field.Invalid(
                field.NewPath("spec", "openapi", "auth"),
                "both",
                "bearerTokenSecret and basicAuthSecret are mutually exclusive",
            ))
        }
    }

    // additionalEndpointsBasePath validation
    if ac.Spec.AdditionalEndpointsBasePath != "" &&
        !strings.HasPrefix(ac.Spec.AdditionalEndpointsBasePath, "/") {
        errs = append(errs, field.Invalid(
            field.NewPath("spec", "additionalEndpointsBasePath"),
            ac.Spec.AdditionalEndpointsBasePath,
            "must start with '/'"))
    }

    // additionalEndpointsBasePath and urlTransform.addPathPrefix are mutually exclusive
    if ac.Spec.AdditionalEndpointsBasePath != "" &&
        ac.Spec.URLTransform != nil && ac.Spec.URLTransform.AddPathPrefix != "" {
        errs = append(errs, field.Invalid(
            field.NewPath("spec", "additionalEndpointsBasePath"),
            ac.Spec.AdditionalEndpointsBasePath,
            "is mutually exclusive with spec.urlTransform.addPathPrefix; set only one"))
    }

    // additionalEndpoints validation
    seenAdditional := make(map[string]struct{}, len(ac.Spec.AdditionalEndpoints))
    for i, ae := range ac.Spec.AdditionalEndpoints {
        p := field.NewPath("spec", "additionalEndpoints").Index(i)

        // Rule 1: endpoint is required
        if ae.Endpoint == "" {
            errs = append(errs, field.Required(p.Child("endpoint"), "endpoint is required"))
        } else if !strings.HasPrefix(ae.Endpoint, "/") {
            // Rule 2: endpoint must start with "/"
            errs = append(errs, field.Invalid(p.Child("endpoint"), ae.Endpoint,
                "endpoint must start with '/'"))
        }

        // Rule 3: backends and shorthand fields are mutually exclusive
        if len(ae.Backends) > 0 && (ae.Host != "" || ae.BackendURLPattern != "" || ae.Encoding != "") {
            errs = append(errs, field.Invalid(p, "both",
                "backends and the host/backendUrlPattern/encoding shorthand are mutually exclusive"))
        }

        // Rule 4: no duplicate (endpoint, method) within the list; method defaults to GET
        method := ae.Method
        if method == "" {
            method = "GET"
        }
        key := method + " " + ae.Endpoint
        if _, dup := seenAdditional[key]; dup {
            errs = append(errs, field.Duplicate(p, key))
        }
        seenAdditional[key] = struct{}{}
    }

    return nil, errs.ToAggregate()
}
```

### Webhook Registration

```go
func SetupWebhooks(mgr ctrl.Manager) error {
    if err := ctrl.NewWebhookManagedBy(mgr).
        For(&v1alpha1.KrakenDGateway{}).
        WithValidator(&GatewayValidator{Client: mgr.GetClient()}).
        Complete(); err != nil {
        return err
    }
    // ... repeat for Endpoint, Policy, AutoConfig
    return nil
}
```

---

## 13. AutoConfig Subsystem

**Package:** `internal/autoconfig/`

The autoconfig subsystem implements the OpenAPI-to-endpoint pipeline described in operator architecture §16. The CUE evaluation engine replaces discrete parse/transform/merge stages with a single declarative evaluation that outputs `KrakenDEndpointSpec` objects.

### Fetcher

**File:** `internal/autoconfig/fetcher.go`

```go
type Fetcher interface {
    Fetch(ctx context.Context, source FetchSource) (*FetchResult, error)
}

type FetchSource struct {
    URL               string
    ConfigMapRef      *v1alpha1.ConfigMapKeyRef
    Auth              *v1alpha1.AuthConfig
    AllowClusterLocal bool
}

type FetchResult struct {
    Data     []byte
    Checksum string // SHA-256 of raw bytes
}
```

The fetcher implements SSRF mitigations from operator architecture §16:

```mermaid
flowchart TD
    A[Receive URL] --> B{Scheme http or https?}
    B -->|No| Z[Reject: unsupported scheme]
    B -->|Yes| C[Resolve DNS]
    C --> D[Normalize: IPv4-mapped IPv6<br/>to IPv4 form]
    D --> E{Loopback?<br/>127.0.0.0/8 or ::1}
    E -->|Yes| Z1[Reject: loopback]
    E -->|No| F{Link-local?<br/>169.254.0.0/16 or fe80::/10}
    F -->|Yes| Z2[Reject: link-local / metadata]
    F -->|No| G{ULA? fc00::/7}
    G -->|Yes| Z3[Reject: IPv6 ULA]
    G -->|No| H{allowClusterLocal?}
    H -->|No| I{RFC 1918 private?}
    I -->|Yes| Z4[Reject: private range]
    I -->|No| J[Allow: make request]
    H -->|Yes| J
    J --> K[Follow redirects<br/>max depth 5]
    K --> L[Apply same IP checks<br/>to each redirect Location]
    L --> M{All redirects pass?}
    M -->|Yes| N[Return response body]
    M -->|No| Z5[Reject: redirect to<br/>blocked address]
```

**IP normalization for IPv4-mapped IPv6:**

```go
func normalizeIP(ip net.IP) net.IP {
    if v4 := ip.To4(); v4 != nil {
        return v4
    }
    return ip
}
```

**ConfigMap source:** When `configMapRef` is set, the fetcher reads the spec directly from the Kubernetes API via the injected `client.Client`, bypassing HTTP entirely.

### CUE Evaluator

**File:** `internal/autoconfig/cue_evaluator.go`

The CUE evaluator replaces the discrete Parser and Transformer stages. It uses the `cuelang.org/go/cue` Go library to evaluate CUE definitions against OpenAPI spec data, producing `KrakenDEndpointSpec` objects directly.

```go
type CUEEvaluator interface {
    Evaluate(ctx context.Context, input CUEInput) (*CUEOutput, error)
}

type CUEInput struct {
    SpecData       []byte                     // fetched OpenAPI spec (JSON or YAML)
    SpecFormat     v1alpha1.SpecFormat        // json, yaml, or auto-detect
    DefaultDefs    map[string]string          // default CUE definitions (filename → content)
    CustomDefs     map[string]string          // custom CUE definitions (filename → content); may be nil
    Defaults       *v1alpha1.EndpointDefaults // CR-level defaults
    Overrides      []v1alpha1.OperationOverride // per-operationId overrides
    URLTransform   *v1alpha1.URLTransformSpec // host mapping + path prefix config
    Environment    string                     // CUE _env field value (injected via FillPath)
    ServiceName    string                     // label for the spec data in CUE namespace
}

type CUEOutput struct {
    Entries      []v1alpha1.EndpointEntry
    OperationIDs map[string]string   // keyed by "path:method" → operationId; used by Generator for naming and dedup
    Tags         map[string][]string // keyed by "path:method", used for tag-based filtering before final output
    Warnings     []string            // non-fatal CUE evaluation warnings
}
```

**Evaluation pipeline:**

`loadDefinitions` compiles multiple CUE definition files into a single unified `cue.Value`:

```go
func (e *cueEvaluator) loadDefinitions(
    cueCtx *cue.Context, defs map[string]string,
) cue.Value {
    var unified cue.Value
    for filename, content := range defs {
        val := cueCtx.CompileString(content, cue.Filename(filename))
        if !unified.Exists() {
            unified = val
        } else {
            unified = unified.Unify(val)
        }
    }
    return unified
}
```

```go
func (e *cueEvaluator) Evaluate(ctx context.Context, input CUEInput) (*CUEOutput, error) {
    cueCtx := cuecontext.New()

    // 1. Normalize spec data to JSON (cue.Context.CompileBytes only accepts CUE/JSON)
    specJSON, err := e.normalizeToJSON(input.SpecData, input.SpecFormat)
    if err != nil {
        return nil, fmt.Errorf("normalizing spec to JSON: %w", err)
    }

    // 2. Import OpenAPI spec as CUE data
    specValue := cueCtx.CompileBytes(specJSON,
        cue.Filename(input.ServiceName+".json"),
    )
    if specValue.Err() != nil {
        return nil, fmt.Errorf("compiling OpenAPI spec as CUE: %w", specValue.Err())
    }

    // 3. Load default CUE definitions with environment value injection
    // Note: built-in functions (strings, list, etc.) are available by default
    // in cue/cuecontext v0.11.x — no InferBuiltins option needed.
    unified := e.loadDefinitions(cueCtx, input.DefaultDefs)
    // Inject environment value into the hidden CUE field _env.
    // CUE definitions reference _env for per-environment host resolution
    // (e.g., #internalHost[_env]). This uses FillPath rather than CUE build
    // tags (@tag) because the operator uses cue/cuecontext directly, not
    // cue/load which is the only API that supports @tag() injection.
    unified = unified.FillPath(cue.ParsePath("_env"), cueCtx.CompileString(
        fmt.Sprintf("%q", input.Environment),
    ))

    // 4. Load and unify custom definitions (if provided)
    if len(input.CustomDefs) > 0 {
        customValue := e.loadDefinitions(cueCtx, input.CustomDefs)
        unified = unified.Unify(customValue)
    }

    // 5. Unify with spec data, CR overrides, and URL transform config
    unified = unified.FillPath(
        cue.ParsePath(input.ServiceName),
        specValue,
    )
    unified = e.applyOverrides(cueCtx, unified, input)

    // 6. Evaluate to concrete endpoint entries
    if err := unified.Validate(cue.Concrete(true)); err != nil {
        return nil, fmt.Errorf("CUE evaluation failed: %w", err)
    }

    endpointsValue := unified.LookupPath(cue.ParsePath("endpoint"))
    return e.exportEndpointEntries(endpointsValue)
}
```

**Key design points:**

- The default CUE definitions define an `endpoint` output label containing the generated `KrakenDEndpointSpec` objects, keyed by `"path:method"`
- Environment injection via `FillPath("_env", ...)` populates a hidden CUE field that CUE definitions reference for per-environment host resolution (matching KrakenD-SwaggerParse's `#internalHost.dev`/`#internalHost.preprod`/`#internalHost.prod` pattern). This approach is used instead of CUE `@tag()` because the operator evaluates CUE via `cue/cuecontext` (not `cue/load`), and `@tag()` injection is only supported by `cue/load`
- The `urlTransform.hostMapping` from the CR is converted to CUE `#internalHost` constraints; when omitted, the host is auto-inferred from `openapi.url` base address
- CR `overrides` (keyed by `operationId`) are converted to per-path CUE values and unified with the evaluation context, producing the same effect as KrakenD-SwaggerParse's `swagger_overrides.cue` per-path overrides
- CUE constraint violations (type mismatches, missing required fields, conflicts) produce structured errors that map to `KrakenDAutoConfig` status conditions

### Filter

**File:** `internal/autoconfig/filter.go`

```go
type Filter interface {
    Apply(entries []v1alpha1.EndpointEntry, tags map[string][]string, spec v1alpha1.FilterSpec) []v1alpha1.EndpointEntry
}
```

`FilterSpec` is defined in `api/v1alpha1/krakendautoconfig_types.go` (see §3 Nested Spec Types).

Path patterns in `includePaths` and `excludePaths` support trailing `*` as a glob (e.g., `/internal/*` matches `/internal/health` and `/internal/debug/pprof`).

The filter operates on `EndpointEntry` objects (CUE evaluator output) rather than raw `Operation` structs. It matches against the `endpoint` path and `method` fields of each entry.

Tag-based filtering (`includeTags`, `excludeTags`) uses the `tags` map (keyed by `"path:method"`) produced by the CUE evaluator. The CUE definitions extract each operation's OpenAPI tags during evaluation and populate the `CUEOutput.Tags` map. The filter receives this map alongside the entries and uses it to match tag-based include/exclude rules. Tags are not persisted on the `EndpointEntry` type in the CRD.

### Generator

**File:** `internal/autoconfig/generator.go`

```go
type Generator interface {
    Generate(ctx context.Context, input GenerateInput) (*GenerateOutput, error)
}

type GenerateInput struct {
    AutoConfig     *v1alpha1.KrakenDAutoConfig
    Entries        []v1alpha1.EndpointEntry // from CUE evaluator, post-filter
    OperationIDs   map[string]string        // from CUEOutput.OperationIDs; keyed by "path:method"
    GatewayRefName string                   // populates gatewayRef on each generated KrakenDEndpoint
}

type GenerateOutput struct {
    Endpoints         []*v1alpha1.KrakenDEndpoint
    SkippedOperations int
    DuplicateIDs      []string
}
```

The generator wraps each `EndpointEntry` (produced by CUE evaluation and filtering) in a `KrakenDEndpoint` CR with metadata, labels, and owner references. It groups entries by a configurable strategy (default: one CR per entry) and handles naming and duplicate detection; the controller diffs the output against the endpoints it already owns.

**Duplicate operationId detection:**

The generator tracks seen operationIds during the naming step. When the same operationId appears on multiple entries, the first occurrence is used for naming (and included in the output), subsequent duplicates are skipped, their operationId is added to `GenerateOutput.DuplicateIDs`, and `SkippedOperations` is incremented. The autoconfig controller records the count in `status.skippedOperations` and, when the inputs changed since the last successful sync, emits a `DuplicateOperationId` Warning event for each duplicate.

```go
func (g *endpointGenerator) Generate(ctx context.Context, input GenerateInput) (*GenerateOutput, error) {
    seen := map[string]struct{}{} // keyed by operationId
    // ... for each entry:
    //   - derive operationId from CUE output metadata
    //   - if operationId in seen, skip + record duplicate
    //   - else: add to seen, generate KrakenDEndpoint CR
}
```

**Naming convention:**

```go
func endpointName(autoconfigName, operationID, method, path string) string {
    if operationID != "" {
        return fmt.Sprintf("%s-%s", autoconfigName, sanitizeName(operationID))
    }
    return fmt.Sprintf("%s-%s-%s", autoconfigName, strings.ToLower(method), sanitizePath(path))
}

func sanitizeName(s string) string {
    s = strings.ToLower(s)
    s = strings.Map(func(r rune) rune {
        if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
            return r
        }
        return '-'
    }, s)
    return strings.Trim(s, "-")
}

func sanitizePath(path string) string {
    path = strings.TrimPrefix(path, "/")
    path = strings.ReplaceAll(path, "/", "-")
    path = strings.ReplaceAll(path, "{", "")
    path = strings.ReplaceAll(path, "}", "")
    return path
}
```

**Labels on generated endpoints:**

```go
labels := map[string]string{
    "gateway.krakend.io/auto-generated": "true",
    "gateway.krakend.io/autoconfig":     ac.Name,
}
```

### Additional Endpoints

**File:** `internal/autoconfig/additional.go`

`BuildAdditionalEntries` and `MergeAdditional` implement the additional-endpoints pipeline. `ApplyURLTransformToEntries` (exported from `cue_evaluator.go`) applies the same `urlTransform` that spec-derived endpoints receive.

```go
// BuildAdditionalEntries synthesizes AdditionalEndpoint specs into full
// EndpointEntry values. defaultHost is derived from spec.openapi.url.
func BuildAdditionalEntries(
    specs     []v1alpha1.AdditionalEndpoint,
    defaults  *v1alpha1.Defaults,
    defaultHost string,
) []v1alpha1.EndpointEntry

// MergeAdditional combines spec-derived (base) entries with synthesized
// additional entries. When an additional entry has the same endpoint+method
// as a base entry, the additional entry replaces it and its key is returned
// in `replaced` (callers emit an AdditionalEndpointOverride warning event).
func MergeAdditional(
    base, additional []v1alpha1.EndpointEntry,
) (combined []v1alpha1.EndpointEntry, replaced []string)
```

**Synthesis rules:**

- `method` defaults to `"GET"` when omitted.
- When `backends` is empty, a single `BackendSpec` is synthesized from `host` (default: `defaultHost`), `backendUrlPattern` (default: `endpoint`), the backend `method`, and `encoding`.
- When `encoding: no-op` is set on the shorthand and `outputEncoding` is not explicitly set, the endpoint `outputEncoding` is also set to `"no-op"`.
- When `inheritDefaults: true`, `spec.defaults` are applied fill-only (the entry's explicit fields win). This flag defaults to `false` so health routes do not inadvertently inherit e.g. a JWT validator.

**URL transform (Option B):**

`ApplyURLTransformToEntries` applies the `urlTransform` spec (host mapping + `stripPathPrefix` / `addPathPrefix`) to each additional entry's endpoint path and backend hosts in the same way spec-derived entries are transformed. The backend `urlPattern` is intentionally left untouched. This function is called BEFORE `MergeAdditional` so collision keys (`endpoint:method`) align with the already-transformed spec-derived entries.

```go
// ApplyURLTransformToEntries applies a URLTransformSpec to each entry in the
// slice. A nil transform is a no-op. The backend urlPattern is not transformed.
func ApplyURLTransformToEntries(
    entries   []v1alpha1.EndpointEntry,
    transform *v1alpha1.URLTransformSpec,
)
```

**Base-path scoping:**

After the URL transform is applied, each additional endpoint's public path is scoped under the application's base path. The controller resolves the base in this order:

1. `spec.additionalEndpointsBasePath` — explicit manual override; skips derivation.
2. `urlTransform.addPathPrefix` — when set, the URL transform already prepended the prefix, so no further scoping is performed (base is treated as resolved).
3. `DeriveBasePath(filtered)` — auto-derived from the generated (filtered) entries: compute the segment-level longest common prefix of each entry's parent directory (path minus its last segment). Returns `""` when no common parent exists (e.g. a root-level endpoint `/health`, divergent top-level paths, or an empty entry list).

If none of these resolves to a non-empty string, the controller fails the sync with `AdditionalEndpointScopeFailed`.

```go
// DeriveBasePath returns the segment-level longest common prefix of each
// generated endpoint's parent directory. Returns "" when indeterminate.
func DeriveBasePath(entries []v1alpha1.EndpointEntry) string

// ScopeAdditionalEntries prepends base to each entry's public Endpoint
// unless it already starts with base. Backend urlPattern is untouched.
// A "" base is a no-op.
func ScopeAdditionalEntries(additional []v1alpha1.EndpointEntry, base string)
```

**Controller injection order:**

```
build (BuildAdditionalEntries)
  → URL-transform (ApplyURLTransformToEntries)
  → resolve base path (manual → addPathPrefix → DeriveBasePath)
  → scope public paths (ScopeAdditionalEntries)     ← backend urlPattern untouched
  → merge (MergeAdditional)
  → emit AdditionalEndpointOverride Warning for each replaced key (only when inputs changed)
  → generate (Generator)
```

Additional endpoints bypass `filter` and `overrides` (they carry no `operationId`).

---

## 14. Utility Packages

**Package:** `internal/util/`

### Hash Utility

**File:** `internal/util/hash.go`

```go
func SHA256Hex(data []byte) string {
    h := sha256.Sum256(data)
    return hex.EncodeToString(h[:])
}

func PluginChecksum(configMaps []corev1.ConfigMap, ociTags []string) string {
    h := sha256.New()
    // Sort ConfigMap names for determinism
    sort.Slice(configMaps, func(i, j int) bool {
        return configMaps[i].Name < configMaps[j].Name
    })
    for _, cm := range configMaps {
        keys := make([]string, 0, len(cm.BinaryData))
        for k := range cm.BinaryData {
            keys = append(keys, k)
        }
        sort.Strings(keys)
        for _, k := range keys {
            h.Write(cm.BinaryData[k])
        }
    }
    sort.Strings(ociTags)
    for _, tag := range ociTags {
        h.Write([]byte(tag))
    }
    return hex.EncodeToString(h.Sum(nil))
}
```

### License Parser

**File:** `internal/util/license.go`

```go
type LicenseParser interface {
    Parse(data []byte) (*LicenseInfo, error)
}

type LicenseInfo struct {
    NotAfter time.Time
    Subject  string
}

type x509LicenseParser struct{}

func NewX509LicenseParser() LicenseParser {
    return &x509LicenseParser{}
}

func (p *x509LicenseParser) Parse(data []byte) (*LicenseInfo, error) {
    block, _ := pem.Decode(data)
    if block == nil {
        return nil, fmt.Errorf("no PEM block found in license data")
    }
    cert, err := x509.ParseCertificate(block.Bytes)
    if err != nil {
        return nil, fmt.Errorf("parsing X.509 certificate: %w", err)
    }
    return &LicenseInfo{
        NotAfter: cert.NotAfter,
        Subject:  cert.Subject.CommonName,
    }, nil
}
```

---

## 15. Dependency Injection and Interfaces

All external dependencies are abstracted behind interfaces, injected via struct fields, and wired in `main.go`.

### Interface Summary

| Interface | Package | Purpose | Production Implementation |
|---|---|---|---|
| `Renderer` | `internal/renderer` | Build `krakend.json` from CRD state | `renderer.configRenderer` |
| `Validator` | `internal/renderer` | Validate rendered config via `krakend check -t -n -c` | `renderer.KrakenDValidator` |
| `CommandExecutor` | `internal/renderer` | Execute shell commands (krakend check) | `renderer.KrakenDExecutor` |
| `Fetcher` | `internal/autoconfig` | Fetch OpenAPI specs (HTTP + ConfigMap) | `autoconfig.httpFetcher` |
| `CUEEvaluator` | `internal/autoconfig` | Evaluate CUE definitions + OpenAPI spec → `EndpointEntry` objects | `autoconfig.cueEvaluator` |
| `Filter` | `internal/autoconfig` | Include/exclude operations | `autoconfig.operationFilter` |
| `Generator` | `internal/autoconfig` | Endpoint entries → `KrakenDEndpoint` CRDs with metadata | `autoconfig.endpointGenerator` |
| `LicenseParser` | `internal/util` | Parse X.509 license certificates | `util.x509LicenseParser` |
| `clock.Clock` | `k8s.io/utils/clock` | Time abstraction for license checks and periodic reconcile scheduling | `clock.RealClock` |
| `client.Client` | `sigs.k8s.io/controller-runtime` | Kubernetes API client | Manager's cached client |
| `record.EventRecorder` | `client-go/tools/record` | Kubernetes event emission | Manager's event recorder |

### Test Doubles

Each interface has a corresponding test fake in the `*_test.go` files adjacent to the consuming package:

```go
type fakeRenderer struct {
    result *renderer.RenderOutput
    err    error
}

func (f *fakeRenderer) Render(input renderer.RenderInput) (*renderer.RenderOutput, error) {
    return f.result, f.err
}
```

For the Kubernetes client, tests use the controller-runtime `fake.NewClientBuilder()`:

```go
client := fake.NewClientBuilder().
    WithScheme(scheme).
    WithObjects(gateway, endpoint1, endpoint2).
    WithStatusSubresource(&v1alpha1.KrakenDGateway{}).
    Build()
```

---

## 16. Error Handling Strategy

### Error Categories

| Category | Handling | Example |
|---|---|---|
| Transient API errors | Return `error` from `Reconcile` — controller-runtime retries with backoff | Network timeout reading Secret |
| Permanent validation errors | Set status condition, emit event, return `nil` (no retry) | Config fails `krakend check -t -n -c` |
| Missing prerequisites | Set status condition, return `nil` with `RequeueAfter` | License Secret not yet synced |
| Programming errors | Panic (should never reach production) | Nil pointer on required field that passed webhook validation |
| Validator unavailable (binary missing, timeout, killed, temp-file I/O, validation copy not prepared) | Set `ConfigValid=Unknown` with reason `ValidatorUnavailable`, emit one Warning event, leave `Ready` Unknown and the serving phase and applied config unchanged, return `error` — controller-runtime retries with backoff | `krakend check -t -n -c` hits its deadline or `/usr/local/bin/krakend` is missing |
| AutoConfig spec/CUE/unmatched-override/scope failures | `Periodic`: `RequeueAfter: spec.periodic.interval`; `OnChange`: return `error` for backoff | `SpecFetchFailed`, `CUEEvaluationFailed`, `UnmatchedOverride`, `AdditionalEndpointScopeFailed` — includes a failed external `$ref` fetch/decode, which fails closed as `SpecFetchFailed` instead of falling back to the raw spec |
| AutoConfig endpoint write failures | Return `error` for backoff regardless of trigger (a `Periodic` AutoConfig does not wait for `spec.periodic.interval`) | `EndpointReconcileFailed` |
| AutoConfig status/endpoint write conflicts | Quiet `RequeueAfter: 1s` — no error log, no event, no status change | Stale-cache `Conflict` on a successful sync's status write, or `Conflict`/`AlreadyExists` on an endpoint write; a failed sync whose status write conflicts keeps its failure row's handling, with no event |

### Error Wrapping Convention

All errors are wrapped with context using `fmt.Errorf` with `%w`:

```go
if err := r.Client.Get(ctx, key, secret); err != nil {
    return ctrl.Result{}, fmt.Errorf("getting license secret %s: %w", key, err)
}
```

### Sentinel Errors

Custom error types are used only where callers need to distinguish error categories:

```go
type ValidationError struct {
    Output string
    Err    error
}

func (e *ValidationError) Error() string {
    return fmt.Sprintf("krakend check validation failed: %s", e.Output)
}

func (e *ValidationError) Unwrap() error { return e.Err }
```

### Status Update Failures

When a status update fails after a successful mutation (e.g., ConfigMap updated but status patch fails), the controller returns the error to trigger a retry. On the next reconcile, the checksum comparison detects no change (ConfigMap is already updated), so the controller skips the mutation and retries only the status update. This ensures eventual consistency without duplicate work.

---

## 17. Metrics Implementation

**Registered in:** `cmd/main.go` via `prometheus.MustRegister`
**Instrumented in:** controller `Reconcile` methods

### Metric Definitions

```go
var (
    configRenders = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "krakend_operator_config_renders_total",
        Help: "Total config render attempts",
    })

    configValidationFailures = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "krakend_operator_config_validation_failures_total",
        Help: "Validation failures (broken configs blocked)",
    })

    rollingRestarts = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "krakend_operator_rolling_restarts_total",
        Help: "Rolling deployments triggered",
    })

    licenseExpiryDays = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "krakend_operator_license_expiry_days",
        Help: "Days until EE license expiry",
    }, []string{"namespace", "name"})

    endpointCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "krakend_operator_endpoint_count",
        Help: "Number of KrakenDEndpoints per gateway",
    }, []string{"namespace", "name"})

    reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "krakend_operator_reconcile_duration_seconds",
        Help:    "Reconciliation loop latency",
        Buckets: prometheus.DefBuckets,
    }, []string{"namespace", "name"})

    dragonflyReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "krakend_operator_dragonfly_ready",
        Help: "1 if Dragonfly is ready, 0 otherwise",
    }, []string{"namespace", "name"})

    gatewayInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "krakend_operator_gateway_info",
        Help: "Gateway metadata labels",
    }, []string{"namespace", "name", "edition", "version"})
)
```

### Instrumentation Points

| Metric | Instrumented In | When |
|---|---|---|
| `config_renders_total` | `GatewayReconciler.Reconcile` | After calling `Renderer.Render` |
| `config_validation_failures_total` | `GatewayReconciler.Reconcile` | When `Validator.Validate` returns `ValidationError` |
| `rolling_restarts_total` | `GatewayReconciler.Reconcile` | After patching Deployment pod template |
| `license_expiry_seconds` | `GatewayReconciler.reconcileLicense` | After parsing license certificate; the series is removed with the gateway |
| `endpoint_count` | `GatewayReconciler.Reconcile` | After listing endpoints for gateway |
| `reconcile_duration_seconds` | `GatewayReconciler.Reconcile` | `defer` at top of Reconcile, observing total duration |
| `dragonfly_ready` | `GatewayReconciler.Reconcile` | After checking Dragonfly CR status |
| `gateway_info` | `GatewayReconciler.Reconcile` | After successful reconcile |

---

## 18. Testing Strategy

### Test Pyramid

```mermaid
graph TB
    subgraph "Test Pyramid"
        E2E["E2E Tests<br/>test/e2e/<br/>Real cluster (kind/k3d)<br/>Full operator + CRDs + KrakenD"]
        INT["Integration Tests<br/>test/integration/<br/>envtest (API server + etcd)<br/>Controllers + webhooks + real API"]
        UNIT["Unit Tests<br/>*_test.go (adjacent)<br/>Pure functions + fakes<br/>Renderer, parser, filter, transformer"]
    end

    E2E --- INT
    INT --- UNIT

    style UNIT fill:#6f6,stroke:#333
    style INT fill:#ff6,stroke:#333
    style E2E fill:#f66,stroke:#333
```

### Unit Tests

**Location:** `*_test.go` files adjacent to the code they test.

Unit tests cover all pure-function logic with no Kubernetes API dependency:

| Package | Key Test Cases |
|---|---|
| `internal/renderer` | Deterministic JSON output; endpoint sorting; extra_config merge precedence; wildcard stripping; plugin block injection; checksum computation |
| `internal/autoconfig` | SSRF rejection (loopback, link-local, ULA, RFC 1918); IPv4-mapped IPv6 normalization; redirect validation; scheme restriction; default definitions produce valid `EndpointEntry` objects; custom definitions unify with defaults; per-environment host resolution via `_env` field injection; CR override application; CUE constraint violation produces structured error; host auto-inference when hostMapping omitted; path prefix strip/add; tag annotation for filter stage; include/exclude paths; include/exclude methods; include/exclude tags; include/exclude operationIds; glob matching; endpoint name generation (with operationId, without); duplicate operationId handling; label assignment; owner reference wiring |
| `internal/util/hash` | SHA-256 consistency; plugin checksum determinism across ConfigMap/OCI ordering |
| `internal/util/license` | X.509 certificate parsing; PEM decoding; expired cert detection; malformed input |
| `internal/resources` | Deployment spec (security context, volumes, probes, rolling update strategy); Service spec; PDB spec; label assignment; image selection logic |

**Example test structure (renderer):**

```go
func TestRender_DeterministicOutput(t *testing.T) {
    gw := testGateway()
    endpoints := []v1alpha1.KrakenDEndpoint{
        testEndpoint("b-endpoint", "GET", "/api/b"),
        testEndpoint("a-endpoint", "GET", "/api/a"),
    }

    r := renderer.New(renderer.Options{})
    out1, err := r.Render(renderer.RenderInput{
        Gateway:   gw,
        Endpoints: endpoints,
        Policies:  map[string]*v1alpha1.KrakenDBackendPolicy{},
    })
    require.NoError(t, err)

    // Reverse input order — output must be identical
    slices.Reverse(endpoints)
    out2, err := r.Render(renderer.RenderInput{
        Gateway:   gw,
        Endpoints: endpoints,
        Policies:  map[string]*v1alpha1.KrakenDBackendPolicy{},
    })
    require.NoError(t, err)

    assert.Equal(t, out1.Checksum, out2.Checksum)
    assert.Equal(t, out1.JSON, out2.JSON)
}
```

### Integration Tests

**Location:** `test/integration/`

Integration tests use envtest to run a real Kubernetes API server and etcd, testing controller logic end-to-end without a real cluster:

```go
func TestGatewayReconciler_CreatesOwnedResources(t *testing.T) {
    ctx := context.Background()

    gw := &v1alpha1.KrakenDGateway{
        ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
        Spec: v1alpha1.KrakenDGatewaySpec{
            Version: "2.9",
            Edition: v1alpha1.EditionCE,
            Config:  v1alpha1.GatewayConfig{Port: 8080},
        },
    }
    require.NoError(t, k8sClient.Create(ctx, gw))

    // Wait for reconciler to create Deployment
    dep := &appsv1.Deployment{}
    require.Eventually(t, func() bool {
        err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), dep)
        return err == nil
    }, 10*time.Second, 250*time.Millisecond)

    // Verify owned resources
    assert.Equal(t, gw.Name, dep.Name)
    assert.True(t, metav1.IsControlledBy(dep, gw))
}
```

**Key integration test scenarios:**

| Scenario | Validates |
|---|---|
| Gateway create → Deployment + Service + ConfigMap + SA + PDB created | Resource builder correctness, owner references |
| Endpoint create → gateway re-reconciles → ConfigMap updated | Endpoint watch, config rendering |
| Endpoint conflict → oldest wins; a loser of every entry gets Accepted=False (EndpointConflict), a loser of some gets Accepted=True (PartiallyAccepted); status.conflicts names the lost entries | Conflict detection logic across endpoints[] entries |
| Policy update → all gateways with referencing endpoints re-queued → ConfigMap updated | `policyToGateways` mapper, namespace-scoped list, re-render |
| Policy delete blocked by referencing endpoint | Webhook DELETE validation |
| Policy create/update with invalid field ranges rejected | Webhook CREATE/UPDATE validation |
| Config validation failure → Error phase, no Deployment update | Validation pipeline, error handling |
| License expiry → CE fallback (image + config change) | License evaluation inside the gateway reconcile |
| EE recovery → restored image + full config | License restoration flow |
| Gateway deletion → orphaned endpoints marked Detached | Endpoint controller gateway watch, Detached phase |
| AutoConfig create → generated endpoints | AutoConfig pipeline end-to-end |
| AutoConfig periodic re-sync | RequeueAfter behavior |
| Webhook rejects invalid CRs | All webhook validation rules |

### End-to-End Tests

**Location:** `test/e2e/`

E2E tests run against a real Kubernetes cluster (kind or k3d) with all CRDs installed and the operator running:

```go
func TestE2E_FullGatewayLifecycle(t *testing.T) {
    // 1. Create KrakenDGateway
    // 2. Create KrakenDEndpoints
    // 3. Verify krakend.json ConfigMap contents
    // 4. Verify Deployment is running
    // 5. Verify Service is reachable
    // 6. Update endpoint → config changes, rolling restart
    // 7. Delete endpoint → config changes, rolling restart
    // 8. Delete gateway → all owned resources cleaned up
}
```

### envtest Suite Setup

```go
var (
    testEnv   *envtest.Environment
    k8sClient client.Client
    ctx       context.Context
    cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
    ctx, cancel = context.WithCancel(context.Background())

    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{
            filepath.Join("..", "..", "config", "crd", "bases"),
        },
        WebhookInstallOptions: envtest.WebhookInstallOptions{
            Paths: []string{filepath.Join("..", "..", "config", "webhook")},
        },
    }

    cfg, err := testEnv.Start()
    if err != nil {
        panic(err)
    }

    // Register scheme, create client, start manager with controllers
    // ...

    code := m.Run()
    cancel()
    testEnv.Stop()
    os.Exit(code)
}
```

### Coverage Requirements

- **Unit tests:** ≥ 85% line coverage per package
- **Integration tests:** Cover every reconciliation path and webhook rule
- **E2E tests:** Cover the critical user journey (create gateway → add endpoints → update → delete)

---

## 19. Build and Packaging

### Dockerfile

Multi-stage build that embeds the KrakenD CE binary for config validation:

```dockerfile
# Stage 1: Build operator binary
FROM golang:1.26-alpine AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o manager cmd/main.go

# Stage 2: Extract KrakenD CE binary for config validation
ARG KRAKEND_VERSION=2.13
FROM krakend:${KRAKEND_VERSION} AS krakend

# Stage 3: Final distroless image
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=krakend /usr/bin/krakend /usr/local/bin/krakend
USER 65532:65532
ENTRYPOINT ["/manager"]
```

### Makefile Targets

```makefile
.PHONY: generate manifests test lint build docker-build

generate:                         ## Run code generators (deepcopy, CRD manifests)
	controller-gen object paths="./api/..."
	controller-gen rbac:roleName=krakend-operator-manager crd webhook \
		paths="./..." output:crd:artifacts:config=config/crd/bases

manifests: generate               ## Generate CRD and RBAC manifests

test:                             ## Run unit + integration tests
	go test -v -race -coverprofile=coverage.out ./...

lint:                             ## Run linter
	golangci-lint run -c .github/.golangci.yml

build:                            ## Build operator binary
	go build -o bin/manager cmd/main.go

docker-build:                     ## Build Docker image
	docker build -t krakend-operator:latest .
```

### Go Module

```
module github.com/mycarrier-devops/krakend-operator

go 1.26

require (
    k8s.io/api v0.31.x
    k8s.io/apimachinery v0.31.x
    k8s.io/client-go v0.31.x
    k8s.io/utils v0.0.0-...
    sigs.k8s.io/controller-runtime v0.19.x
    cuelang.org/go v0.11.x             // CUE evaluation engine for autoconfig
    github.com/dragonflydb/dragonfly-operator/api ...
    github.com/external-secrets/external-secrets/apis ...
    istio.io/client-go ...
    github.com/prometheus/client_golang ...
    github.com/stretchr/testify ...  // test only
)
```
