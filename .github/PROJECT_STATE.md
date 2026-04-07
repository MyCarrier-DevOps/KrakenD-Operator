# Project State — KrakenD Operator

> **Last Updated:** 2026-04-06
> **Status:** All §1-§19 implemented; 93.8% autoconfig coverage; envtest integration + e2e tests added

## Overview

Kubernetes operator that manages KrakenD API Gateway instances declaratively via Custom Resources. Built with operator-sdk (kubebuilder v4 layout), controller-runtime v0.21.0, Go 1.26. Four CRDs: KrakenDGateway, KrakenDEndpoint, KrakenDBackendPolicy, KrakenDAutoConfig.

## Implemented Systems

### Scaffold (operator-sdk)
- **operator-sdk v1.42.2** initialized in `operator/` subdirectory
- Domain: `krakend.io`, API Group: `gateway.krakend.io/v1alpha1`
- Module: `github.com/mycarrier-devops/krakend-operator`
- All 4 CRDs scaffolded with placeholder types (default `Foo` field)
- Controllers scaffolded with empty `Reconcile()` stubs
- Full kustomize config tree generated (`config/crd/`, `config/rbac/`, `config/manager/`, etc.)
- `make build` verified — produces `bin/manager`

### Architecture Documents
- `architecture/README.md` — Operator architecture (17 sections), DA-vetted (58 rounds)
- `architecture/application/application-architecture.md` — Application architecture (19 sections), DA-vetted (9 rounds)

### CRD Types (`api/v1alpha1/`)
- All 4 CRDs fully implemented with architecture-defined types
- `krakendgateway_types.go` — KrakenDGatewaySpec/Status, GatewayConfig, RouterConfig, TelemetryConfig, CORSConfig, SecurityConfig, LicenseConfig, PluginsSpec, etc.
- `krakendendpoint_types.go` — KrakenDEndpointSpec with BackendSpec, timeout, headers, querystrings
- `krakendbackendpolicy_types.go` — Rate limiting, circuit breaker, HTTP cache, raw extra_config
- `krakendautoconfig_types.go` — Swagger import automation
- `shared_types.go` — ConfigMapKeyRef, condition constants, Edition enum
- `zz_generated.deepcopy.go` — Generated deepcopy functions
- CRD manifests generated in `config/crd/bases/`

### Utility Packages
- `internal/util/hash/` — SHA256 config/plugin checksum functions, 100% coverage
- `internal/util/license/` — License validation, expiry parsing, 100% coverage

### Renderer (`internal/renderer/`)
- `types.go` — Renderer/Validator interfaces, RenderInput/RenderOutput, Options
- `config.go` — Core Render() method, buildRootConfig, buildGatewayExtraConfig (refactored into 6 helpers), ResolveImage, serializeJSON
- `endpoints.go` — flattenEndpoints (oldest-wins conflict resolution), buildEndpointJSON, buildBackendJSON
- `extra_config.go` — 3-layer merge (raw < typed < inline) for backend/endpoint extra_config
- `validator.go` — KrakenD CLI validation via temp file, PrepareValidationCopy for CE
- `plugins.go` — buildPluginBlock, computePluginChecksum
- 57 tests, 93.9% coverage, 0 lint issues

### Resource Builders (`internal/resources/`)
- `labels.go` — StandardLabels (6 labels), SelectorLabels (2), DragonflyLabels (4)
- `configmap.go` — BuildConfigMap stores rendered krakend.json
- `serviceaccount.go` — BuildServiceAccount with standard labels
- `service.go` — BuildService with ClusterIP, configurable port
- `pdb.go` — BuildPDB with maxUnavailable=1, selector labels
- `hpa.go` — BuildHPA targets Deployment, min/max replicas, CPU metric
- `deployment.go` — BuildDeployment with rolling update (maxSurge=1, maxUnavailable=0), health probes, security context, volume assembly for config/license/plugins. Plugin volumes: single-source (ConfigMap/PVC) and multi-source (OCI init containers + shared emptyDir)
- 23 tests, 100% coverage, 0 lint issues

