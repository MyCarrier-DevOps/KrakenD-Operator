# Operations Runbook — KrakenD Operator

## Overview

The KrakenD Operator manages KrakenD API Gateway instances on Kubernetes via four Custom Resources:

| CRD | Purpose |
|---|---|
| `KrakenDGateway` | Declares a gateway deployment (edition, replicas, config, license) |
| `KrakenDEndpoint` | Defines a single API endpoint with backends |
| `KrakenDBackendPolicy` | Reusable backend policies (rate limit, circuit breaker, cache) |
| `KrakenDAutoConfig` | Generates endpoints from OpenAPI/Swagger specs |

---

## Health Checks

The operator exposes two probes on port **8081**:

| Path | Purpose |
|---|---|
| `/healthz` | Liveness — restart if failing |
| `/readyz` | Readiness — remove from service if failing. With webhooks enabled it also fails until the pod's webhook server accepts connections (check `webhook`) |

Verify manually:

```bash
kubectl -n krakend-operator-system port-forward deploy/krakend-operator-controller-manager 8081
curl -s http://localhost:8081/healthz  # {"status":"ok"}
curl -s http://localhost:8081/readyz   # {"status":"ok"}
```

---

## Prometheus Metrics

Metrics are exposed on port **8443** (HTTPS). Scrapes need a bearer token
whose identity is bound to the `<fullname>-metrics-reader` ClusterRole;
without it the endpoint answers 403. The chart grants the operator the
TokenReview and SubjectAccessReview permissions this check needs, and creates
a `ServiceMonitor` when `metrics.serviceMonitor.enabled` is true. Every name below carries the
`krakend_operator_` prefix (`krakend_operator_license_expiry_seconds`, and so
on).

The metrics are recorded with OpenTelemetry and served through its Prometheus exporter. Names, labels, help text and histogram buckets are those of earlier releases, no `target_info` series or `otel_scope_*` label is added, and controller-runtime's own metrics (`controller_runtime_*`, `workqueue_*`, `rest_client_*`) are unchanged. One difference: after a gateway is deleted, its `krakend_operator_reconcile_duration_seconds` series stays until the operator restarts, because OpenTelemetry cannot remove a histogram series. Its gauges disappear as before. With an OTLP endpoint configured, the same metrics are also pushed over OTLP every `OTEL_METRIC_EXPORT_INTERVAL` (60 s by default); `OTEL_METRICS_EXPORTER=none` (Helm: `telemetry.otlp.signals.metrics: false`) turns that off.

Key metrics:

| Metric | Type | Description |
|---|---|---|
| `config_renders_total` | Counter | Total config renders |
| `config_validation_failures_total` | Counter | Fresh rejections of a gateway root, a backend policy or an endpoint checked on its own, counted once per change of what the gateway controller checks, not once per reconcile; content that comes back, the same policy on another gateway and an operator restart each count again. The full check of a gateway's whole render is not counted: a config that fails only with its endpoints together shows as gateway_config_valid 0 (ConfigValid=False, CombinedConfigInvalid) instead |
| `rolling_restarts_total` | Counter | Deployment writes that changed the pod template, once per write (a creation does not count; drift in the template the operator reverts does) |
| `license_expiry_seconds` | Gauge | Seconds until license expiry (per gateway) |
| `endpoints` | Gauge | Number of endpoints (per gateway) |
| `dragonfly_ready` | Gauge | Dragonfly readiness (1/0 per gateway) |
| `reconcile_duration_seconds` | Histogram | Reconcile loop duration; a deleted gateway's series stays until the operator restarts |
| `gateway_info` | Gauge | Gateway metadata labels (edition, version); one series per gateway |
| `gateway_config_valid` | Gauge | 1 while the gateway's newest config passed validation, 0 while it is rejected or unjudged (per gateway); removed when the gateway is deleted |
| `gateway_excluded_endpoints` | Gauge | KrakenDEndpoints a gateway leaves out because they fail validation on their own, by `Accepted` reason (`EndpointInvalid`, `PolicyInvalid`); labels `namespace`, `gateway`, `reason`; counted once the exclusion is recorded, including while the gateway cannot apply its newest config; absent while none; removed with the gateway |
| `autoconfig_synced` | Gauge | 1 after an AutoConfig's last sync succeeded, 0 while it's failing or any of its operations is held (`OperationsFailed`) (per AutoConfig); removed when the AutoConfig is deleted |

### Recommended Alerts

```yaml
# License expiring within 7 days
- alert: KrakenDLicenseExpiringSoon
  expr: krakend_operator_license_expiry_seconds < 604800
  for: 1h
  labels:
    severity: warning

# License expired
- alert: KrakenDLicenseExpired
  expr: krakend_operator_license_expiry_seconds <= 0
  for: 5m
  labels:
    severity: critical

# A gateway's newest config cannot be applied: its root fails on its own
# (GatewayRootInvalid), its endpoints fail only together
# (CombinedConfigInvalid), or it could not be validated. It keeps serving the
# last applied one. One bad endpoint no longer fires this: it is excluded
# instead (KrakenDGatewayEndpointsExcluded). A failure only together is not
# counted in config_validation_failures_total, so alert on this gauge.
- alert: KrakenDGatewayConfigRejected
  expr: krakend_operator_gateway_config_valid == 0
  for: 15m
  labels:
    severity: warning

# A gateway leaves out endpoints that fail validation on their own; the rest
# of it is served. Their owners must fix them.
- alert: KrakenDGatewayEndpointsExcluded
  expr: sum by (namespace, gateway) (krakend_operator_gateway_excluded_endpoints) > 0
  for: 10m
  labels:
    severity: warning
  annotations:
    summary: "Gateway {{ $labels.namespace }}/{{ $labels.gateway }} leaves out {{ $value }} endpoint(s) that fail validation"
    description: "The gateway's EndpointsExcluded condition names them: kubectl get krakendgateway {{ $labels.gateway }} -n {{ $labels.namespace }} -o jsonpath='{.status.conditions[?(@.type==\"EndpointsExcluded\")].message}'"

# Reconcile taking too long
- alert: KrakenDSlowReconcile
  expr: histogram_quantile(0.99, rate(krakend_operator_reconcile_duration_seconds_bucket[5m])) > 30
  for: 15m
  labels:
    severity: warning

# AutoConfig hasn't synced — its endpoints are frozen at the last good state,
# or some of its operations are held (OperationsFailed)
- alert: KrakenDAutoConfigNotSynced
  expr: krakend_operator_autoconfig_synced == 0
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: "AutoConfig {{ $labels.namespace }}/{{ $labels.name }} has not synced for 15 minutes; its endpoints are frozen at the last good state, or some operations are held"
```

The gauge is also `0` while any operation is held (`Synced=False`, reason
`OperationsFailed`): the rest of the AutoConfig keeps converging, but the held
operations keep their last endpoints. To tell the two apart, read the `Synced`
reason, or `status.failedOperations`.

The operator exports no metric for `EndpointsReady`. To alert on generated
endpoints that are not serving, alert from your cluster's custom-resource
state tooling on the AutoConfig condition
`status.conditions[?(@.type=="EndpointsReady")].status` being `False` for 15
minutes (the condition's message names up to five endpoints and their reasons).

---

## Tracing

Traces are exported over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` (or a per-signal `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) is set. Helm sets it with `telemetry.otlp.endpoint`, or with `telemetry.otlp.nodeCollector.enabled` to reach a collector on the pod's node. Without an endpoint no trace is exported. The default sampler, `parentbased_always_on`, keeps every new trace; use `OTEL_TRACES_SAMPLER=parentbased_traceidratio` with `OTEL_TRACES_SAMPLER_ARG=0.1` (Helm: `telemetry.traces.sampler` and `telemetry.traces.samplerArg`) to keep one in ten.

Every trace starts at one of these spans:

| Root span | Attributes | Below it |
|---|---|---|
| `reconcile KrakenDGateway` | `k8s.namespace.name`, `k8s.object.name`, `k8s.object.kind`, `k8s.object.generation`, `controller_runtime.reconcile_id` | `configcheck.Gather`, `gateway.license`, `gateway.plugins`, `gateway.dragonfly`, `gateway.render`, `gateway.config` (its `configcheck.*` checks, each `gateway.judge_endpoints` pass with its `configcheck.CheckEndpoint` checks, `gateway.publish_configmap` and `gateway.verify_configmap`), `gateway.acceptance` (`gateway.endpoint_status`), `gateway.core_resources` (`apply serviceaccount`, `apply service`, `apply pdb`), `gateway.infrastructure` (`apply deployment`, `apply job`, `gateway.collect_configmaps`, …), `gateway.status` (with `gateway.status.written`, false when the status was unchanged and nothing was written); a `k8s.discovery` span under the stage that looks an optional CRD up |
| `reconcile KrakenDAutoConfig` | the same | `autoconfig.fetch_spec` (with `autoconfig.fetch`, `HTTP GET` and `autoconfig.resolve_refs`), `autoconfig.cue_definitions`, `autoconfig.evaluate`, `autoconfig.filter`, `autoconfig.generate`, `autoconfig.endpoints` (each `autoconfig.precheck`, with its router-clash check and `autoconfig.judge_candidates`, whose checks each wait in `controller.check_slot`, and each `autoconfig.write_endpoint` or `autoconfig.delete_endpoint`), `autoconfig.status` |
| `reconcile KrakenDEndpoint`, `reconcile KrakenDBackendPolicy` | the same | `endpoint.resolve_refs`, `endpoint.status`; `policy.protection`, `controller.check_slot` (the wait for a controller check slot), `configcheck.CheckPolicy` (with its `krakend check` run, or a memo hit), `policy.status` |
| `admission /validate-…` | HTTP server attributes | `admission.validate <Kind>` (object identity, `k8s.admission.operation`, `k8s.admission.dry_run`, `k8s.admission.uid`, and the outcome below), `admission.structural`, the `configcheck.*` checks; a gateway update's `admission.judge_served`; a policy write's `admission.screen_policy` and `admission.judge_policy`, one of each per gateway that uses the policy |

Each reconcile is a new root: it does not continue the trace of an earlier reconcile or of the admission request that wrote the object.

**Admission outcome.** `admission.validate <Kind>` carries `admission.allowed` (bool) and `admission.code` (int), the status code of the answer: 200 when the request is allowed, the denial's own code when it is denied (422 for rejected fields, 403 for a plain denial), or 500 when the check could not run. `admission.allowed=false` with `admission.code=500` therefore means the request was not decided, not that it was denied. A denial is an answer, not a failure: its span has no error status and records no event. No span carries the denial's text, a warning's text or krakend's output; they would quote the tenant's object, and a denial's output can reveal a gateway root setting to an endpoint's author (see "Endpoint shows `Accepted=False`, reason `EndpointInvalid`" under Troubleshooting). A span whose check could not run is an error with a fixed text ("the admission could not be decided" or "the rules could not be evaluated"). The `admission.structural` spans can also carry the error of a failed lookup, from the API server or the cache; that text names objects but never holds values from a spec.

**`client.address`** on the admission server span is the first value of the request's `X-Forwarded-For` header, which any caller inside the cluster can set. `network.peer.address` is the real peer.

**Config checks.** Each `configcheck.<Method>` span (`CheckRoot`, `CheckGroup`, `CheckEndpoint`, `CheckPolicy`, `CheckRendered`, `Conflicts`, `SameConfig`, `Gather`) names the object it checks: the gateway, or for `CheckPolicy` the policy. The checks that judge a config (`CheckRoot`, `CheckGroup`, `CheckEndpoint`, `CheckPolicy`, `CheckRendered`) carry `configcheck.ok`; `Gather`, `SameConfig` and `Conflicts` render or read and judge nothing, so they do not. A config that fails `krakend check` is an answer, not an error: its span has `configcheck.ok=false` and no error status. Below it, `configcheck.render` is each in-process render, and `configcheck.lint` or `configcheck.validate` is one content check, with `configcheck.slot` (the wait for one of the three validation slots) and `krakend check` (the binary) below it. A content check answered from the operator's memory of recent verdicts has `configcheck.memo_hit=true` and neither child. A content check with `configcheck.memo_hit=false` and no `krakend check` child was either refused before krakend ran by one of the operator's own rules (the Enterprise router rule or the route check; `configcheck.ok=false`), or could not run at all (an error status, for example a `configcheck.slot` wait that reached the deadline). The `krakend check` span carries `process.executable.name`, `process.command_args` (the config file reduced to its base name), `krakend.check.mode` (`lint` or `validate`) and `process.exit.code`; a rejection is a non-zero exit, so that span has an error status reading `exit status N`, N being the exit code, and krakend's output is on no span. When an endpoint fails on its own, the config stage checks the whole render twice: `configcheck.purpose=combined` is the render with every endpoint, `configcheck.purpose=safety_net` the render without the excluded ones. Each `gateway.judge_endpoints` pass says which endpoints it judges: `gateway.judge.pass=suspects`, those the whole render's check left unjudged, or `gateway.judge.pass=masked`, those the applied config leaves an entry of out.

