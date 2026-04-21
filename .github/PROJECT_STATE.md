# Project State — KrakenD Operator

> **Last Updated:** 2026-04-21
> **Status:** Added OpenAPI export sidecar, post-restart Jobs, AutoConfig external $ref resolver, richer documentation/openapi CUE extraction. All unit tests pass, 0 lint issues.

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
- `validator.go` — KrakenD CLI validation via temp file, PrepareValidationCopy strips EE-only extra_config keys (e.g. `backend/redis`) and wildcard endpoints before CE validation
- `plugins.go` — buildPluginBlock, computePluginChecksum
- 60 tests, 94.1% coverage, 0 lint issues

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
- `krakendgateway_controller.go` — Primary reconciler: gathers endpoints/policies/plugin ConfigMaps → detects Dragonfly state → Renderer.Render → checksum comparison → Validator.Validate → update ConfigMap → reconcileOwnedResources (SA, Service, ConfigMap, PDB, Deployment, HPA, Dragonfly, ExternalSecret, VirtualService via CreateOrUpdate) → inspectDeploymentStatus (replica propagation, ProgressDeadlineExceeded detection, rollout convergence). Mappers: endpointToGateway, policyToGateways, licenseSecretToGateway. Owns() watches for 9 resource types including unstructured external CRDs. `Clock` field for timing. `detectDragonflyState()` sets DragonflyReady condition and `dragonfly_ready` metric. RBAC markers for dragonflydb.io, external-secrets.io, networking.istio.io.
- `krakendendpoint_controller.go` — Validates gateway/policy refs, maintains phase (Pending/Active/Detached/Invalid), sets conditions, emits events. Mapper: gatewayToEndpoints.
- `krakendbackendpolicy_controller.go` — Counts endpoint references (referencedBy), validates CircuitBreaker/RateLimit fields, sets Valid condition. Mapper: endpointToReferencedPolicies. Pure function: validatePolicy with named returns.
- `krakendautoconfig_controller.go` — AutoConfig reconciler: fetch spec → checksum comparison → load CUE definitions → CUE evaluate → filter → generate → diff/create/update/delete KrakenDEndpoint CRs. Phase-based status tracking (Pending/Fetching/Rendering/Synced/Error). Periodic requeue support. ConfigMap watch for CUE definitions changes. Owner-reference-based endpoint lifecycle. `Clock` field for timing.
- `suite_test.go` — Standard Go test helpers (testScheme, fakeClientBuilder, fakeRecorder) replacing Ginkgo+envtest
- `helpers_test.go` — 10 tests for conditionsEqual() covering both-empty, different-length, same-content, ignore-LastTransitionTime, different-status/reason/message/generation, missing-type
- 69 tests across 6 test files, ~84.8% coverage (filtered), 0 lint issues

### Prometheus Metrics (`internal/controller/metrics.go`)
- 8 metrics registered via controller-runtime `metrics.Registry`
- `config_renders_total`, `config_validation_failures_total`, `rolling_restarts_total`
- `license_expiry_seconds` (gauge per gateway), `endpoints` (gauge per gateway)
- `dragonfly_ready` (gauge per gateway, 1 if ready, 0 otherwise)
- `reconcile_duration_seconds` (histogram), `gateway_info` (metadata labels)
- Instrumented in gateway controller Reconcile (including Dragonfly state) and license monitor checkGateway
- `cmd/main.go` — Wires Renderer, Validator, Recorder, Clock into all controller setups; adds LicenseMonitor as manager.Runnable with RealClock and X509LicenseParser; wires AutoConfig controller with Fetcher, CUEEvaluator, Filter, Generator; calls `webhook.SetupWebhooks(mgr)` for admission webhook registration; `LeaderElectionID` set to `krakend-operator-leader`

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
- `embed.go` — `//go:embed cue/*.cue` embeds default CUE definitions in the operator binary; `EmbeddedCUEDefinitions()` returns `map[string]string`
- `cue/defaults.cue` — Embedded default CUE transformation logic, adapted from KrakenD-SwaggerParse endpoints.cue. Transforms OpenAPI specs into EndpointEntry objects with: path rewrite support, no-op output encoding, configurable timeout, auth-conditional header forwarding (Authorization, X-MC-Api-Key, Content-Type), header/query parameter extraction from operation and path-item, rate limiting extra_config (`qos/ratelimit/router`), full OpenAPI documentation extra_config (`documentation/openapi`) with description, summary, tags, operationId, query/param/request/response definitions including `$ref` resolution and `allOf` handling
- Controller falls back to embedded CUE definitions when the default ConfigMap is not found
- Uses `cuelang.org/go` v0.16.0
- 60 tests across 5 test files, ~94% coverage, 0 lint issues

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