### Controllers (`internal/controller/`)
- `krakendgateway_controller.go` — Primary reconciler: gathers endpoints/policies/plugin ConfigMaps → Renderer.Render → checksum comparison → Validator.Validate → update ConfigMap → reconcileOwnedResources (SA, Service, ConfigMap, PDB, Deployment, HPA via CreateOrUpdate). Mappers: endpointToGateway, policyToGateways, licenseSecretToGateway. Full RBAC markers.
- `krakendendpoint_controller.go` — Validates gateway/policy refs, maintains phase (Pending/Active/Detached/Invalid), sets conditions, emits events. Mapper: gatewayToEndpoints.
- `krakendbackendpolicy_controller.go` — Counts endpoint references (referencedBy), validates CircuitBreaker/RateLimit fields, sets Valid condition. Mapper: endpointToReferencedPolicies. Pure function: validatePolicy with named returns.
- `krakendautoconfig_controller.go` — AutoConfig reconciler: fetch spec → checksum comparison → load CUE definitions → CUE evaluate → filter → generate → diff/create/update/delete KrakenDEndpoint CRs. Phase-based status tracking (Pending/Fetching/Rendering/Synced/Error). Periodic requeue support. ConfigMap watch for CUE definitions changes. Owner-reference-based endpoint lifecycle.
- `suite_test.go` — Standard Go test helpers (testScheme, fakeClientBuilder, fakeRecorder) replacing Ginkgo+envtest
- 43 tests across 5 test files, ~82% coverage, 0 lint issues

### Prometheus Metrics (`internal/controller/metrics.go`)
- 7 metrics registered via controller-runtime `metrics.Registry`
- `config_renders_total`, `config_validation_failures_total`, `rolling_restarts_total`
- `license_expiry_seconds` (gauge per gateway), `endpoints` (gauge per gateway)
- `reconcile_duration_seconds` (histogram), `gateway_info` (metadata labels)
- Instrumented in gateway controller Reconcile and license monitor checkGateway
- `cmd/main.go` — Wires Renderer, Validator, Recorder into all controller setups; adds LicenseMonitor as manager.Runnable with RealClock and X509LicenseParser; wires AutoConfig controller with Fetcher, CUEEvaluator, Filter, Generator

### External CRD Builders (`internal/resources/`)
- `dragonfly.go` — BuildDragonfly as `unstructured.Unstructured`, DragonflyGVR, DragonflyName, DragonflyServiceDNS, buildResourceRequirements helper
- `virtualservice.go` — BuildVirtualService as `unstructured.Unstructured`, VirtualServiceGVR, StandardLabels, HTTP route with service DNS
- `externalsecret.go` — BuildExternalSecret as `unstructured.Unstructured`, ExternalSecretGVR, ExternalSecretName, license template
- All use `unstructured.Unstructured` to avoid heavy third-party dependency chains
- 12 additional tests in `external_crd_test.go`, 100% package coverage maintained

### AutoConfig Subsystem (`internal/autoconfig/`)
- `filter.go` — Filter interface: Apply with include/exclude rules for paths (glob), tags, methods (case-insensitive), operationIds
- `fetcher.go` — Fetcher interface: ConfigMap and HTTP sources, SSRF mitigations (loopback, link-local, ULA, RFC 1918), dual transport policy (strict/lenient), bearer token and basic auth from Secrets
- `generator.go` — Generator interface: wraps EndpointEntry in KrakenDEndpoint CRs, auto-generated labels, duplicate operationId detection, sanitized naming
- `cue_evaluator.go` — CUEEvaluator interface: multi-file CUE definition loading, JSON/YAML/auto-detect normalization, environment injection via Unify (not FillPath, for hidden label compatibility), custom defs unification, exportEndpointEntries with _operationId and _tags extraction
- Uses `cuelang.org/go` v0.16.0
- 55 tests across 4 test files, 93.8% coverage, 0 lint issues

### License Monitor (`internal/controller/license_monitor.go`)
- `LicenseMonitor` — manager.Runnable goroutine with periodic ticker-based checking
- Checks all EE gateways' license secrets, parses X.509 certificates
- Handles expiry (with/without fallback-to-CE), safety buffer evaluation
- Rate-limited expiring-soon warnings (24h per gateway via in-memory map)
- Recovery detection: clears degraded/expired conditions, triggers reconcile via annotation patch
- `readLicenseSecret` — supports both SecretRef and ExternalSecret convention (`{name}-license`)
- 13 unit tests, 82% controller package coverage