- **Kubernetes API calls** made during any of these are client spans named `k8s <verb> <resource>`, such as `k8s update krakendgateways/status`, with `k8s.verb`, `k8s.resource`, `k8s.namespace.name` and `k8s.object.name`. Their URLs are recorded without user information and with every query value replaced by `REDACTED`.
- **Cache reads.** Every read through the manager's client, whether the cache answers it or not, adds a `k8s.client.get` or `k8s.client.list` event to the span that made it, with `k8s.client.found` (or `error.type`) on a get and `k8s.client.succeeded` on a list. A read the cache answers sends no request and so has no client span; a read of a Secret or ConfigMap, which is never cached, has both. A read through the operator's uncached reader has a client span and no event.
- **The API server's trace.** When the API server is configured for tracing, an admission trace continues the API server's own. If the API server sends an unsampled trace context (flags `00`, for example with a low `samplingRatePerMillion`), `parentbased_always_on` follows it and the whole admission subtree goes unrecorded; that is standard W3C behaviour, not a fault. To record admission requests whatever the API server decided, set `OTEL_TRACES_SAMPLER=always_on` (Helm: `telemetry.traces.sampler=always_on`), or `traceidratio` with `OTEL_TRACES_SAMPLER_ARG` to keep a share of them.
- **Spec hosts.** The trace context is never sent to OpenAPI spec hosts or to the hosts of their `$ref` documents. Each request, and each redirect hop, is its own `HTTP GET` span under `autoconfig.fetch`. A spec URL is recorded, in spans and in errors and the AutoConfig's status, without user information and fragment, with every query value (or bare query key) replaced by `REDACTED`; a URL that cannot be shown safely is recorded as `<unparseable URL>`. A `$ref` with an unsupported scheme fails without echoing the scheme.
- **CRD lookups.** A lookup of an optional CRD (Dragonfly, ExternalSecret, VirtualService) during a reconcile is a `k8s.discovery` span, with `k8s.discovery.kind`, under the stage that needs the kind. client-go sends the discovery request it may need without a context, so the span is the only record of its time. The check at startup that decides which of these kinds to watch is not traced.
- **Not traced:** these record no span and send no trace header:
  - informer list and watch requests, which run outside any reconcile;
  - leader-election lease renewals, about every 2 seconds;
  - the metrics endpoint's TokenReview and SubjectAccessReview for each scrape;
  - RESTMapper discovery requests: client-go sends them without a span context, including the lazy RESTMapper's lookups during a reconcile, so only the operator's own `crdAvailable` lookups are recorded, as the `k8s.discovery` spans above;
  - Kubernetes events: a reconcile raises them, but the event recorder takes no context and writes each event later, so an event is raised inside a reconcile and written asynchronously without a span.

**`k8s.namespace.name` has two meanings.** On the resource (every span, log record and metric the operator exports), it is the operator pod's namespace. On a reconcile, admission or `k8s …` span, it is the namespace of the object the span is about. To find an object's traces, filter on the span attribute, not the resource attribute (in TraceQL, `span.k8s.namespace.name` rather than `resource.k8s.namespace.name`; in a SQL-backed store, the span-attributes column rather than the resource-attributes one). Filtering on the resource attribute matches every trace of the operator, whatever the object.

### Finding the trace of a slow or stuck reconcile

1. In the trace backend, search for spans named `reconcile KrakenDGateway` (or the kind) whose span attributes `k8s.namespace.name` and `k8s.object.name` are the object's, sorted by duration. The waterfall shows which child took the time:
   - `configcheck.slot`: waiting for one of the three validation slots, or `controller.check_slot` for the share of them the AutoConfig and policy controllers hold between them;
   - `krakend check`: the binary itself;
   - a `k8s …` span: the API server;
   - `HTTP GET`: a spec host.
2. A span is exported only when it ends. A reconcile that is still running, or hung, has no root span yet; its finished children appear under a missing parent. Look for those, or start from the logs.
3. From the operator's logs, take the reconcile's `controller_runtime.reconcile_id` (every record of a reconcile carries it as the `reconcileID` attribute) or a record's `TraceID`, and search the backend for it.

A failed step's error is recorded on its span as an error status and an `exception` event. It can be the text of an API server or cache error, which names objects. When the API server refuses a child object the gateway reconcile creates, its refusal text lands on the `apply …` stage span and on the gateway's reconcile root, and can quote values from the gateway's own spec: for a post-restart Job, a `spec.postRestartJob.podLabels` value that is not a valid label value. Those values were written by the gateway's owner; the text never contains another tenant's endpoint or policy content. The `autoconfig.evaluate` and `autoconfig.generate` spans, and the `reconcile KrakenDAutoConfig` root span, carry a fixed description and no `exception` event, not the CUE or generation error: those can quote the spec, and stay in the returned error and its log line. The `autoconfig.write_endpoint` span can carry the API server's refusal of a generated endpoint, which can echo only the endpoint fields the AutoConfig generated. Spans carry no krakend output and no spec values beyond that, but they do name objects, namespaces and redacted URLs: restrict access to the trace backend as you do the operator's logs, which also quote krakend's output.

### From a log line to its trace

A record logged inside a reconcile or an admission request carries `TraceID` and `SpanID`:

```bash
kubectl -n krakend-operator-system logs deploy/krakend-operator-controller-manager \
  | jq -c 'select(.TraceID != "00000000000000000000000000000000") | {t: .Timestamp, sev: .SeverityText, msg: .Body.Value, trace: .TraceID, span: .SpanID}'
```

Open the `TraceID` in the trace backend to see the record's place in the waterfall. Without an OTLP endpoint, traces use a no-op provider and `TraceID` is all zeros, unless the API server propagated one into an admission request. A sampler that keeps only some traces still gives every record a `TraceID`: a record whose `TraceFlags` is `00` belongs to a trace the sampler dropped, which the backend does not hold.

---

## Gateway Lifecycle

### Phases

`status.phase` is derived from the `Ready` condition on every reconcile and
kept for compatibility; alert and gate on `Ready` instead.

| Phase | Ready | Meaning |
|---|---|---|
| `Pending` | `Unknown` | No configuration has been validated yet |
| the serving phase (`Pending` before any rollout, `Deploying` while a rollout is in progress or the Deployment is not available, `Running` otherwise) | `Unknown`, reason `ValidatorUnavailable` | The validator could not run; the last applied configuration keeps serving and validation is retried with backoff |
| `Deploying` | `False` | A rollout is in progress, or the Deployment has not reported available replicas yet |
| `Running` | `True` | Configuration applied, the Deployment is available and the applied config is rolled out to all replicas |
| `Degraded` | `False` | EE license expired or in the pre-expiry window; falling back to or running on CE (`LicenseDegraded=True` or `CEFallbackApplied=True`; only `LicenseDegraded` until the CE render is applied) |
| `Error` | `False` | Configuration rejected (`ConfigValid=False`), a plugin ConfigMap missing (`PluginsResolved=False`), an existing object the gateway will not take over (`ResourcesControlled=False`), rollout failed or the Deployment lost availability (`Available=False`), or license expired without CE fallback |

`Rendering` and `Validating` stay in the CRD enum only so stored objects keep
validating; the operator does not write them.

The Deployment rolls with `maxUnavailable: 0`, so any replica that stops being
available outside a rollout (an HPA scale-up whose new pod is not ready yet, a
pod eviction) makes the Deployment report `Available=False`
(`MinimumReplicasUnavailable`). The gateway mirrors it: `Ready` is briefly
`False` and the phase `Error` until the replica is available again.

### Common Conditions

| Condition | Meaning |
|---|---|
| `Ready` | Summary, written only by the gateway controller. `True` (`Ready`): the configuration is applied, the Deployment is available and the applied config is rolled out to all replicas. Otherwise its reason names the first thing that blocks it. `False`: `GatewayRootInvalid`, `CombinedConfigInvalid`, `LicenseExpiredNoFallback`, `ConfigMapNotFound`, `ResourceNotControlled`, the Deployment's reason when `Available` is `False` (`RolloutFailed`, `MinimumReplicasUnavailable`), `EEFeaturesStripped` or `LicenseFallbackCE` while the gateway runs CE as a fallback, `ConfigDeployed` or `DeploymentUpdated` while a rollout is in progress, `AwaitingAvailability` while the Deployment reports no available replicas. `Unknown`: `Pending` (no configuration validated yet), `ValidatorUnavailable` or `ConfigPublishFailed` |
| `ConfigValid` | `True` (`ConfigApplied`): the rendered config passed validation and is the applied config. `False` (`GatewayRootInvalid`): the gateway root fails on its own, so nothing is judged; `False` (`CombinedConfigInvalid`): the endpoints that pass krakend check on their own fail it together (any endpoint that fails on its own is excluded first). In both, the last applied config keeps serving. `Unknown` (`ValidatorUnavailable`): krakend check could not run; retried with backoff. `Unknown` (`ConfigPublishFailed`): the newest render passed validation but its ConfigMap could not be published, or the applied config's ConfigMap does not hold the config; retried with backoff |
| `EndpointsExcluded` | `True` (`InvalidEndpointsExcluded`) while the gateway leaves out endpoints that fail validation on their own; the message counts them and names the first 10. It reads `… fail validation and are not served: …` while the gateway serves its newest config, and `… fail validation and will not be served when the gateway next applies its config: …` while `ConfigValid` is not `True` (the last applied config, which may still hold them, keeps serving). The wording switch is a message change, so a rejected root or a validator outage emits one extra `InvalidEndpointsExcluded` Warning, and another when the gateway applies again. Absent otherwise. `ConfigValid` and `Ready` are not affected by the exclusions themselves |
| `PluginsResolved` | `True` (`ConfigMapsFound`) when every plugin ConfigMap exists; `False` (`ConfigMapNotFound`) naming the missing ones while the Deployment is held. Absent without ConfigMap plugin sources |
| `ResourcesControlled` | `True` (`ResourcesControlled`) when no object the gateway wrote this pass was refused (it keeps its last value while the pass failed otherwise or held the Deployment); `False` (`ResourceNotControlled`) naming each existing object it leaves alone and what to do (see *Gateway reports `ResourceNotControlled`*) |
| `Available` | The Deployment is available; `False` when it loses its minimum replicas (for example all pods crash-looping) or its rollout fails |
| `Progressing` | A rollout is in progress: the Deployment was created, its pod template was written (a config, image, plugin or license change, but also resources, probes or drift the operator reverted), or old pods remain beside updated ones. It stays `True` until the Deployment has observed the change and every replica is updated and available. A replica change alone (an HPA scale) does not raise it, though the brief `Available=False` a scale-up can cause is still mirrored |
| `CEFallbackApplied` | `True` (`EEFeaturesStripped`) while the applied config is the CE-fallback render; the message lists the Enterprise-only features it removed. Absent otherwise |
| `DragonflyReady` | DragonflyDB instance is operational; `False` (`ResourceNotControlled`) while the Dragonfly named like the gateway's is one the gateway refuses (see *Gateway reports `ResourceNotControlled`*) |
| `IstioConfigured` | VirtualService has been reconciled; `False` (`ResourceNotControlled`) while the gateway refuses the Service the VirtualService would route to |
| `LicenseValid` | EE license state: `True` (`LicenseOK`), `True` (`LicenseExpiringSoon`) inside the warning window, `False` (`LicensePreExpiry`, `LicenseExpired`), or `Unknown` (`LicenseSecretMissing`) while the license cannot be read, unless the last known expiry is already inside the safety buffer or past |
| `LicenseExpired` | `True` once the license is expired or inside the 1 h safety buffer; without `fallbackToCE` the gateway reports phase `Error` |
| `LicenseDegraded` | `True` (`LicenseFallbackCE`) while the gateway runs CE because its license expired or entered the 1 h safety buffer |
| `LicenseSecretUnavailable` | License secret could not be read or parsed; `LicenseValid` is `Unknown` meanwhile. The stage is still judged from the last known expiry (`status.licenseExpiry`), so a `fallbackToCE` gateway falls back to CE when that expiry enters the 1 h safety buffer. Once that expiry is inside the buffer or past, `LicenseValid` shows the stage (`False`, reason `LicensePreExpiry` or `LicenseExpired`) instead of `Unknown` |
| any of `DragonflyReady`, `IstioConfigured`, `LicenseSecretUnavailable` with reason `CRDNotInstalled` | The feature is enabled but its CRD (Dragonfly Operator, Istio, External Secrets Operator) is not installed. Install it, then restart the operator so it also watches the kind |