### 2026-04-21
- **New gateway feature — OpenAPI export + serve**: `spec.openapi` (KrakenDGateway) runs `krakend openapi export` in an init container and serves `/openapi.json` from a busybox `httpd` sidecar on a configurable port (default 8090). Port is exposed on the gateway Service as named port `openapi` but deliberately NOT added to the Istio VirtualService (local/in-cluster traffic only). Supports `audience`, `legacy`, `skipJsonSchema`, custom sidecar image/resources. New helpers: `buildOpenAPIPieces()` in `internal/resources/deployment.go`, `OpenAPIPort()` in `internal/resources/service.go`.
- **New gateway feature — Post-restart Jobs**: `spec.postRestartJob` creates a Kubernetes Job that runs a user-provided bash script after every config-triggered rolling restart. Job name embeds a 12-char prefix of the config checksum → one Job per unique config revision (idempotent). Supports env vars, envFrom, pod annotations, custom SA/image/resources, backoffLimit, activeDeadlineSeconds, TTL. Only created once the Deployment has converged on the matching checksum (reads `dep.Spec.Template.Annotations["krakend.io/checksum-config"]` + UpdatedReplicas/AvailableReplicas). New builder: `internal/resources/job.go`. New status field: `status.lastPostRestartJobChecksum`. RBAC marker for `batch/jobs` added.
- **AutoConfig external $ref resolver** (`internal/autoconfig/refresolver.go`): `ResolveExternalRefs` walks the fetched OpenAPI spec, fetches every external `$ref` (absolute or relative to baseURL) once (cached), inlines referenced fragments under `components/schemas/<sanitized-name>`, and rewrites the $ref to the local form. Accepts JSON or YAML fetched documents. Failures become warnings (not fatal) logged by the controller. Internal `#/...` refs and ConfigMap-sourced specs pass through unchanged.
- **Richer endpoint-level `documentation/openapi` CUE extraction**: `cue/defaults.cue` now emits `header_definition` (from OpenAPI `parameters` where `in == "header"`), plus `type`/`format`/`enum`/`example`/`examples` on query/param/header definitions, and `description`/`examples` on `request_definition`.
- **Strip upstream `servers`** (`internal/autoconfig/preprocessor.go`): new `StripServers` removes `servers` from the root document, path-items, and operations before CUE evaluation. The KrakenD gateway is the externally-visible server; upstream URLs must not bleed into generated docs/config. Wired in the AutoConfig controller after `ResolveExternalRefs`. Failures are logged and the raw spec is used as fallback. 4 new unit tests.
- Regenerated CRDs (operator, Helm chart, OLM bundle) and deepcopy. Added 21 new unit tests (5 resources, 6 controller, 10 autoconfig). Lint clean. `go test -race ./...` passes for all non-e2e packages.