### Webhook Validators (`internal/webhook/`)
- `GatewayValidator` — EE requires license, CE forbids license, mutually exclusive sources, max 1 PVC
- `EndpointValidator` — gatewayRef/policyRef existence checks, conflict warnings via List
- `PolicyValidator` — CircuitBreaker/RateLimit validation, delete protection (blocks if referenced)
- `AutoConfigValidator` — gatewayRef existence, mutually exclusive OpenAPI sources, configMapRef requires hostMapping, Periodic requires interval, mutually exclusive auth
- `SetupWebhooks(mgr)` — registers all 4 validators via ctrl.NewWebhookManagedBy
- All methods use runtime.Object with checked comma-ok type assertions
- 28 unit tests, 84.3% webhook package coverage

## Recent Changes

### 2026-04-06
- Improved autoconfig test coverage from 76.2% to 93.8%: tests for applyAuth (bearer token, basic auth, custom keys, error cases), SSRFSafeTransportWithPolicy (loopback/strict/lenient), applyOverrides (ExtraConfig, nil), IPv6 ULA, pure IPv6 normalization, invalid URL
- Fixed Dockerfile: moved KRAKEND_VERSION ARG to global scope (before first FROM) — was causing 'invalid reference format' during multi-stage build
- Promoted prometheus/client_golang to direct dependency (used in metrics.go)
- Created envtest integration test suite (`test/integration/`): 6 tests covering resource creation, endpoint triggers re-reconcile, owner reference verification, Active/Detached endpoint status, policy referenced-by count
- Added KrakenD CRD lifecycle e2e test: full gateway lifecycle from create → Deployment/Service → endpoint Active → gateway delete → endpoint Detached
- All unit/integration tests pass, 0 new lint issues, `make build` succeeds

### 2025-07-12 (continued)
- Implemented external CRD builders (§11): Dragonfly, VirtualService, ExternalSecret using `unstructured.Unstructured`
- 12 external CRD tests, 100% package coverage
- Implemented AutoConfig subsystem (§13): filter, fetcher, generator, CUE evaluator
- 41 autoconfig subsystem tests, 76.2% coverage
- Fixed CUE hidden label bug: `cue.ParsePath("_spec")` rejects hidden labels in v0.16.0 — switched to `Unify` with compiled CUE strings
- Implemented AutoConfig controller (§8): full reconcile pipeline with phase tracking
- 13 autoconfig controller tests
- Wired AutoConfig controller dependencies in cmd/main.go
- CUE dependency (`cuelang.org/go v0.16.0`) bumped go.mod from go 1.24.0 to go 1.25.0
- Committed in 6 groups: CUE dep, filter+generator, fetcher, CUE evaluator, controller, main.go wiring

### 2025-07-12
- Implemented License Monitor (§9): periodic license checking as manager.Runnable
- 13 license monitor tests covering all states (healthy, expired, degraded, recovery, rate-limited warnings)
- Implemented Webhook Validators (§12): 4 validators for all CRDs
- 28 webhook tests, 84.3% coverage
- Fixed errcheck lint: all type assertions use checked comma-ok pattern
- Wired LicenseMonitor into cmd/main.go with RealClock and X509LicenseParser
- Committed in 3 groups: license monitor, webhooks, main.go wiring

### 2025-07-11
- Implemented 3 controllers (Gateway, Endpoint, Policy) with full reconciliation logic
- 30 controller unit tests, 81.7% coverage, 0 lint issues
- Replaced Ginkgo+envtest test suite with standard Go test helpers using fake client
- Wired Renderer, Validator, Recorder into controller setups in main.go
- Fixed funcorder lint issues (SetupWithManager before private methods)
- Fixed gocritic emptyStringTest and gochecknoinits in main.go
- Committed in 4 groups: endpoint controller, policy controller, gateway controller, test helpers + main.go + RBAC
- Completed resource builders package (`internal/resources/`): 7 source files, 3 test files
- 23 tests, 100% statement coverage, 0 lint issues
- Committed in 3 groups: labels+configmap+sa+service+pdb, hpa+deployment, tests