---

## Endpoint Status

| Condition | Meaning |
|---|---|
| `ResolvedRefs` | Gateway and referenced policies exist (endpoint controller): `True` (`RefsResolved`), or `False` (`GatewayNotFound`, `PolicyNotFound`) |
| `Accepted` | Included in the gateway's validated configuration (gateway controller): `True` (`Accepted`); `True` (`PartiallyAccepted`) when an older endpoint serves some of its entries; `False` (`EndpointConflict`) when older endpoints serve all of them; `False` (`EndpointInvalid`) when it fails krakend check on its own; `False` (`PolicyInvalid`) when a policy it references fails on its own, or it fails only together with a policy of another namespace. Both messages begin "Not served by gateway <ns>/<gw>, which serves its other endpoints:", or "Will not be served when gateway <ns>/<gw> next applies its config:" while the gateway's pass applied nothing new (its endpoints fail only together, or its ConfigMap could not be published; on a root failure or a validator outage the endpoint keeps the message it has); `EEFeaturesStripped` (`True` while some entry is still served, `False` when every entry was an EE wildcard) after a CE fallback render removed Enterprise-only features; `True` (`SchemaNameConflict`) for a docs-only schema name clash. Removed while a referenced policy is missing |
| `Ready` | Both of the above, for the current generation: `True` (`Ready`), or `True` (`SchemaNameConflict`) for a docs-only clash; `Unknown` (`Pending`) until the gateway accepts the generation; otherwise `False` with the failing condition's reason, so a `PartiallyAccepted` or `EEFeaturesStripped` endpoint is not `Ready` |

- **`Ready=Unknown`, reason `Pending`, for more than a few seconds:** the
  gateway has not accepted this generation. Check the gateway:
  `kubectl get krakendgateway <gw> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="ConfigValid")]}'`.
  A gateway whose newest config cannot be applied (`GatewayRootInvalid`,
  `CombinedConfigInvalid`) keeps every changed endpoint `Pending`, except one
  that fails on its own, which reads `EndpointInvalid` or `PolicyInvalid`.
- **`Accepted=False`, reason `EndpointConflict`:** older KrakenDEndpoints
  serve every one of this endpoint's (path, method) pairs, so none is served
  (when only some are lost the reason is `PartiallyAccepted`, below). Rename
  the route or remove the duplicate.
- **`ResolvedRefs=False`, reason `GatewayNotFound` or `PolicyNotFound`:** create
  the named gateway or policy; the endpoint recovers on its own.