### 2026-04-09 (continued)
- **Breaking CRD change: Flattened telemetry config** — removed `openTelemetry` wrapper and standalone `prometheus` from `TelemetryConfig`
  - Old: `spec.config.telemetry.openTelemetry.{serviceName,exporters,layers}` + `spec.config.telemetry.prometheus`
  - New: `spec.config.telemetry.{serviceName,exporters,layers}` (directly on TelemetryConfig)
  - Removed types: `OpenTelemetryConfig`, `PrometheusConfig`
  - Renderer (`config.go`): `appendTelemetryConfig` and `buildOpenTelemetryConfig` updated to read directly from `TelemetryConfig`; removed deprecated standalone prometheus warning; removed unused `logf` import
  - Tests updated: `TestBuildGatewayExtraConfig_Telemetry`, `TestBuildOpenTelemetryConfig_FullExporters` use flat structure
  - Rendered JSON output (`telemetry/opentelemetry` extra_config key) remains identical
  - Regenerated: deepcopy, CRD manifests, Helm chart CRDs, OLM bundle CRDs
  - Updated: sample CR (`krakendgateway.yaml`), architecture README example

### 2026-04-09
- Added full webhook infrastructure to Helm chart: webhook Service, ValidatingWebhookConfiguration (4 webhooks for Gateway/Endpoint/Policy/AutoConfig), cert-manager self-signed Issuer+Certificate, cert volume mount in Deployment
- New templates: `webhook-service.yaml`, `validating-webhook-configuration.yaml`, `webhook-certificate.yaml`
- Updated `deployment.yaml`: webhook port (9443), `--webhook-cert-path` arg, cert volume/mount
- Updated `values.yaml`: `webhooks.enabled`, `webhooks.failurePolicy`, `webhooks.certManager.enabled`, `webhooks.caBundle`
- Fixed operator crash: `open /tmp/k8s-webhook-server/serving-certs/tls.crt: no such file or directory` — cert-manager now provisions TLS certs via the Helm chart
- Fixed multi-arch Dockerfile: musl libraries now glob-copied via staging directory instead of hardcoded x86_64 paths (arm64 uses `ld-musl-aarch64.so.1`)

### 2026-04-08 (continued)
- Unified release pipeline: `release.yml` now triggers on push to main, auto-calculates next semver from conventional commits via `mathieudutour/github-tag-action@v6.2`, runs full CI gates (lint, test, build, e2e, helm-lint), then simultaneously releases Docker image to GHCR and Helm chart via chart-releaser-action, and creates a GitHub release with auto-generated changelog
- `ci.yml` changed to PR-only trigger (removes push-to-main, which is now handled by `release.yml`)
- `helm-ci.yml` changed to PR-only trigger
- E2e CI job updated: removed Kind install step, runs `go test` directly (testcontainers manages ephemeral K3s)
- Makefile: removed Kind-based `setup-test-e2e`/`cleanup-test-e2e` targets, simplified `test-e2e` to direct `go test` invocation, removed `KIND` tool variable