### 2025-07-10 (earlier sessions)
- Completed renderer package (`internal/renderer/`): 6 source files, 5 test files
- 57 tests, 93.9% coverage, 0 lint issues, committed in 3 groups
- Completed utility packages (hash, license) with 100% coverage
- All 4 CRD types fully implemented and committed with generated deepcopy/manifests
- Scaffolded operator-sdk project in `operator/`

### Testing Infrastructure
- `test/integration/suite_test.go` — envtest setup: starts real API server + etcd, wires Gateway/Endpoint/Policy controllers with noopValidator
- `test/integration/gateway_test.go` — 6 integration tests: resource creation with owner refs, endpoint triggers re-reconcile, owner reference verification, Active/Detached endpoint status, policy referenced-by count
- `test/e2e/e2e_test.go` — Scaffolded by operator-sdk (manager pod + metrics validation) + KrakenD CRD lifecycle test (create gateway → Deployment/Service → create endpoint → Active → delete gateway → Detached)

## Current Focus

All §1-§19 architecture sections fully implemented. All unit, integration, and envtest tests pass. Remaining work is CI pipeline integration and operational documentation.

## Architectural Decisions

| Decision | Rationale |
|---|---|
| `operator/` as Go module root | operator-sdk requires an empty directory; repo root contains `architecture/` |
| operator-sdk over raw kubebuilder | Provides OLM bundle, scorecard, and additional scaffolding |
| Binary name `manager` | Default operator-sdk convention; architecture updated to match |
| `cmd/main.go` (not `cmd/operator/main.go`) | operator-sdk default layout; architecture updated to match |
| Mutate-function pattern for builders | Pure functions that modify objects in place, used with `controllerutil.CreateOrUpdate` |
| `unstructured.Unstructured` for external CRDs | Avoids pulling in heavy third-party Go types (dragonflydb-operator, istio, external-secrets) that upgrade controller-runtime/k8s |
| CUE Unify instead of FillPath for hidden labels | `cue.ParsePath("_spec")` rejects hidden labels in CUE v0.16.0; Unify with compiled CUE strings works correctly |

## Technical Debt / Known Issues

- Scaffolded `test/utils/utils.go` has 13 lint issues (errcheck, gochecknoinits, gocritic) — leave as-is (operator-sdk scaffold)
- `api/v1alpha1/*_types.go` init() functions flagged by gochecknoinits — required by controller-runtime scheme registration
- E2e tests require Kind cluster + Docker image build — CI only

## Next Steps (Not Yet Implemented)

1. ~~Replace placeholder types with architecture-defined types (§3)~~ ✅
2. ~~Implement Gateway controller reconciliation logic (§5)~~ ✅
3. ~~Implement Endpoint controller (§6)~~ ✅
4. ~~Implement Policy controller (§7)~~ ✅
5. ~~Implement AutoConfig controller (§8)~~ ✅
6. ~~Implement configuration rendering pipeline (§10)~~ ✅
7. ~~Implement resource builders (§11)~~ ✅
8. ~~Implement external CRD builders — Dragonfly, VirtualService, ExternalSecret (§11)~~ ✅
9. ~~Implement webhook validation (§12)~~ ✅
10. ~~Implement AutoConfig subsystem (§13)~~ ✅
11. ~~Implement utility packages (§14)~~ ✅
12. ~~Implement License Monitor (§9)~~ ✅
13. ~~Update Dockerfile to golang:1.26 with KrakenD CE binary (§19)~~ ✅
14. ~~Implement Prometheus metrics (§17)~~ ✅
15. ~~Improve autoconfig subsystem test coverage (applyAuth, SSRF transport)~~ ✅ — 93.8% coverage
16. ~~Integration tests with envtest~~ ✅ — 6 tests in `test/integration/`
17. ~~E2e test infrastructure~~ ✅ — KrakenD CRD lifecycle e2e + Dockerfile ARG fix
18. CI pipeline (GitHub Actions) — build, lint, test, e2e with Kind
19. OLM bundle generation (`make bundle`)
20. Operational documentation (runbook, upgrade guide)