- **`Accepted=False`, reason `EndpointInvalid`:** see
  [Endpoint shows `Accepted=False`, reason `EndpointInvalid`](#endpoint-shows-acceptedfalse-reason-endpointinvalid).
- **`Accepted=False`, reason `PolicyInvalid`:** see
  [Endpoint shows `Accepted=False`, reason `PolicyInvalid`](#endpoint-shows-acceptedfalse-reason-policyinvalid).
- **`Accepted` reason `PartiallyAccepted`:** see
  [Endpoint shows `Accepted` reason `PartiallyAccepted`](#endpoint-shows-accepted-reason-partiallyaccepted).
- **`Accepted` reason `EEFeaturesStripped`:** the gateway runs CE in license
  fallback and its render removed Enterprise-only features from this endpoint;
  the message lists them. See *License expiry warnings*.
- **`Ready=True`, reason `SchemaNameConflict`:** the endpoint is served, but it
  defines a component schema differently from the endpoint the gateway's
  published documentation takes that name from. See
  [Endpoint shows `Ready` reason `SchemaNameConflict`](#endpoint-shows-ready-reason-schemanameconflict).

---

## AutoConfig Status

| Condition | Meaning |
|---|---|
| `SpecAvailable` | The OpenAPI spec was fetched (`SpecFetched`), or not (`SpecFetchFailed`) |
| `Synced` | Generated endpoints are in sync with the spec (`True`, reason `Synced`). `False` with reason `OperationsFailed` while any operation is held, or with the failure's reason (`SpecFetchFailed`, `CUEEvaluationFailed`, `UnmatchedOverride`, `AmbiguousOverride`, `AdditionalEndpointScopeFailed`, `EndpointReconcileFailed`, `ValidatorUnavailable`) |
| `EndpointsReady` | Every generated endpoint's `Ready` is True for its current generation (`AllEndpointsReady`), or `False` (`EndpointsNotReady`) naming up to five endpoints and their reasons (`Pending` while the endpoint controller has not reported a new or changed endpoint) |
| `Ready` | `SpecAvailable`, `Synced` and `EndpointsReady` are True (`Ready`); otherwise `False` with the reason of the first that is not, or `Unknown` (`Pending`) while one of them is still absent |

The status also lists, each at most 20 entries with messages cut at 256 bytes:

- `status.failedOperations`: operations held, with reason `CUEEvaluationFailed`,
  `ConfigValidationFailed` or `EndpointRejected`;
- `status.skipped`: operations given no endpoint by rule, with reason
  `UnsupportedMethod` (HEAD, OPTIONS, TRACE) or `DuplicateOperationId`;
  `status.skippedOperations` counts them all;
- `status.warnings`: spec problems that do not stop a sync;
- `status.readyEndpoints`: generated endpoints that are Ready.

When a sync fails before its endpoint writes, these lists stay as the last sync
that reached the writes recorded them.

## ReadMe Publishing (postRestartJob)

The three gateway CRs in `AppCluster-Infrastructure` (dev, preprod, and prod
`api-gateway`) use `spec.postRestartJob` to publish the gateway's OpenAPI
spec to ReadMe after every restart, keeping the hosted API reference in sync
with what's actually deployed.

| Component | Value |
|---|---|
| Image | `mycarrieracr.azurecr.io/ci/rdme-publisher:<yy>.<ww>.<sha>` |
| Source | `ITM.Docker.Images` — `ci/rdme-publisher/` |
| Baked entrypoint | `/usr/local/bin/publish-openapi.sh` |
| CR `script` | 2-line stub: `exec /usr/local/bin/publish-openapi.sh` |

### Environment contract

| Variable | Purpose |
|---|---|
| `GATEWAY_NAME` | Gateway Service name — with `GATEWAY_NAMESPACE` and `OPENAPI_PORT` it forms the in-cluster fetch URL `http://$GATEWAY_NAME.$GATEWAY_NAMESPACE.svc.cluster.local:$OPENAPI_PORT/openapi.json` |
| `GATEWAY_NAMESPACE` | Namespace the gateway runs in |
| `OPENAPI_PORT` | Port the gateway serves its OpenAPI spec on |
| `RDME_BRANCH` | Target ReadMe version (see mapping table below) |
| `RDME_KEY` | ReadMe API key, injected via the banzaicloud Vault webhook annotations — never set as a plain env value |

**Slug is the upsert identity key.** Uploading with the same `--slug` value
UPDATES the existing ReadMe definition; a different slug — including one
that changes only because it was omitted — creates a NEW, duplicate
definition silently. With `--slug` omitted, `rdme` derives the slug from the
file argument, so uploading `/tmp/openapi.json` yields the slug
`tmpopenapi.json`. This happened once and left a stray duplicate definition
in the ReadMe `v0` branch. The upload MUST always pin `--slug=openapi.json`
explicitly. **Health signal:** the job log line "successfully **updated**"
means the publish landed correctly; "successfully **created**" means the
slug drifted and a new duplicate definition was just created — investigate
immediately.

### openapi-serve sidecar restarts

The sidecar serving `/openapi.json` has a liveness probe (a TCP check on the
openapi port), so a wedged listener is restarted rather than left unready.
`RESTARTS` climbing on `openapi-serve`, or `CrashLoopBackOff`, means the probe
is failing repeatedly:

```bash
kubectl describe pod -l app.kubernetes.io/instance=<name> | grep -A5 openapi-serve
kubectl logs <pod> -c openapi-serve --previous
```

`kubectl describe pod` prints the probe's own failure text, which is usually
enough on its own. Note the pod leaves the Service endpoints while the sidecar
is unready — pod readiness is the AND across containers — so a sidecar in
backoff also stops the main gateway serving traffic. If the publish job cannot
fetch the spec, check this before looking at ReadMe.

A custom `spec.openapi.livenessProbe` that can never succeed produces exactly
this shape. The webhook rejects the structurally impossible cases (a `grpc` or
HTTPS handler against the default busybox sidecar, an off-pod `host`, no
handler), but it cannot validate an `exec` handler — the sidecar has a
read-only root filesystem and only busybox binaries, so an exec probe that
writes to disk or calls a missing binary will fail every check.

### ReadMe mapping

| ReadMe branch | Gateway environment |
|---|---|
| `v0` | dev |
| `v0.2` | preprod |
| `v1.0` | prod |

ReadMe project id: `676076b78e57b80044fa3114`. Definitions are managed at
dash.readme.com → version switcher → **Settings** → **API Definitions**. The
published definition powers the docs site's top-nav **API Reference**
button.

### Forcing a re-publish

A change to the Job's image, command, script, or env mints a new job
checksum, which triggers exactly one re-fire per gateway. To force a re-run
without any spec change, clear the stored checksum:

```bash
kubectl patch krakendgateway <name> -n <ns> --subresource=status --type=merge -p '{"status":{"lastPostRestartJobChecksum":""}}'
```

See docs/upgrade-guide.md `v0.13.4 — postRestartJob hardening` for the
checksum mechanics this relies on.

---

## Troubleshooting

### Gateway stuck in `Deploying`

**Symptom:** Gateway phase remains `Deploying` and never transitions to `Running`.

**Diagnosis:**
```bash
kubectl describe krakendgateway <name>
kubectl get deploy -l app.kubernetes.io/instance=<name> -o wide
kubectl describe deploy <name>-krakend
```

**Common causes:**
- Image pull failure (check image name, pull secrets)
- Resource limits too low (OOMKilled)
- Liveness probe failing. The pod carries two, with different handlers — the
  `krakend` container's HTTP probe on `/healthz`, and (when
  `spec.openapi.enabled: true`) the `openapi-serve` sidecar's TCP probe on the
  openapi port. `kubectl describe pod` names the container that failed.
- A Deployment that was deleted and is being recreated reads `Deploying` until
  its pods are available.
- `ProgressDeadlineExceeded` — sets `Available=False`/`RolloutFailed` (phase `Error`) with a `RolloutFailed` event; the gateway returns to `Running` once the rollout recovers

### Gateway stuck in `Error`

**Diagnosis:**
```bash
kubectl describe krakendgateway <name>
kubectl get events --field-selector involvedObject.name=<name> --sort-by='.lastTimestamp'
```

**Common causes:**
- Config validation failure — check the `ConfigValid` condition message.
  `ConfigValid=False` with reason `GatewayRootInvalid` quotes the root's own
  output (fix `spec.config`). With reason `CombinedConfigInvalid` the endpoints
  that pass on their own fail the config together; the message reads "The
  endpoints that pass krakend check on their own fail it together, so the last
  applied config keeps serving; any endpoint that fails on its own is excluded
  first", and the output is only in the operator log ("the gateway's config
  fails krakend check only together"). Three paths lead there. A write that
  admission let through with a warning can: both policy and gateway admission
  check an endpoint that already fails on its own before they check the
  endpoints together, so while that endpoint is not yet recorded as excluded, a
  write that makes the others fail only together is admitted with a warning.
  A gateway with more router clashes than the render resolves (21 entries) can:
  the full check's route stage refuses the entries that were not resolved, so
  remove the clashing endpoints. And the safety re-check after exclusions can
  fail even though the excluded endpoints are out. Report any other case with
  the log line. Whichever path led there, and for `GatewayRootInvalid`, the
  gateway keeps serving its last applied config (`status.configChecksum`). Endpoints the operator excludes meanwhile
  say they will not be served when the gateway next applies its config. The
  gateway's Deployment (unless a plugin ConfigMap is missing, which holds it),
  Service and other resources are still reconciled. Only the rejected render
  waits for a fix.
- License expired without CE fallback (`LicenseExpired=True`, `Ready` reason `LicenseExpiredNoFallback`) — renew the license or set `fallbackToCE: true`. A missing license Secret (`LicenseSecretUnavailable=True`) does not change `Ready` or the phase
- Rollout timeout (`Progressing=False`, `Available=False`, reason `RolloutFailed`) — check
  Deployment events. `RolloutFailed` describes the current rollout only: once
  you push a fix, the gateway reads `Deploying` until the new rollout
  converges or misses its own deadline, and a second `RolloutFailed` event
  means the new rollout failed too.
- `PluginsResolved=False`, reason `ConfigMapNotFound` — a plugin ConfigMap is
  missing; the Deployment is held until it exists. Create it in the gateway's
  namespace. A config applied during the hold (or a new gateway's first
  config) then rolls out; otherwise nothing rolls.

### `observedGeneration` stays behind `metadata.generation`

**Symptom:** `kubectl get krakendgateway <name> -o jsonpath='{.metadata.generation} {.status.observedGeneration}'`
prints two different numbers for more than a pass or two, and the operator log
shows an error for the gateway. Flux and kstatus report the gateway as in
progress.

**Meaning:** the operator could not apply the whole spec, so it does not
claim the new generation (the `Ready` condition's `observedGeneration` stays
behind with it). The status is otherwise current, and the gateway is retried
with backoff.

**Common causes** (the log line names the object):
- A child resource was rejected, for example an HPA, VirtualService,
  ExternalSecret or Dragonfly refused by an admission webhook. Every other
  child is still reconciled.
- Old config ConfigMaps could not be deleted.
- A ConfigMap that is not the gateway's sits at `<gateway>-config-<hash>`, the
  name of the applied config. The Deployment is held, so spec changes such as
  `spec.replicas` are not applied. Rename or delete that ConfigMap.

A rejected config, an unavailable validator and a missing plugin ConfigMap
do not hold it back; their conditions say why.

### Installed Istio, External Secrets or Dragonfly after the operator

The operator decides at startup which optional kinds to watch. Restart it
after installing one of these CRDs; until then the objects are created and
corrected only when their gateway reconciles for another reason. The
operator log lists the kinds it did not watch (`optional CRD not installed
at startup`).

### An HPA, Dragonfly, ExternalSecret or VirtualService disappeared

The operator deletes these when the gateway no longer asks for them
(`spec.autoscaling`, `spec.dragonfly.enabled`,
`spec.license.externalSecret.enabled`, `spec.istio.enabled`). It deletes
only objects whose controller owner reference is the gateway. To keep one,
re-enable the feature, or copy the object under another name before
disabling the feature. For the HPA, keep `spec.autoscaling` set instead: with
it removed the operator sets the Deployment's replicas to `spec.replicas` on
every reconcile and would fight a renamed HPA.

Disabling the license ExternalSecret also garbage-collects the
`<gateway>-license` Secret it created, so a `license.secretRef` pointing at
that Secret stops working. Disabling Dragonfly deletes the instance at once;
pods still running the previous config lose their Redis connection until the
rollout completes, or until a rejected render is fixed.

### Gateway reports `ValidatorUnavailable`

**Symptom:** `ConfigValid` is `Unknown` with reason `ValidatorUnavailable`; the
gateway keeps serving its last applied config and a new config is not rolled out
(image, plugin and license changes still roll). The outage excludes no endpoint
and lifts no exclusion: each endpoint keeps the `Accepted` verdict it has (a
gateway that has never applied a config is the exception: a pass that applies
nothing removes every `Accepted`).

**Diagnosis:** the condition message carries the cause. `no such file or directory` means the operator image lacks `/usr/local/bin/krakend`; `context deadline exceeded` means a run exceeded 30 seconds (check the operator pod's CPU throttling and memory); `signal: killed` without `context deadline exceeded` means the process was killed, usually by memory pressure on the operator container (a timeout's message also ends in `signal: killed`); `creating temp file` or `writing config to temp file` means the operator's temp directory is unwritable or full; `preparing validation copy` means the validation copy of the rendered config could not be built.

**Resolution:** fix the environment; the operator retries with exponential backoff and the gateway recovers on its own. Backoff grows up to 5 minutes, so recovery can lag that long after the cause is fixed. Editing the gateway, or restarting the operator, retries at once. Reverting the change that could not be validated also clears the condition: once the render equals the applied configuration again, `ConfigValid` returns to `True`.

### Gateway reports `ConfigPublishFailed`

**Symptom:** `ConfigValid` and `Ready` are `Unknown` with reason `ConfigPublishFailed`; the gateway keeps serving its previous config and a Warning event `ConfigPublishFailed` was emitted when it entered the state. The newest render passed `krakend check`, so the cause is not the config's content.

**Diagnosis:** the `ConfigValid` message ends with the cause (the operator log has the same error):
- `configmap <ns>/<gateway>-config-<hash> exists but is not controlled by gateway <name>`, or `holds config <x>, not <y>`: someone else's ConfigMap sits at the content-addressed name and holds the right bytes. The operator hashes the payload of any ConfigMap at that name whatever its owner or annotation, deletes one that holds other bytes, and does not delete one that holds the right bytes. Look at it with `kubectl get configmap <name> -n <ns> -o yaml` (owner references, annotation `krakend.io/checksum-config`); delete it to let the operator create its own copy. Pods that start meanwhile mount the right bytes.
- `deleting configmap <name> whose data does not match its checksum: ...`: a ConfigMap at the name holds another payload (someone edited, copied or recreated it) and the operator may not delete it; check the operator's `delete` permission on ConfigMaps. Treat the unexpected writer as a security finding: the payload is what the pods load.
- `creating configmap <name>: ...` followed by the API server's refusal: a `count/configmaps` ResourceQuota is exhausted (free quota or raise it; old revisions are collected down to the last three), an admission policy refuses the create, or the rendered config exceeds the 1 MiB ConfigMap limit (split the gateway's endpoints or trim the spec).

**Resolution:** remove the cause. The operator retries with exponential backoff up to 5 minutes; editing the gateway or restarting the operator retries at once. Once the ConfigMap is published, `ConfigValid` returns to `True`.

### Reconcile error "updating gateway status" or "the cached gateway is behind its stored status"

A newly published config is recorded in `status.configChecksum` before endpoints are accepted or the Deployment is pointed at it, and that record reads as a rollout under way (`Progressing=True`, reason `ConfigDeployed`, so `Ready` is not `True`) until the Deployment moves on, including while a failed Deployment or ServiceAccount step keeps it from moving; while the gateway's own `Available` is `False`, it reads `Error` instead. If the Deployment loses its availability only after that record was stored and the failing step keeps it from moving, the gateway keeps reading `Deploying` (`Ready` is never `True`) until the step recovers. If the write fails (usually a conflict because the gateway was changed during the pass), the pods and the endpoints' `Accepted` stay as they were and the next pass applies the config. The log shows either `recording applied config <hash>: ...` or, when anything else in status changed in the same pass (for example `endpointCount` for an added endpoint) and the end-of-pass status write conflicts too, only `updating gateway status: Operation cannot be fulfilled ...`. Both clear within a retry or two. If `updating gateway status` persists as `forbidden`, check the operator's RBAC on `krakendgateways/status`; if it persists as `Operation cannot be fulfilled`, the gateway cache is not catching up: check list and watch on `krakendgateways`.

The error `the cached gateway is behind its stored status` means a pass that applies nothing found the Deployment on a config other than the one its cached gateway names, and the stored status disagreed with the cache. The pass changes nothing and retries, and clears once the cache catches up. If it persists, the gateway informer is not receiving events: check the operator's list and watch permission on `krakendgateways` and the watch's health.

### Gateway Deployment not updated: "no ConfigMap holds the applied config"

The operator logs this when the ConfigMap for `status.configChecksum`
(`<gateway>-config-<hash>`) was deleted while a newer render is being
rejected. The Deployment is left as it is: running pods keep their config,
but a deleted Deployment cannot be recreated. Fix the rejected input
(`kubectl describe krakendgateway <name>`, condition `ConfigValid`). The next
config that passes validation is published and rolled out. The same
hold follows when the operator deletes the applied config's ConfigMap because
its `krakend.json` does not hash to the checksum (someone replaced the payload,
or created a ConfigMap of that name with another payload, owned or not). The
operator emits a Warning event `ConfigMapTampered` on the gateway naming the
deleted ConfigMap (`kubectl get events --field-selector reason=ConfigMapTampered`);
when the pass publishes the config again, the status ends `ConfigApplied` and the event is the only trace.

### Gateway reports `ResourceNotControlled`

An object already exists with the gateway's name, and the gateway does not take
it over. It writes an existing ServiceAccount, Service, PodDisruptionBudget,
HorizontalPodAutoscaler, Deployment, Dragonfly, ExternalSecret or VirtualService
only when it controls it, or when the object has no controller and carries the
labels the operator stamps on that kind: `app.kubernetes.io/instance=<gateway>`
and `app.kubernetes.io/managed-by=krakend-operator`, except for the Dragonfly,
whose instance is `<gateway>-dragonfly`. The message names the labels each
refused object needs.

**Symptom:** `ResourcesControlled` is `False` with reason `ResourceNotControlled`,
and `Ready` carries the same reason (phase `Error`). The message names each
refused object (kind and `<namespace>/<name>`) and its controller if it has one:

```bash
kubectl get krakendgateway <name> -n <ns> \
  -o jsonpath='{.status.conditions[?(@.type=="ResourcesControlled")].message}'
```

The operator also logs `reconciling <kind>: <kind> <ns>/<name> ...`. When the
refused object is the ServiceAccount, it logs `holding the Deployment and the
post-restart Job: serviceaccount <ns>/<name> is not controlled by gateway
<name>`: the Deployment and the post-restart Job run as that ServiceAccount, so
both are left as they are. Running pods keep running, and a new gateway gets no
Deployment. A ServiceAccount the operator could not write holds them too, and
the condition keeps its previous value: only the log names it.

What points at a refused object by name is held with it:

- **A refused Deployment.** The HorizontalPodAutoscaler would scale it, so the
  gateway writes none and deletes the one it controls. The configs applied
  meanwhile raise no `Progressing`, and old config ConfigMaps are still
  collected, so a long refusal does not pile them up.
- **A refused Service.** The VirtualService would route the gateway's hosts to
  it, so the gateway writes none and deletes the one it controls.
  `IstioConfigured` is `False` with reason `ResourceNotControlled`.
- **A refused Dragonfly.** It is not reported ready (`DragonflyReady=False`,
  reason `ResourceNotControlled`), `status.dragonflyAddress` is cleared, and the
  rendered Redis pool does not point at it: the render falls back to
  `spec.redis`, as when the Dragonfly CRD is missing, and without `spec.redis`
  the config has no Redis pool until the conflict is resolved. The rendered
  config changes, and so does its checksum.

Once the conflict is resolved, the next pass writes them again.

**Fix:** either of:

- Rename the gateway, so its name no longer collides.
- Hand the object over. If it has no controller (`kubectl get <kind> <name> -n
  <ns> -o jsonpath='{.metadata.ownerReferences}'` prints nothing), label it:
  `kubectl label <kind> <name> -n <ns> app.kubernetes.io/instance=<gateway>
  app.kubernetes.io/managed-by=krakend-operator` (the labels named in the
  message). The gateway takes it over, and
  deleting the gateway then deletes it. An object that another controller owns
  cannot be handed over: remove its other owner or rename the gateway.

Nothing watches an object the gateway does not control, so the reconcile is
retried with backoff. Edit the gateway or restart the operator to retry at once.
The condition turning `False` raises a Warning event with the same reason, and
turning `True` again a Normal one. Once the conflict is resolved, the rollout of
the config applied meanwhile shows only as `Progressing` (`DeploymentUpdated`).
If the message still names an object the gateway now controls, another write of
the pass is failing or the Deployment is held: the operator log names it, and
the condition clears on the first pass that fails nothing and writes the
Deployment.

### Endpoint shows `Invalid`

**Diagnosis:**
```bash
kubectl describe krakendendpoint <name>
```

`Invalid` is the phase of every `Ready=False` endpoint that is not `Detached`,
`Conflicted` or `Pending`.

**Common causes:**
- `policyRef` references a non-existent policy (`ResolvedRefs=False`, reason `PolicyNotFound`)
- The gateway leaves the endpoint out because it, or a policy it references,
  fails krakend check on its own (`Accepted=False`, reason `EndpointInvalid` or
  `PolicyInvalid`, below)
- A CE fallback render removed every entry (all wildcards) (`Accepted=False`,
  reason `EEFeaturesStripped`)

A `gatewayRef` to a non-existent gateway shows phase `Detached` instead
(`ResolvedRefs=False`, reason `GatewayNotFound`). For what each condition
means, see [Endpoint Status](#endpoint-status).

### Endpoint shows `Accepted=False`, reason `EndpointInvalid`

**Meaning:** the endpoint fails `krakend check` on its own: the gateway root
with this endpoint and the policies it references. The gateway leaves it out:
none of its routes is served, even if an earlier version of it was, and the
gateway serves its other endpoints. The message quotes the check's output,
which is this endpoint's own. That check renders the gateway root together
with the endpoint, so output from a failure that occurs only with a root
setting can reveal that setting to the endpoint's author.

The message begins `Not served by gateway <ns>/<gw>, which serves its other
endpoints:` once the gateway applied a config without it. It begins `Will not
be served when gateway <ns>/<gw> next applies its config:` while the gateway
cannot apply its newest config (its endpoints fail only together, or its
ConfigMap could not be published; on a root failure or a validator outage the
endpoint keeps the message it has): until then the last applied config keeps serving, which
may still hold an earlier version of this endpoint.

An endpoint that loses an entry to an older endpoint (`PartiallyAccepted`) is
judged with all its entries, including the one left out: when that entry fails
on its own, the whole endpoint is excluded.

**What to do:**
- Fix the entry the output points at.
  `kubectl apply --dry-run=server -f <endpoint>.yaml` runs the same check in
  admission and prints the same output.
- An update to an endpoint that already fails is admitted with a warning. It
  stays excluded until it passes.
- When the endpoint references a policy of another namespace, the quoted output
  comes from a check with that policy rendered empty, so it is the endpoint's
  own fault either way.

### Endpoint shows `Accepted=False`, reason `PolicyInvalid`

**Meaning:** a policy the endpoint references fails `krakend check` on its own,
or the endpoint fails only together with a policy of another namespace. The
message names the policy and never quotes it: its owner may be another team.
The endpoint is excluded as for `EndpointInvalid`, and its message begins the
same way.

**What to do:**
- The policy's owner fixes the policy. The endpoint's message never quotes the
  policy, but the policy's own status does: its `Ready` condition is `False`
  with reason `PolicyInvalid` and carries the policy's `krakend check` output
  (see [Policy shows `Ready=False`, reason `PolicyInvalid`](#policy-shows-readyfalse-reason-policyinvalid)).
  Read it with `kubectl get krakendbackendpolicy <name> -n <ns>
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'`. A policy of
  another namespace that passes alone has no `PolicyInvalid` of its own: the
  endpoint fails only together with it.
- Or point the endpoint at another policy.

### Policy shows `Ready=False`, reason `PolicyInvalid`

**Meaning:** the policy fails `krakend check` on its own: it is rendered alone,
on a default gateway in its own namespace, and checked. Every endpoint that
references it, in any namespace, is excluded from its gateway's config with
`Accepted=False`, reason `PolicyInvalid`. The message is `fails krakend check on
its own:` followed by the policy's own output, which its owner may read. A
Warning event with the same reason is raised when the policy turns invalid,
not on every reconcile. A policy whose circuit breaker or rate limit fields are
out of range reads `InvalidCircuitBreaker` or `InvalidRateLimit` instead, and
is not rendered.

A stored policy can fail alone after an upgrade, after a `krakend` version
bump, or on an install with `webhooks.enabled: false`; admission rejects a
create that fails alone and a change that makes a passing policy fail.

**What to do:** fix the field the output points at. Once the policy passes,
`Ready` returns to `True`, and the endpoints that use it are served again on the
gateway's next reconcile.

### Policy shows `Ready=Unknown`, reason `ValidatorUnavailable`

**Meaning:** `krakend check` could not run for the policy: the binary is
missing or the run timed out. Nothing is known about the policy, so it is not
reported as invalid, and no event is raised when it turns `Unknown`. The
reconcile retries with exponential backoff, up to 5 minutes. The message
carries the cause; see [Gateway reports
`ValidatorUnavailable`](#gateway-reports-validatorunavailable) for how to read
it.

When the check works again, a policy that was `Unknown` emits a Normal `Ready`
event as it returns to `True`, and one that is invalid repeats its Warning
`PolicyInvalid` as it goes `Unknown` to `False`. Both fire once per change. A
reconcile cancelled while it waited for a check slot (the operator stopping or
losing its lease) shows only on the reconcile span and in the controller log.

### Gateway reports `EndpointsExcluded`

**Meaning:** one or more of the gateway's endpoints fail validation on their
own and are left out of its config; the rest is served. The exclusions
themselves change neither `ConfigValid` nor `Ready`.
`krakend_operator_gateway_excluded_endpoints` counts the excluded endpoints by
reason (alert: `KrakenDGatewayEndpointsExcluded`). When the message says the
endpoints `will not be served when the gateway next applies its config`, the
gateway cannot apply its newest config (`ConfigValid` is `False` or `Unknown`):
see [Gateway stuck in `Error`](#gateway-stuck-in-error) and
[Gateway reports `ValidatorUnavailable`](#gateway-reports-validatorunavailable).

**Finding the endpoints:** the condition's message names the first 10. All of
them:

```bash
kubectl get krakendendpoints -A -o json | jq -r --arg gw <gateway> --arg gwns <namespace> '
  .items[]
  | select((.spec.gatewayRef.name == $gw) and ((.spec.gatewayRef.namespace // .metadata.namespace) == $gwns))
  | select(any(.status.conditions[]?; .type == "Accepted" and .status == "False"
      and (.reason == "EndpointInvalid" or .reason == "PolicyInvalid")))
  | "\(.metadata.namespace)/\(.metadata.name)\t\([.status.conditions[] | select(.type == "Accepted") | .reason][0])"'
```

Each endpoint's own `Accepted` message says why. The condition clears, with no
action on the gateway, once the last of them passes, or is deleted.

**Slow reconciles after an operator restart:** the operator remembers each
verdict in memory only. After a restart or a leader failover, a gateway whose
whole render fails checks every one of its endpoints once more (when the render
is the applied config, only the endpoints that lost an entry are judged first,
and the rest runs only if one of them fails), one at a time
on the gateway controller's single worker: about 0.1 s per endpoint at the
chart's 500m CPU limit, so about 50–60 s for a gateway of 500 endpoints, while
other gateways' reconciles wait. It happens once per such gateway; afterwards
an unchanged gateway runs no check. An edit of the gateway root or its edition
costs the same once, because every endpoint's check then covers new content.

### Endpoint shows `Accepted` reason `PartiallyAccepted`

Some of this endpoint's entries are served; the ones in `status.conflicts`
are not, because they lose to the KrakenDEndpoint named in `winner`: an older
endpoint with the same method and route shape, or one whose route clashes with
this entry's in the router. Paths that differ only in parameter names are the
same route (for example `/users/{id}` wins over `/users/{name}`). An entry is
listed once for each endpoint it loses to, and the winner's entry may itself be
left out. When `winner` is the endpoint itself, an earlier entry of the same
KrakenDEndpoint has the same route shape and method. Remove the
duplicate entry from one of the two, or move it to the KrakenDEndpoint that
should own it.

### An endpoint was admitted, but the gateway excludes it

Admission and the gateway controller run the same checks, so this is rare.
It happens when:
- the update was admitted with a warning because its stored version already
  failed (the ratchet);
- the gateway's license changed: admission reads CE fallback from the
  gateway's status (`LicenseDegraded`), while the gateway controller decides it
  from the license during its reconcile, so until the status catches up the
  two can check different editions;
- the gateway uses Dragonfly: admission checks without the Redis address
  Dragonfly provides, the gateway controller with it;
- the gateway root or a referenced policy changed after the endpoint was
  admitted.

The endpoint's `Accepted` message quotes the gateway controller's check.

### Endpoint shows `Ready` reason `SchemaNameConflict`

The endpoint is served and `Ready` stays `True`, but its `Accepted` condition has
reason `SchemaNameConflict`: it defines a component schema under a name whose
published definition comes from another endpoint, with a different body, so the
gateway's documentation shows the other endpoint's schema. The message names
the schemas and the endpoint each is published from. The published definition is
the first in namespace/name order among the gateway's endpoints, not the oldest
endpoint (routes are oldest-wins). It is reported only where documentation is
published: an EE gateway, not in CE fallback, with `spec.openapi.enabled`. No
event is emitted for it, except the usual recovery event when the endpoint was
not accepted before. Rename the schema in the OpenAPI
spec of one of the services to publish both. Two endpoints of one AutoConfig
conflict only while they carry schemas from different versions of its spec, for
example while an operation is held. A conflict that outlasts a clean sync comes
from different AutoConfigs or hand-written endpoints.

### A KrakenDEndpoint is rejected with `Duplicate value`

Admission refuses an entry whose method and route another entry on the same
gateway already has: the same path, a path that differs only in parameter
names (`/users/{id}` and `/users/{name}`) or in repeated slashes. The message
names the KrakenDEndpoint that owns the route (or the entry of the same
KrakenDEndpoint). Remove or change the entry, or delete the other one first.
Against other KrakenDEndpoints only a route new to the stored object is
checked, so an older conflict does not block unrelated edits; it shows as
`PartiallyAccepted` or `EndpointConflict` above. Same-shape entries inside one
KrakenDEndpoint are always checked when an entry changes.

### Renaming a path parameter across several routes

Routes that share a parameterized prefix (`/users/{id}` and
`/users/{id}/orders`) must use the same parameter name at that position, because
KrakenD cannot route both otherwise. When they live in different
KrakenDEndpoints there is no valid intermediate state: the gateway-wide check
rejects each rename, with krakend's wildcard conflict on the renamed entry,
until the other is done. Keep such routes in one KrakenDEndpoint and rename them
in one apply, or delete the second KrakenDEndpoint, rename the first, then
recreate the second with the new name.

### Admission refuses a post-restart Job with `403 Forbidden`

**Symptom:** `kubectl apply` of a KrakenDGateway fails with `krakendgateways.gateway.krakend.io "x" is forbidden: spec.postRestartJob: <user> may not create pods in namespace <ns>, so the post-restart Job may not run as another ServiceAccount, read a Secret, or relax the operator's default security context`.

**Cause:** The enabled `spec.postRestartJob` sets a `serviceAccountName` other
than the gateway's name, takes `envFrom` from a Secret, has an `env`
`secretKeyRef`, or sets a `securityContext` or `podSecurityContext` field
outside the allow-list of settings that grant no privilege (for example
`privileged: true`, added capabilities, a `capabilities.drop` without `ALL`, an
`Unconfined` seccomp or AppArmor profile, `sysctls`, `seLinuxOptions`) or a
`container.apparmor.security.beta.kubernetes.io/*` annotation other than
`runtime/default`. The user making the request may not create pods in the
gateway's namespace. The operator creates the Job with its own permissions, so
the webhook asks the API server (SubjectAccessReview) whether the requester
could create such a pod themselves. A Job on the gateway's own ServiceAccount
with no Secret reference and only allow-listed security settings is not
reviewed, nor is one that runs as root with `runAsNonRoot: false`. `podLabels`
and the other `podAnnotations` are never reviewed.

**Resolution:** Have a user who may create pods in the namespace make the
change, or grant the requester (for a GitOps controller, its ServiceAccount)
`create` on `pods` in that namespace. Or drop the field: use the gateway's
ServiceAccount, keep the default security context, and inject secrets by another route (for example the Vault
annotations the ReadMe publisher uses). A `500` with `reviewing the requester's
access` instead means the review itself failed: check that the operator's role
has `create` on `subjectaccessreviews`.

### Admission rejects an endpoint with a krakend finding

**Symptom:** `kubectl apply` of a KrakenDEndpoint fails with `The
KrakenDEndpoint "x" is invalid: spec.endpoints: Invalid value: this endpoint
fails krakend check on its own: <message>`, where the message is what
`krakend check` reported for this endpoint (for example `undefined output
param`), or with a router clash that names the other endpoint.

**Cause:**
- the denial quotes only the output of this endpoint's own check, which renders
  the gateway root with this endpoint and the policies it references, alone, so
  output from a failure that occurs only with a root setting can reveal that
  setting to the endpoint's author;
- when the gateway's root fails on its own, the write is admitted with a warning naming the gateway, whose owner must fix it;
- an update whose stored version already fails is admitted with a warning, and the endpoint stays excluded until it passes;
- a write that would clash in KrakenD's router with another endpoint is refused,
  naming that endpoint and both paths: an older endpoint's entry wins, and a
  newer write may not push it out;
- a write refused because the gateway has more router clashes than the operator
  resolves at once (21) is refused whatever it changes, until the existing
  clashes are fixed: list them with `kubectl get krakendendpoints -A -o json |
  jq -r '.items[] | select(.status.conflicts != null) |
  "\(.metadata.namespace)/\(.metadata.name)"'` and remove or rename the clashing
  entries. On a pass that applies nothing, `status.conflicts` stays as the last
  applied render recorded it, so clashes past the cap never appear there; the
  gateway reports `CombinedConfigInvalid` until they are fixed.

**Resolution:** fix the entry the output points at. `kubectl apply
--dry-run=server -f <endpoint>.yaml` runs the same check and prints the same
output. A policy of another namespace that the endpoint references is named,
never quoted. For a warning that names the gateway, ask the gateway's owner to
fix its `spec.config`.

**`500 Internal Error: validating the gateway config: ...`:** the check could
not run: all three validation slots stayed busy for the 12 s budget, or the
validator itself failed. This is transient and `kubectl` does not retry it, so
run the command again. Controllers and GitOps tools retry on their own. A
gateway edit that cannot change the rendered config (image, version, replicas,
resources, probes, `postRestartJob`) is not checked and never draws this error. If it
repeats, check the operator pod's CPU and memory. A
write that breaks endpoints which pass today is refused with a `422` naming
the endpoints the check reached, even when the budget runs out first. The
`500` that remains on a large gateway needs endpoints that already fail, a
gateway update whose stored root fails on its own (fix that root first, see
the root denial above), or a decision that cannot finish in the budget (see
`breaks gateway` below, whose causes apply to a gateway update too, including
an endpoint that fails because a policy it references fails on its own).

### Admission refuses a policy with `breaks gateway`

**Symptom:** a KrakenDBackendPolicy write fails with `spec: Invalid value:
breaks gateway <ns>/<name>: ...`.

**Cause:**
- the denial names the endpoints the change breaks (`<ns>/<name>`), never their
  output: each fails on its own with the new policy and passes with the stored
  one;
- an endpoint that already failed with the stored policy never hides one the change breaks;
- `(+N more not checked)` means naming stopped at 20 endpoints; `(N not checked
  within the admission time)` means it stopped because a check could not run,
  for example when the 12 s budget ran out (a denial prints at most one of the
  two). Either way the change breaks at least the endpoints named;
- `500 Internal Error` with no endpoint named means the checks could not finish
  before the budget, or could not run, before any broken endpoint was found and
  before the change was decided. A change that breaks endpoints which pass
  today is refused with a `422` even when the budget ends first, so this `500`
  needs endpoints that already fail but are not yet recorded as excluded (for
  example right after their owners changed them), or an endpoint that fails
  because a policy it references fails on its own, which the controller never
  excludes while the whole render passes: fix that policy, or the endpoint's
  inline override of it. The decision itself can also be too large for the
  budget: many endpoints that lost an entry (resolve the clashes in
  `status.conflicts`), a write to a policy whose stored content fails on its
  own, or a gateway update whose endpoints use such a policy (every endpoint
  that uses it is judged first; fix the policy), or many distinct policies not
  checked yet since a restart (a retry helps). For failing, not yet recorded
  endpoints, each request scans the endpoints in a random order, so a retry
  also reaches others; but on a gateway of several hundred served endpoints it
  may not converge. It stops when the gateway controller records their
  exclusions: retry after its next reconcile;
- fix the policy, or ask those endpoints' owners;
- a gateway whose root fails on its own, or whose failing endpoints all already
  failed with the stored policy, gets a warning instead. On a create there is no
  stored policy, so the warning says the endpoints `already fail validation
  whatever this policy holds` (they fail with the policy empty too), and a
  failure that appears only with the endpoints together is still refused;
- a denial `breaks gateway <ns>/<name>: with this change the endpoints that use
  it fail validation together, though each passes on its own` names no endpoint:
  they passed together with the stored policy. It quotes nothing, because that
  output would show other tenants' values. Report it with the policy diff.

The same applies to a KrakenDGateway update refused for the endpoints it serves, except that a gateway whose stored root fails on its own does not turn the break into a warning for an endpoint its last applied config served: that endpoint failing with the new root is refused, naming it, and so is a failure that appears only with the endpoints together. Any other endpoint only draws a warning.

**Resolution:** fix the policy, or ask the owners of the endpoints it names to fix them.

### Admission refuses a policy update because of another namespace's endpoint

**Meaning:** an endpoint of another namespace references the policy, and the
update makes that endpoint fail on its own. A KrakenDEndpoint may reference a
KrakenDBackendPolicy of any namespace, without the policy owner's consent. Its
owner can therefore block updates of the policy this way, and can tell from
whether their endpoint reports `EndpointInvalid` or `PolicyInvalid` whether the
policy is what breaks it. The policy's content is never quoted to them.

**What to do:** ask the endpoint's owner to point it at their own policy, or
change the policy in a way their endpoint still passes. Restrict who may create
KrakenDEndpoints in namespaces you do not trust.

### A KrakenDBackendPolicy is stuck in `Terminating`

**Symptom:** `kubectl delete krakendbackendpolicy` returns, but the policy stays
with a `deletionTimestamp` and `kubectl describe` shows a `DeletionBlocked`
warning event.

**Cause:** the policy is still referenced. The finalizer
`gateway.krakend.io/policy-protection` keeps it, and keeps it rendered, until no
KrakenDEndpoint references it. List the referencing endpoints:

```bash
kubectl get krakendendpoints -A -o json | jq -r --arg ns <policy-namespace> --arg name <policy-name> \
  '.items[] | . as $e | select([.spec.endpoints[].backends[].policyRef | select(. != null)
     | select(.name == $name and ((.namespace // $e.metadata.namespace) == $ns))] | length > 0)
   | "\(.metadata.namespace)/\(.metadata.name)"'
```

Remove or repoint those references and the deletion completes. If the operator
is down, it completes when the operator is back. To remove the finalizer by
hand, use the command in the upgrade guide under
[Rollback](upgrade-guide.md#rollback). It is an UPDATE, which the policy webhook
still intercepts with `failurePolicy: Fail`, so it works only while the
operator is up, or after the operator's webhook configuration is removed.
Repoint the referencing endpoints first: a policy that is deleted while still
referenced leaves its endpoints with `PolicyNotFound`.

### The operator's AutoConfig writes skip the render check

The operator's own writes to KrakenDEndpoints a KrakenDAutoConfig controls are
not rendered at admission, because the AutoConfig controller validates the
endpoints it is about to write first. The audience, entry and duplicate-route
rules still apply to them. With webhooks enabled, the operator logs a startup
line `admission skips the render check for AutoConfig endpoint writes from`
with the username it is configured to trust, normally
`system:serviceaccount:<namespace>:<serviceaccount>` of the operator pod. The
line confirms the configured username, not that the operator's API client
authenticates as it: if the client runs under a different identity, the skip
simply does not apply and every write is rendered. If the log says
`no operator username`, the pod lacks
`POD_NAMESPACE` or `POD_SERVICE_ACCOUNT` (a custom deployment) and no write is
trusted: set `--operator-username` to the pod's ServiceAccount username.
Disabling the exemption is safe but costs one `krakend check` per generated
endpoint write, which can make a large AutoConfig sync slow and can fail it with
`500 Internal Error` when the validation slots stay busy.

### AutoConfig not generating endpoints

**Diagnosis:**
```bash
kubectl describe krakendautoconfig <name>
kubectl get events --field-selector involvedObject.name=<name>
```

**Common causes:**
- OpenAPI spec fetch failure (check URL, auth credentials)
- A failure to fetch or decode an externally-`$ref`'d document: the
  `SpecAvailable` and `Synced` conditions are `False` with reason
  `SpecFetchFailed` and a message prefixed `resolving external $refs: `, and
  existing endpoints are left at their last-good state. A relative `$ref`
  resolves against the URL of the document that contains it. Example data
  (an `example` field, an `examples` field, and everything inside an Example
  Object that an `examples` entry points to, such as its `value` or a raw
  payload file) is not fetched, so a `$ref`-shaped value there never causes
  this failure. The Example Object's own `$ref`, an `examples` or
  `components.examples` entry that is itself a `{"$ref": "…"}`, is fetched,
  and a failure to fetch it still causes this failure; so does a link of a
  root `$ref` chain in the fetched target. The fetched Example Object is
  inlined under `components/examples`, not `components/schemas`. A pointer not found, a resolution cycle, or
  a schema-name collision between two `$ref`s are listed in `status.warnings`,
  with a `SpecWarning` event when the inputs change, and don't block the sync.
- CUE evaluation error (check embedded/custom CUE definitions): an error
  outside the endpoint entries fails the whole sync with `CUEEvaluationFailed`,
  and one inside an entry holds that operation (see `OperationsFailed` below)
- Filter excludes all operations
- A `policyRef` in `defaults`, `overrides` or `additionalEndpoints` names a policy that doesn't exist: the AutoConfig's `kubectl apply` printed a `Warning:` naming the field (for example `spec.defaults.policyRef: KrakenDBackendPolicy ns/name not found`), and each generated endpoint that uses it is rejected at admission (`Not found` on `...policyRef.name`). The AutoConfig holds each as `EndpointRejected` in `status.failedOperations`, with `Synced=False` and reason `OperationsFailed`; it is not retried with backoff, and the other operations are still written. Create the policy or fix the reference; `kubectl get krakendbackendpolicies -n <namespace>` lists what exists
- An override's `operationId` doesn't match any operation in the spec — see *AutoConfig sync fails with `UnmatchedOverride`* below; one that several operations declare — see *AutoConfig sync fails with `AmbiguousOverride`*
- `Synced=False` with reason `OperationsFailed`: some operations are held, and the rest of the AutoConfig is converging. List them with `kubectl get kac <name> -n <namespace> -o jsonpath='{.status.failedOperations}'`. Each entry names the operation (`method`, `path`, `operationId`), a `reason` and a `message` cut at 256 bytes. An entry held at its endpoint (`ConfigValidationFailed`, `EndpointRejected`) also names the generated `endpoint`; a `CUEEvaluationFailed` entry has no `endpoint`. The operator log has the full cause of every held operation, a CUE failure or a rejection, at Info (message `operation held`, with `operation` and `error` fields), once per change of the causes for each operator process, so a restart logs them again; the status cuts each message at 256 bytes. An `OperationsFailed` Warning event is emitted only when the `Synced` condition or `status.failedOperations` changes, not when endpoints turn Ready. A held operation keeps its last endpoint, and no stale endpoint is deleted until none is held. It is not retried with backoff: the AutoConfig retries at its resync interval, or at once when an input changes (see *Forcing an immediate AutoConfig reconcile*). By reason:
  - `CUEEvaluationFailed`: the operation's entry fails CUE evaluation (the message names the constraint, for example a `documentation/openapi.audience` declared on the operation that is not a list of strings). Fix the operation in the spec or the CUE definitions.
  - `ConfigValidationFailed`: the operation's own endpoint fails krakend check
    on its own (the message quotes its output), or would make KrakenD's router
    unable to serve one of two entries (the message names the other endpoint),
    or the gateway has more router clashes than the operator resolves at once
    (every write is held until they are fixed). Two generated endpoints that
    share a method and route shape (`has the same route as GET /x in
    <endpoint>`) are held the same way. The status never copies `krakend check`
    text about other endpoints. When the operation fails on its own, the message
    after `fails krakend check on its own: ` is the check's finding, cut at 256
    bytes. For a route clash the advice in the message is written for
    KrakenDEndpoint authors and the status cuts it at 256 bytes: exclude one of
    the operations with `spec.filter`, or fix the upstream paths to use one
    parameter name. A `urlTransform` that collapses two operations onto one
    method and path is a misconfiguration the same way: one is published and the
    other is listed in `status.skipped` as a duplicate, and `spec.filter` cannot
    separate them, so fix the transform or the paths.
  - `EndpointRejected`: the API server or the webhook refused the write (the message says why, for example a missing `policyRef` policy), another object controls the endpoint's name, a label-matched endpoint could not be adopted, or the gateway is CE and the endpoint uses an Enterprise-only `extra_config` namespace (`not written: gateway ns/name runs CE, which ignores these Enterprise-only namespaces`). Fix the cause, move the gateway to EE, or exclude the operation with `spec.filter`.
- `Synced=False` with reason `ValidatorUnavailable`: `krakend check` could not run (or the reconcile was cancelled while it waited for a check slot), so nothing was written or deleted (it may still have adopted label-matched endpoints, which changes owner references only). It retries with backoff on either trigger. See *Gateway reports `ValidatorUnavailable`*.
- `status.skipped` lists operations given no endpoint by rule: HEAD, OPTIONS and TRACE operations (`UnsupportedMethod`, no event) and duplicates of an earlier operation (`DuplicateOperationId`: the same path and method, operationId or endpoint name). An operation `spec.filter` excludes is not listed.
- `status.warnings` lists spec problems that do not stop a sync but can leave the endpoints or the published documentation wrong: `$ref`s the resolver could not honour, colliding schema names, `#/…` refs inside fetched documents that are not resolved in their document (a pointer the document lacks, or an Example Object reference: both resolve against the main spec), recursive schemas in fetched documents (a cycle note; the schema is still rewritten correctly), external `$ref`s in a ConfigMap-sourced spec, parameter `$ref`s that do not resolve (the operation using one is held; an unresolvable path-level ref holds every operation on the path), and schema references `components/schemas` does not define. One root cause can list two notes. A `SpecWarning` event is emitted when the inputs change, at most 20 input warning events per sync (`SpecWarning`, `DuplicateOperationId` and `AdditionalEndpointOverride` together). A dereferenced spec over 10 MiB fails the sync (`SpecFetchFailed`).
- A spec fetch that exceeds the 2-minute deadline for fetching and resolving external `$ref`s fails with `SpecFetchFailed` and `context deadline exceeded`; a single request is bounded at 30 seconds.
- Generated endpoints can't be written for a transient reason: the `Synced` condition is `False` with reason `EndpointReconcileFailed` and the message names up to five endpoints and the API error (for example the API server or an admission webhook timed out). Every endpoint is attempted, no stale endpoint is deleted, and it retries with backoff regardless of `trigger`, at least every 5 minutes. A write the API server rejects as invalid, or a name another object controls, is not this failure: it holds that operation (`OperationsFailed` above). A `Conflict` or `AlreadyExists` from a stale cache is *not* a failure either — it requeues quietly a second later with no error or event, so if the AutoConfig converges a moment later with nothing in between, that's this path working as intended, not a bug.
- `Ready=False` with reason `EndpointsNotReady` while `Synced` is `True`: the
  endpoints were written but one is not `Ready` (the `EndpointsReady` message
  names up to five and their reasons: `Pending` while the endpoint controller
  has not caught up, `EndpointConflict` when an older endpoint serves the route,
  `EndpointInvalid`, `PolicyInvalid`, `GatewayNotFound`). See [Endpoint
  Status](#endpoint-status).
- Adoption and ownership: the AutoConfig tracks its endpoints by controller owner reference, not by labels. An endpoint carrying both `gateway.krakend.io/autoconfig=<name>` and `gateway.krakend.io/auto-generated=true` with no controller is adopted on the next reconcile (uncontrolled endpoints are not watched), then converged or deleted. Anyone who can label an uncontrolled endpoint in the namespace can therefore hand it to the AutoConfig, which deletes it if no operation generates it. An uncontrolled endpoint whose name an operation generates is taken over whatever its labels. An endpoint controlled by another object is never changed or deleted.
- Two AutoConfigs reconciling at once can check the same gateway without seeing
  each other's pending writes. The gateway controller still never publishes a
  render that fails krakend check: it excludes the endpoints that fail on their
  own, and keeps its applied config with `ConfigValid=False` when its root fails
  or its endpoints fail only together.
- A rename and a brand-new rejection in the same pass: the precheck models the
  stale endpoints as deleted unless the last status already lists a write of
  this pass as `EndpointRejected`, so a rejection that first appears in that
  pass is not predicted. A renamed sibling is written, the 422 stops the delete,
  and the gateway renders the old and the new endpoints together. The mixed
  render resolves oldest-first: the stale endpoint is older and keeps its
  routes, and the newer entry is left out. The whole render passes, so
  `ConfigValid` stays `True` and nothing is counted. The losing endpoint reads
  `Accepted=False` with reason `EndpointConflict` (or `PartiallyAccepted`),
  `status.conflicts` names the stale endpoint as winner, an `EndpointConflict`
  Warning fires, and the AutoConfig reads `EndpointsNotReady`. The moved route
  is not served until the cause of the API server's 422 is fixed; this is the
  accepted write-time remainder. When the rejection is already recorded, the
  sibling is held instead.
- A rename shows the same conflict briefly without any rejection: between the
  sibling's write and the stale delete the gateway renders the mixed state, the
  newer endpoint reads `EndpointConflict` or `PartiallyAccepted`, and it clears
  once the stale endpoint is deleted. `ConfigValid` stays `True` and the failure
  counter does not move.

### AutoConfig sync fails with `UnmatchedOverride`

**Diagnosis:**
```bash
kubectl describe krakendautoconfig <name>
```
The `Synced` condition is `False` with reason `UnmatchedOverride` and message
`spec.overrides reference operationIds or backend indexes not present in the
OpenAPI spec: <entries separated by "; ">`, naming at most five and then "and N
more". A backend index outside the operation's backends is listed as
`<operationId> backends[<i>]`. A matching `Warning` event is also emitted.

**Common causes:**
- A `spec.overrides[].operationId` doesn't match any operationId in the fetched OpenAPI spec (e.g. the service doesn't declare an operationId for that operation at all)
- A `spec.overrides[].backends[].index` is at or above the operation's backend count (1 with the default CUE definitions)
- If the operationId *is* in the spec, check `status.failedOperations`: an override on an operation that failed evaluation is held with it, not unmatched

**Resolution:** Correct the override's `operationId` to match the spec, or
remove the override if it's no longer needed. If the target operation has no
`operationId` at all: `spec.defaults` applies to every generated operation;
for that single operation, add an `operationId` to the service's OpenAPI spec,
replace the operation with an `additionalEndpoints` entry (the same endpoint
and method replaces the spec-derived one), use a custom CUE definitions
ConfigMap (`spec.cue.definitionsConfigMapRef`), or have the service declare
`audience` directly on the operation as a list of strings — the default CUE
definitions default it to `["public"]` when absent and require a list of
strings when present (`cue/defaults.cue`), which avoids needing an override
at all. Existing `KrakenDEndpoints` keep their last-good state — nothing
regenerates — until the override is fixed and the resource re-syncs.

### AutoConfig sync fails with `AmbiguousOverride`

**Diagnosis:**
```bash
kubectl describe krakendautoconfig <name>
```
The `Synced` condition is `False` with reason `AmbiguousOverride` and message
`spec.overrides reference operationIds that more than one operation declares:
<ids>`, naming at most five and then "and N more". A matching `Warning` event is
also emitted.

**Cause:** the OpenAPI spec declares an `operationId` that an override targets
on more than one operation. An override would land on only one of them, so the
sync fails closed instead of applying it to an arbitrary operation, and
existing `KrakenDEndpoints` keep their last-good state. The check runs on every
operation in the spec, before `spec.filter` applies.

**Resolution:** make the operationIds unique in the spec. To find the
duplicates, fetch the spec (convert YAML with `yq -o=json`) and run:

```bash
curl -s <spec-url> | jq -r '[.paths[][]? | objects | .operationId? // empty] | group_by(.) | map(select(length > 1) | .[0])[]'
```

### `kubectl apply` rejected: `audience` must be a list of strings

**Symptom:** `kubectl apply` (or a CI check running admission) rejects a
`KrakenDAutoConfig` with `documentation/openapi.audience must be a list of
strings, e.g. ["internal"]`.

**Cause:** `extraConfig`'s `documentation/openapi.audience` — on the
AutoConfig's `spec.overrides[]`, `spec.defaults.endpoint`, or
`spec.additionalEndpoints[]` — was set to something other than a list of
strings (a YAML mapping is the usual mistake). Left unchecked, this would pass
CRD and CUE validation unchanged but fail `krakend check -t -n -c`, blocking
config updates for every service on that gateway, not just the one with the bad
value.

**Resolution:** Set `audience` to a list, e.g. `["internal"]` or `["public",
"partner"]`. If `audience` is instead declared directly on the OpenAPI
operation (not via `extraConfig`), the same list-of-strings requirement is
enforced by the default CUE definitions at sync time, surfacing as
`CUEEvaluationFailed` instead of an admission rejection.

A `KrakenDEndpoint` gets the same message on `spec.endpoints[i].extraConfig`
when an added or changed entry carries a malformed audience, on CE and EE
gateways alike; set it to a list of strings. What `krakend check` finds in an
entry is reported by the gateway-wide check, as in *Admission rejects an
endpoint with a krakend finding*; the entry rules and the duplicate-route
check report their own causes.

### Forcing an immediate AutoConfig reconcile

`trigger: OnChange` AutoConfigs are re-polled every 5 minutes even with no
watch event; `trigger: Periodic` AutoConfigs resync at `spec.periodic.interval`.
To converge sooner — after fixing an upstream spec, correcting an override, or
restoring a hand-edited generated endpoint — change any annotation to trigger
an immediate reconcile (the watch predicate reacts to any annotation change,
not a specific key):

```bash
kubectl annotate krakendautoconfig <name> -n <ns> krakend.io/resync="$(date +%s)" --overwrite
```

### License expiry warnings

EE gateway licenses are checked on every gateway reconcile, at each expiry
boundary, and at least every 5 minutes. When a license enters its warning
window (`spec.license.expiryWarningDays`, default 30):

1. `LicenseValid` stays `True` with reason `LicenseExpiringSoon`
2. One `LicenseExpiringSoon` warning event is emitted
3. The `krakend_operator_license_expiry_seconds` metric keeps decreasing

**Resolution:** renew the license and update the Kubernetes Secret. The
gateway reconciles on the Secret change and sets `LicenseValid` back to
`LicenseOK`. The pods roll automatically when the license in the Secret
changes (the pod template's `krakend.io/checksum-license` annotation), so
every pod starts with the new license; no manual restart is needed. Until the
new pods are available the gateway shows `Progressing=True` (reason
`DeploymentUpdated`) and is not `Ready`. If the Secret cannot be read, nothing
rolls.

If the license reaches its safety buffer (1 h before expiry) or expires and
`fallbackToCE` is enabled, the gateway falls back to CE:

- `LicenseDegraded` is `True` (reason `LicenseFallbackCE`) and the gateway
  transitions to `Degraded` phase, with `CEFallbackApplied=True`
- The `CEFallbackApplied` message lists every Enterprise-only feature removed from
  the config (wildcard endpoints, namespaces such as `auth/api-keys`); each
  affected KrakenDEndpoint shows the same list under `Accepted` reason
  `EEFeaturesStripped`. **Check it: removed authentication means those
  routes are now unauthenticated.**
- OpenAPI export and the `openapi-serve` sidecar are off until EE returns

Without `fallbackToCE`, `LicenseExpired` is `True` and the gateway reports
phase `Error`.

---

## Scaling

### Operator Replicas

The chart runs two replicas by default, with a PodDisruptionBudget allowing one voluntary disruption and a preference for different nodes. Leader election (`krakend-operator-leader` Lease) keeps one replica running the controllers; every replica serves the admission webhooks and is Ready only once its webhook server is serving, so a rollout or node drain does not route admission requests to a pod whose webhook server is not listening. Readiness does not cover cache sync: a request that arrives before the caches have synced waits up to the 12 s budget and then fails closed with a 500. Every replica, not only the leader, reloads the webhook and metrics serving certificates when they are renewed. The chart refuses `replicaCount` > 1 with `leaderElection.enabled: false`.

The active replica reconciles up to 4 KrakenDAutoConfigs at once, because each reconcile fetches its OpenAPI spec over the network and a slow upstream should delay only its own AutoConfig. Set `--autoconfig-max-concurrent-reconciles` (chart value `autoconfig.maxConcurrentReconciles`, default `4`; values below 1 mean 1) to change it. The AutoConfig config checks and the policy controller's checks hold at most 1 of the pod's 3 validation slots between them, and the gateway controller at most 1, so the controllers never hold more than 2 of the 3 slots, however many workers there are. Concurrent admission requests can take the rest.

The gateway controller remembers its verdicts in memory only. After a restart or
a leader failover, each gateway whose whole render fails checks every one of its
endpoints once more (when the render is the applied config, only the endpoints
that lost an entry are judged first, and the rest runs only if one of them
fails), one at a time on the gateway controller's single worker (about 0.1 s per
endpoint at the 500m CPU limit; about 50–60 s for a gateway of 500 endpoints),
and other gateways wait behind it. Gateways whose render passes check only the
root, the whole render and the endpoints that lost an entry. The checks are not
parallelised, so that a validation slot stays free for admission. Raising the
CPU limit shortens them.

The operator caches only metadata for Secrets and ConfigMaps (names, labels and owners, without annotations or `managedFields`), so no Secret data or ConfigMap payload is cached and its memory no longer grows with their size (one small metadata entry per object remains). A reconcile reads the content it needs live from the API server: a gateway reads its plugin ConfigMaps and license Secret in full, and an AutoConfig reads its spec and auth sources in full, the namespace's `krakend-cue-definitions` ConfigMap once in full (falling back to the embedded definitions) and the `cue.definitionsConfigMapRef` ConfigMap once in full when it is set (a missing one fails the sync with `CUEEvaluationFailed`). Each of those reads is an API request, so API server latency shows up in reconcile time.

### Gateway Replicas

Set `spec.replicas` on the `KrakenDGateway` resource. With `spec.autoscaling` set, the operator creates an HPA targeting the gateway Deployment, starts a new Deployment at `spec.autoscaling.minReplicas` (1 when unset), and never writes the replica count again: the HPA owns it, and `spec.replicas` is ignored (the webhook warns when both are set).

---

## Pre-Upgrade Admission Audit

`operator/hack/audit-admission-rules.sh` lists stored KrakenD objects that
break the rules the operator's CRDs and admission webhooks enforce. It is
read-only: it runs `kubectl get` for the four kinds and `jq`, and writes
nothing to the cluster; it keeps a temporary 0700 copy of the objects that is
removed on exit. Run it from a checkout of the operator repository (it reads
`operator/internal/renderer/eeonly_namespaces.json`, the same Enterprise-only
list the renderer uses), with `kubectl` pointing at the cluster:

```bash
operator/hack/audit-admission-rules.sh
```

To audit a saved snapshot instead, point it at a directory holding
`endpoints.json`, `gateways.json`, `autoconfigs.json` and
`backendpolicies.json` (each the output of `kubectl get <kind> -A -o json`):

```bash
operator/hack/audit-admission-rules.sh ./snapshot
```

No output means none of the checks found anything. It does not check other
reserved paths under `/__debug`, `/__echo` and `/__health` (a GET on the
gateway's own health path is reported), unnamed `/*` wildcards on CE
gateways, unknown `urlPattern` placeholders or cross-method `auto_options`
clashes. Admission rejects those only on a new or changed object. A stored
health-path entry is that endpoint's own failure, so the gateway excludes the
endpoint (`EndpointInvalid`) and applies the rest; a stored `auto_options`
clash is resolved oldest-first, and the newer entry is left out. A root that
fails on its own keeps the gateway at its last applied config. The lines are:

| Line | Meaning |
|---|---|
| `KrakenDEndpoint ns/name: <field> <value>; ...` | The object breaks the listed CRD rules. The field is a path such as `spec.endpoints[0].timeout`. A duration reads `does not fit in 64 bits of nanoseconds` when it matches the pattern but overflows. |
| `KrakenDGateway ns/name: ...`, `KrakenDAutoConfig ns/name: ...` | The same, for gateways and AutoConfigs. |
| `gateway "ns/name": GET "/a/{id}" (ns/one) vs GET "/a/{name}" (ns/two)` | Two entries a gateway would route as one: the same method and path once parameter names are erased and the path is cleaned. Both entries are listed with their objects. Entries generated by one AutoConfig do not clash with each other here; the next line covers them. |
| `KrakenDAutoConfig ns/name: endpoints share a route and the operator holds all but the one the gateway serves, unless one of them is a rename in flight: GET "/h/{a}" (ns/one) vs GET "/h/{b}" (ns/two)` | Two endpoints one AutoConfig controls share a method and route shape. The gateway serves only one, so after the upgrade the AutoConfig holds every other as `ConfigValidationFailed` (`Synced=False`, `OperationsFailed`) and keeps its stale endpoints until the pair is resolved. The audit cannot tell a stale endpoint from a generated one, so a pair where one is the old endpoint of an operation being renamed is also listed, and is not held: the old one is deleted once the new one is written. |
| `KrakenDEndpoint ns/name: GET /x is the health path of gateway ns/gw` | A GET entry on the gateway's health path. `krakend check` accepts it, and the controller's route check rejects it on its own, so the gateway excludes the endpoint (`EndpointInvalid`) and applies the rest. |
| `KrakenDEndpoint ns/name: Enterprise-only on CE gateway ns/gw: ...` | The entry or a backend uses an Enterprise-only `extraConfig` namespace that KrakenD CE ignores. |
| `KrakenDBackendPolicy ns/name: Enterprise-only on CE gateway ns/gw: spec.raw ...` | A policy an endpoint of a CE gateway references carries an Enterprise-only namespace. |

What a listed object blocks, and how to fix it, is in the Pre-Upgrade
Checklist of the upgrade guide. To fix a line, change the named field in the
object (or delete the stale object), then run the audit again. For an
Enterprise-only line on a CE gateway, remove the namespace or move the gateway
to EE. The audit judges Enterprise-only names by the list the renderer strips,
so a namespace missing from that list is never reported.

---

## Backup and Recovery

### CRD Resources

All state is in Kubernetes CRD resources. Back up with:

```bash
kubectl get krakendgateways,krakendendpoints,krakendbackendpolicies,krakendautoconfigs -A -o yaml > krakend-backup.yaml
```

### Config Restoration

The operator is fully declarative. Restoring CRD resources triggers reconciliation, which regenerates the KrakenD config, Deployment, and all owned resources.

```bash
kubectl apply -f krakend-backup.yaml
```

Roll a gateway back by reverting its CRs, not with `kubectl rollout undo`.
The operator owns the Deployment's pod template and restores it, and a
reverted-to ReplicaSet's config ConfigMap may already have been collected.

---

## Log Analysis

Every log line is one OpenTelemetry log record in JSON, written to **stdout**. Releases before OpenTelemetry wrote zap's console text to stderr; a log agent that reads only stderr must read stdout instead. That covers the operator's own records, controller-runtime's, client-go's (klog) and the Go HTTP servers' (TLS handshake errors). Records are written as they are logged, so none is lost at exit.

| Field | Meaning |
|---|---|
| `Timestamp` | When the record was logged |
| `SeverityText` | `INFO`, `ERROR`, or `DEBUG4` … `DEBUG` (verbosity 1 … 4) and `TRACE4` … `TRACE` (verbosity 5 … 8) |
| `Body.Value` | The message |
| `Attributes` | The key/value pairs, as `{"Key":…,"Value":{"Type":…,"Value":…}}`: `controller`, `namespace`, `name`, `reconcileID`, … Values the log bridge cannot render natively (such as controller-runtime's `reconcileID`) are written as text, so `reconcileID` is the bare UUID, the same as the span's `controller_runtime.reconcile_id` |
| `exception.message`, `exception.type` (in `Attributes`) | On a record logged with an error: the error's text and its Go type. There is no `error` attribute |
| `TraceID`, `SpanID` | The span the record was logged in (all zeros outside one) |
| `Scope.Name` | The logger: `krakend-operator/setup`, `krakend-operator/controller-runtime/metrics`, `krakend-operator/klog`, … |
| `Resource` | `service.name`, `service.version`, `k8s.pod.name`, `k8s.namespace.name` (the operator's own namespace) |

Error records carry no stack trace. Earlier releases added one to error records in development mode (`--zap-devel`); the error's text and type are what remains.

```bash
# Errors, with their text
kubectl -n krakend-operator-system logs deploy/krakend-operator-controller-manager \
  | jq -c 'select(.SeverityText == "ERROR") | {t: .Timestamp, msg: .Body.Value, error: ([.Attributes[] | select(.Key == "exception.message") | .Value.Value][0])}'

# One reconcile, by the reconcileID controller-runtime gives it
kubectl -n krakend-operator-system logs deploy/krakend-operator-controller-manager \
  | jq -c --arg id "<reconcileID>" 'select(any(.Attributes[]; .Key == "reconcileID" and .Value.Value == $id))'
```

Error records, unlike spans, can quote krakend's output, which may hold tenant values. Restrict access to the logs accordingly, and to the trace backend as well, since spans name objects, namespaces and redacted URLs.

**Level and format.**
- `--zap-log-level` keeps its values (`debug`, `info`, `error`, `panic`, or an integer N for verbosity N).
- `--zap-devel` (on by default) means `debug`; `--zap-devel=false` logs at `info`.
- `--log-format=pretty` (Helm: `telemetry.logs.format`) indents each record for reading by hand. `--zap-encoder=console` selects the same; `json` keeps JSON. Both flags ignore case.
- `--zap-stacktrace-level` and `--zap-time-encoding` are still accepted and ignored, and a startup record names them.

**`k8s.namespace.name` in queries.** In the log record's `Resource` it is the operator's own namespace. Scope a query for one object by the `namespace` and `name` attributes of the record, not by the resource attribute.

**Diagnosing the telemetry itself.**
- A failure of the OTLP export (a collector that cannot be reached, a rejected batch) is an `ERROR` record with the message `OpenTelemetry pipeline error` and the scope `opentelemetry`. It is written to stdout only, never to OTLP, so a failing exporter cannot queue records for itself.
- A malformed OTLP header or endpoint variable does not stop the operator: that signal is not exported over OTLP, and the startup `ERROR` record `ignoring part of the telemetry configuration` names the variable, never its value. A malformed `OTEL_RESOURCE_ATTRIBUTES` entry is left out and named in the same record.
- grpc-go's own log, which the OTLP gRPC exporters use, goes to stdout only, never to OTLP, as the `opentelemetry/grpc` logger: a collector that cannot be reached makes grpc-go warn at every reconnect, and those records must not queue for the exporter that failed. Its info records are at verbosity 2. Until logging is installed, while the exporters are built, they are dropped.

**Lines that do not go through OpenTelemetry.** These are written to stderr; a dependency that wrote to stderr directly would add one:
- flag parsing errors and `--help` (`--help` exits 0, a flag error exits 2);
- an invalid logging flag (exit 2);
- an `OTEL_*` setting the operator cannot use, such as an unsupported `OTEL_*_EXPORTER` or OTLP protocol, printed as `setting up telemetry: …` (exit 1);
- OpenTelemetry's own warnings about its `OTEL_*` variables while the exporters start, before logging is set up;
- a failure of the final flush at shutdown, printed as `flushing telemetry: …`;
- Go runtime crashes.

**Shutdown.** On `SIGTERM` the manager stops, waiting at most 4 seconds for its controllers and servers (its graceful shutdown timeout), then the operator flushes the batched OTLP traces, metrics and logs, waiting at most 5 seconds, so that both fit the pod's 10 second termination grace period. Only OTLP data can be cut off by that limit. When the leader-election lease is lost the process exits the same way, after at most the same wait. A second signal exits at once, without the flush.