### 2026-04-08 (continued)
- Refactored e2e tests from Kind-cluster to testcontainers-go + K3s module for fully ephemeral isolated test environments
- Added `testcontainers-go` v0.41.0, K3s module v0.41.0, docker/docker v28.5.2 dependencies
- `test/e2e/e2e_suite_test.go` — Complete rewrite: BeforeSuite creates K3s container (`rancher/k3s:v1.31.6-k3s1`), extracts kubeconfig, builds operator image, loads image into K3s, installs cert-manager + all CRDs; AfterSuite terminates container
- `test/utils/utils.go` — Added kubeconfig management (`SetKubeconfig`/`GetKubeconfig`), `KUBECONFIG` injection in `Run()`, `CONTAINER_HOST` propagation for podman CLI compatibility, removed Kind image loader
- `test/e2e/e2e_test.go` — Fixed `serviceAccountToken()` to use `utils.Run()` for KUBECONFIG injection
- Fixed `PrepareValidationCopy` in `internal/renderer/validator.go`: now strips EE-only extra_config keys (`backend/redis`) before CE validation. CE JSON schema rejects EE-only keys like `backend/redis` — this was causing validation failures for Dragonfly/Istio gateway tests
- Added `ceUnsupportedExtraConfig` list for keys that must be stripped before CE linter runs
- Added 3 new unit tests: `TestPrepareValidationCopy_StripsEEExtraConfig`, `TestPrepareValidationCopy_StripsEEExtraConfigRemovesEmptyBlock`, `TestPrepareValidationCopy_StripsEEExtraConfigAndWildcard`
- All 8 e2e tests pass: Controller Manager (pod running, metrics, sample CRs), Basic CE Gateway (create, endpoint lifecycle), Dragonfly Gateway, Istio Gateway, Dragonfly+Istio Gateway
- E2e tests require rootful podman machine with SSH tunnel to `/tmp/podman-rootful.sock` (rootless podman can't delegate cgroup v2 for K3s pod sandboxes)
- Required env vars: `DOCKER_HOST=unix:///tmp/podman-rootful.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`

### 2026-04-08
- Increased test coverage from 55.2% (unfiltered) to 84.8% (filtered) — above 80% CI threshold
- CI coverage config: excluded `zz_generated.deepcopy.go` and `cmd/main.go` from coverage threshold, bumped threshold to 80%
- Created `internal/controller/helpers_test.go` with 10 tests for `conditionsEqual()` function
- Added 5 endpoint controller tests: PolicyToEndpoints mapper, NoOpWhenUnchanged, DetachedToActive, MultiplePoliciesDedup
- Added 8 policy controller tests: PolicyRefsFromNonEndpoint, NoPolicies, StatusNoOpWhenUnchanged, InvalidCircuitBreakerInterval/Timeout, InvalidToValid, ValidatePolicy_NilSpecs
- Resolved PR review comments #59-#62: sanitizePath() rune-mapping fix, init()→registerTypes() refactor (eliminates all //nolint:gochecknoinits)
- Fixed KrakenD CE image: `krakend/krakend:{version}` → `krakend:{version}` (CE is top-level Docker Hub, EE is `krakend/krakend-ee:{version}`)
- Bumped golangci-lint from v2.1.0 to v2.11.4 for Go 1.26 compatibility
- All 62 PR review threads resolved

### 2026-04-07 (continued)
- Embedded default CUE definitions in the operator binary via `//go:embed cue/*.cue` (`internal/autoconfig/embed.go`, `internal/autoconfig/cue/defaults.cue`)
- CUE definitions adapted from KrakenD-SwaggerParse `endpoints.cue`: path rewrite, no-op encoding, timeout, auth-conditional headers, rate limiting, full OpenAPI documentation extraction with `$ref`/`allOf`/multipart request body handling and response definitions
- Controller falls back to `autoconfig.EmbeddedCUEDefinitions()` when default ConfigMap not found (`krakendautoconfig_controller.go`)
- CUE design: overridable defaults use `string | *"value"` disjunctions (resolved by `Concrete(true)` via CUE default selection); values used in output through `let` bindings must be plain concrete values to avoid incomplete-value errors
- 5 embedded CUE tests: ReturnsFiles, EvaluatesWithSpec (timeout/auth/extraConfig/headers), EnvironmentInjection, SkipsNonMethodKeys, CustomDefsOverride
- Fixed critical bug: KrakenD binary path `/usr/bin/krakend` → `/usr/local/bin/krakend` in `cmd/main.go` (Dockerfile copies to `/usr/local/bin/`)
- Added `Clock` field to `KrakenDAutoConfigReconciler` for architecture §8 compliance
- Implemented `inspectDeploymentStatus()` method: reads Deployment status, propagates `Replicas`/`ReadyReplicas` to gateway status, detects `ProgressDeadlineExceeded` (sets phase=Error, emits RolloutFailed event), detects rollout convergence (sets Available=True, Progressing=False)
- Added 3 new controller tests: ProgressDeadlineExceeded detection, rollout convergence, Deployment not found
- All 65 unit tests pass (controller: 46, autoconfig: ~55, renderer: 57, etc.), 0 lint issues, `make build` succeeds

### 2026-04-07
- Architecture gap analysis: identified and resolved all critical gaps between architecture spec and implementation
- Wired `webhook.SetupWebhooks(mgr)` in `cmd/main.go` — admission webhooks now registered before manager starts
- Added unstructured `Owns()` watches for Dragonfly, ExternalSecret, VirtualService in gateway controller `SetupWithManager`
- Wired external CRD reconciliation in `reconcileOwnedResources()`: Dragonfly (if enabled), ExternalSecret (if license.externalSecret.enabled), VirtualService (if istio.enabled)
- Added `Clock` field to `KrakenDGatewayReconciler` struct, injected `clock.RealClock{}` in main.go
- Added `detectDragonflyState()` method: reads unstructured Dragonfly CR status, sets `DragonflyReady` condition, instruments `dragonfly_ready` metric, passes `DragonflyState` to renderer
- Added `dragonflyReady` Prometheus gauge metric (per gateway)
- Added RBAC markers for `dragonflydb.io/dragonflies`, `external-secrets.io/externalsecrets`, `networking.istio.io/virtualservices`
- Changed `LeaderElectionID` from `672753c6.krakend.io` to `krakend-operator-leader` per architecture
- Added 8 new controller tests: detectDragonflyState (not enabled, CR not found, disabled), reconcile with Dragonfly, ExternalSecret, VirtualService
- All 37 controller tests pass, 0 lint issues, `make build` succeeds

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
- `test/e2e/e2e_suite_test.go` — testcontainers-go + K3s module: ephemeral K3s cluster per test run, image build + load, cert-manager + CRD install, full teardown. Requires rootful podman machine
- `test/e2e/e2e_test.go` — 8 e2e specs: Controller Manager (pod running, metrics, sample CRs), Basic CE Gateway (create, endpoint lifecycle), Dragonfly Gateway, Istio Gateway, Dragonfly+Istio Gateway
- `test/utils/utils.go` — Shared e2e helpers: kubeconfig management, command execution with KUBECONFIG/CONTAINER_HOST injection, cert-manager install, CRD installers (Dragonfly, Istio, ExternalSecrets)

### OLM Bundle (`operator/bundle/`)
- Generated via `make bundle` with validated CSV, CRDs, RBAC roles
- CSV includes all 4 owned CRDs with descriptions
- InstallModes: OwnNamespace, SingleNamespace, AllNamespaces
- `bundle.Dockerfile` for OLM bundle image builds

### Helm Chart (`charts/krakend-operator/`)
- `Chart.yaml` with kubeVersion >=1.28 constraint
- Templated: Deployment, ClusterRole/Binding, leader-election Role/Binding, ServiceAccount, metrics Service
- Webhook infrastructure: ValidatingWebhookConfiguration (4 webhooks), webhook Service, cert-manager Issuer+Certificate, cert volume mount in Deployment
- Webhooks enabled by default (`webhooks.enabled=true`, `webhooks.certManager.enabled=true`); can be disabled or used with external CA bundle
- CRDs in `crds/` directory (auto-installed by Helm)
- Configurable: image, replicas, resources, security context, probes, affinity, tolerations, webhooks, cert-manager
- Validated: `helm lint` + `helm template` pass

### CI Pipelines (`.github/workflows/`)
- `ci.yml` — PR-only gate: lint (golangci-lint v2.11.4), test (race, coverage >=80%), build, e2e (testcontainers + K3s); triggers on `operator/**` changes
- `helm-ci.yml` — PR-only gate: Helm lint + template; triggers on `charts/**` changes
- `release.yml` — On push to main: runs lint/test/build/e2e/helm-lint gates, then auto-calculates next semver from conventional commits (`mathieudutour/github-tag-action`), builds+pushes multi-arch image to GHCR, releases Helm chart via chart-releaser-action, creates GitHub release with changelog

### Operational Documentation (`docs/`)
- `runbook.md` — Health checks, Prometheus metrics, alerts, gateway lifecycle phases, troubleshooting, scaling, backup/recovery, log analysis
- `upgrade-guide.md` — Pre-upgrade checklist, Helm/kustomize upgrade, CRD updates, rollback, version compatibility
- Root `README.md` — Quick start, install instructions, development guide

## Current Focus

PR #12 (`fix/deepmerge-deepcopy-bug`): Deep merge/deep copy bug fixes, defaults restructuring, BackendDefaults expansion, Devil's Advocate schema review fixes. All unit tests pass (7 packages), 0 lint issues. Ready for commit.

### Recent Changes (PR #12, 2026-07-XX)
- **Defaults restructured**: `spec.defaults` changed from flat `EndpointDefaults` to nested `Defaults{Endpoint, Backend, PolicyRef}`
- **BackendDefaults expanded**: Added `Encoding`, `SD`, `SDScheme`, `DisableHostSanitize`, `InputHeaders`, `InputQueryStrings`, `ExtraConfig` — all non-per-backend-specific fields from KrakenD v2.13 backend schema
- **BackendSpec expanded**: Added `SD`, `SDScheme`, `DisableHostSanitize`, `InputHeaders`, `InputQueryStrings`, `Deny`, `Group`, `Target`, `IsCollection` fields
- **PolicyRef moved**: From `EndpointDefaults`/`BackendDefaults` to `Defaults.PolicyRef` (operator-level concept, not KrakenD schema)
- **New function `applyDefaultPolicyRef()`**: Sets PolicyRef on backends that don't already have one
- **Renderer updated**: `buildBackendJSON()` now serializes all new fields (sd, sd_scheme, disable_host_sanitize, input_headers, input_query_strings, deny, group, target, is_collection)
- **Devil's Advocate review fixes**: Method enum restricted to GET/POST/PUT/PATCH/DELETE, ConcurrentCalls bounded 1-5, EndpointDefaults doc comments added
- **Comprehensive tests added**: Deep merge interaction tests, BackendDefaults field tests (all 7 fields), PolicyRef tests, scenario tests for 3-layer override chains, expanded renderer tests

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
| Embedded CUE definitions with ConfigMap fallback | Default CUE definitions are embedded in the operator binary per architecture §16; controller uses ConfigMap CUE defs when available, falls back to embedded defs when ConfigMap not found |
| CUE adapted from KrakenD-SwaggerParse | Default CUE transformation logic faithfully adapted from the production-vetted KrakenD-SwaggerParse `endpoints.cue`, adapted to produce EndpointEntry objects instead of raw KrakenD JSON |

## Technical Debt / Known Issues

- Architecture §17 specifies `licenseExpiryDays` metric name but `promlinter` enforces Prometheus base-unit convention (seconds). Metric kept as `license_expiry_seconds` per Prometheus best practice.
- Architecture §2 scheme registration shows typed external CRD imports (dragonflyv1alpha1, esv1, istiov1), but implementation uses `unstructured.Unstructured` per architectural decision to avoid heavy dependency chains. Watches use unstructured GVK objects.
- E2e tests require rootful podman machine (rootless can't delegate cgroup v2 for K3s). SSH tunnel: `ssh -i ~/.ssh/rootful-machine -p 41941 -nNT -L /tmp/podman-rootful.sock:/run/podman/podman.sock root@127.0.0.1`
- `ceUnsupportedExtraConfig` list in `validator.go` may need expansion as more EE-only features are supported

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
18. ~~CI pipeline (GitHub Actions) — build, lint, test~~ ✅ — `ci.yml`, `helm-ci.yml`, `release.yml`
19. ~~OLM bundle generation (`make bundle`)~~ ✅ — validated bundle in `operator/bundle/`
20. ~~Operational documentation (runbook, upgrade guide)~~ ✅ — `docs/runbook.md`, `docs/upgrade-guide.md`, `README.md`
