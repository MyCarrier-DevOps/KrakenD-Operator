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
| `/readyz` | Readiness — remove from service if failing |

Verify manually:

```bash
kubectl -n krakend-operator-system port-forward deploy/krakend-operator-controller-manager 8081
curl -s http://localhost:8081/healthz  # {"status":"ok"}
curl -s http://localhost:8081/readyz   # {"status":"ok"}
```

---

## Prometheus Metrics

Metrics are exposed on port **8443** (HTTPS). Key metrics:

| Metric | Type | Description |
|---|---|---|
| `config_renders_total` | Counter | Total config renders |
| `config_validation_failures_total` | Counter | Config validation failures |
| `rolling_restarts_total` | Counter | Rolling restarts triggered |
| `license_expiry_seconds` | Gauge | Seconds until license expiry (per gateway) |
| `endpoints` | Gauge | Number of endpoints (per gateway) |
| `dragonfly_ready` | Gauge | Dragonfly readiness (1/0 per gateway) |
| `reconcile_duration_seconds` | Histogram | Reconcile loop duration |
| `gateway_info` | Gauge | Gateway metadata labels |
| `autoconfig_synced` | Gauge | 1 after an AutoConfig's last sync succeeded, 0 while it's failing (per AutoConfig); removed when the AutoConfig is deleted |

### Recommended Alerts

```yaml
# License expiring within 7 days
- alert: KrakenDLicenseExpiringSoon
  expr: license_expiry_seconds < 604800
  for: 1h
  labels:
    severity: warning

# License expired
- alert: KrakenDLicenseExpired
  expr: license_expiry_seconds <= 0
  for: 5m
  labels:
    severity: critical

# Repeated validation failures
- alert: KrakenDConfigValidationFailures
  expr: rate(config_validation_failures_total[5m]) > 0
  for: 10m
  labels:
    severity: warning

# Reconcile taking too long
- alert: KrakenDSlowReconcile
  expr: histogram_quantile(0.99, rate(reconcile_duration_seconds_bucket[5m])) > 30
  for: 15m
  labels:
    severity: warning

# AutoConfig hasn't synced — its endpoints are frozen at the last good state
- alert: KrakenDAutoConfigNotSynced
  expr: krakend_operator_autoconfig_synced == 0
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: "AutoConfig {{ $labels.namespace }}/{{ $labels.name }} has not synced for 15 minutes; its endpoints are frozen at the last good state"
```

---

## Gateway Lifecycle

### Phases

`status.phase` is derived from the `Ready` condition on every reconcile and
kept for compatibility; alert and gate on `Ready` instead.

| Phase | Ready | Meaning |
|---|---|---|
| `Pending` | `Unknown` | No configuration has been validated yet |
| any serving phase | `Unknown`, reason `ValidatorUnavailable` | The validator could not run; the last applied configuration keeps serving and validation is retried with backoff |
| `Deploying` | `False` | A rollout is in progress, or the Deployment has not reported available replicas yet |
| `Running` | `True` | Configuration applied, all replicas available |
| `Degraded` | `False` | EE license expired or in the pre-expiry window; running on CE (`LicenseDegraded=True`) |
| `Error` | `False` | Configuration rejected (`ConfigValid=False`), rollout failed (`Available=False`), or license expired without CE fallback |

`Rendering` and `Validating` stay in the CRD enum only so stored objects keep
validating; the operator does not write them.

### Common Conditions

| Condition | Meaning |
|---|---|
| `Ready` | Summary: the applied configuration is served on all replicas; its reason names the blocking condition |
| `ConfigValid` | `True`: the rendered config passed `krakend check`. `False` (`ConfigValidationFailed`): rejected; the last applied config keeps serving. `Unknown` (`ValidatorUnavailable`): krakend check could not run; retried with backoff |
| `Available` | Deployment has ready replicas |
| `Progressing` | Deployment rollout in progress |
| `DragonflyReady` | DragonflyDB instance is operational |
| `IstioConfigured` | VirtualService has been reconciled |
| `LicenseValid` | License secret is present and not expired |
| `LicenseExpiringSoon` | License will expire within the safety buffer |
| `LicenseSecretUnavailable` | License secret could not be read |

---

## Endpoint Status

| Condition | Meaning |
|---|---|
| `ResolvedRefs` | Gateway and referenced policies exist (endpoint controller) |
| `Accepted` | Included in the gateway's validated configuration (gateway controller) |
| `Ready` | Both of the above, for the current generation |

- **`Ready=Unknown`, reason `Pending`, for more than a few seconds:** the
  gateway has not accepted this generation. Check the gateway:
  `kubectl get krakendgateway <gw> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="ConfigValid")]}'`.
  A rejected configuration keeps every changed endpoint `Pending`.
- **`Accepted=False`, reason `EndpointConflict`:** an older KrakenDEndpoint
  owns one of this endpoint's (path, method) pairs; those entries are not
  served. Rename the route or remove the duplicate.
- **`ResolvedRefs=False`:** create the named gateway or policy; the endpoint
  recovers on its own.

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
- `ProgressDeadlineExceeded` — sets `Available=False`/`RolloutFailed` (phase `Error`) with a `RolloutFailed` event; the gateway returns to `Running` once the rollout recovers

### Gateway stuck in `Error`

**Diagnosis:**
```bash
kubectl describe krakendgateway <name>
kubectl get events --field-selector involvedObject.name=<name> --sort-by='.lastTimestamp'
```

**Common causes:**
- Config validation failure — check the `ConfigValid` condition message. It carries at most 4 KiB of krakend check output; the full output is in the operator log, message `krakend check rejected the rendered config`.
- License missing for EE gateway — provide license secret
- Rollout timeout — check Deployment events

### Gateway reports `ValidatorUnavailable`

**Symptom:** `ConfigValid` is `Unknown` with reason `ValidatorUnavailable`; the gateway keeps serving its last applied config and new changes are not rolled out.

**Diagnosis:** the condition message carries the cause. `no such file or directory` means the operator image lacks `/usr/local/bin/krakend`; `context deadline exceeded` means a run exceeded 30 seconds (check the operator pod's CPU throttling and memory); `signal: killed` without `context deadline exceeded` means the process was killed, usually by memory pressure on the operator container (a timeout's message also ends in `signal: killed`); `creating temp file` or `writing config to temp file` means the operator's temp directory is unwritable or full; `preparing validation copy` means the validation copy of the rendered config could not be built.

**Resolution:** fix the environment; the operator retries with exponential backoff and the gateway recovers on its own. Backoff grows up to about 16–17 minutes, so recovery can lag that long after the cause is fixed. Editing the gateway, or restarting the operator, retries at once. Reverting the change that could not be validated also clears the condition: once the render equals the applied configuration again, `ConfigValid` returns to `True`.

### Endpoint shows `Invalid`

**Diagnosis:**
```bash
kubectl describe krakendendpoint <name>
```

**Common causes:**
- `policyRef` references a non-existent policy (`ResolvedRefs=False`, reason `PolicyNotFound`)

A `gatewayRef` to a non-existent gateway shows phase `Detached` instead
(`ResolvedRefs=False`, reason `GatewayNotFound`). For what each condition
means, see [Endpoint Status](#endpoint-status).

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
  resolves against the URL of the document that contains it. A pointer not found, a resolution cycle, or
  a schema-name collision between two `$ref`s are only logged as warnings and
  don't block the sync.
- CUE evaluation error (check embedded/custom CUE definitions) — including a
  `documentation/openapi.audience` declared directly on an operation that
  isn't a list of strings
- Filter excludes all operations
- An override's `operationId` doesn't match any operation in the spec — see *AutoConfig sync fails with `UnmatchedOverride`* below
- Generated endpoints can't be written: the `Synced` condition is `False` with reason `EndpointReconcileFailed` and the message names the endpoint and the API error (e.g. an admission webhook rejected it, or a `KrakenDEndpoint` with that name is controlled by another owner). This retries with backoff regardless of `trigger`. A `Conflict` or `AlreadyExists` from a stale cache is *not* this failure — it requeues quietly a second later with no error or event, so if the AutoConfig converges a moment later with nothing in between, that's this path working as intended, not a bug.

### AutoConfig sync fails with `UnmatchedOverride`

**Diagnosis:**
```bash
kubectl describe krakendautoconfig <name>
```
The `Synced` condition is `False` with reason `UnmatchedOverride` and message
`spec.overrides reference operationIds not present in the OpenAPI spec: <ids,
comma-separated>`. A matching `Warning` event is also emitted.

**Common causes:**
- A `spec.overrides[].operationId` doesn't match any operationId in the fetched OpenAPI spec (e.g. the service doesn't declare an operationId for that operation at all)
- If the operationId *is* in the spec, check the AutoConfig's `CUEEvaluationWarning` events — the operation may have been skipped during evaluation

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

### `kubectl apply` rejected: `audience` must be a list of strings

**Symptom:** `kubectl apply` (or a CI check running admission) rejects a
`KrakenDAutoConfig` or `KrakenDEndpoint` with `documentation/openapi.audience
must be a list of strings, e.g. ["internal"]`.

**Cause:** `extraConfig`'s `documentation/openapi.audience` — on an
AutoConfig's `spec.overrides[]`, `spec.defaults.endpoint`, or
`spec.additionalEndpoints[]`, or on a `KrakenDEndpoint`'s
`spec.endpoints[]` — was set to something other than a list of strings (a
YAML mapping is the usual mistake). Left unchecked, this would pass CRD and
CUE validation unchanged but fail `krakend check -t -n -c`, blocking config
updates for every service on that gateway, not just the one with the bad
value.

**Resolution:** Set `audience` to a list, e.g. `["internal"]` or `["public",
"partner"]`. If `audience` is instead declared directly on the OpenAPI
operation (not via `extraConfig`), the same list-of-strings requirement is
enforced by the default CUE definitions at sync time, surfacing as
`CUEEvaluationFailed` instead of an admission rejection.

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

The license monitor checks EE gateway licenses periodically. When a license is expiring soon:

1. The `LicenseExpiringSoon` condition is set on the gateway
2. A warning event is emitted (rate-limited to once per 24h per gateway)
3. The `license_expiry_seconds` metric decreases

**Resolution:** Renew the license and update the Kubernetes Secret. The monitor will detect the change and clear the condition.

If the license expires and `fallbackToCE` is enabled:
- Gateway transitions to `Degraded` phase
- EE-only features are disabled
- Gateway continues operating with CE feature set

---

## Scaling

### Operator Replicas

The operator uses leader election (`krakend-operator-leader` lease). You can run multiple replicas for high availability, but only one will be active at a time.

### Gateway Replicas

Set `spec.replicas` on the `KrakenDGateway` resource. With `spec.autoscaling` set, the operator creates an HPA targeting the gateway Deployment, starts a new Deployment at `spec.autoscaling.minReplicas` (1 when unset), and never writes the replica count again: the HPA owns it, and `spec.replicas` is ignored (the webhook warns when both are set).

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

---

## Log Analysis

Operator logs use structured JSON logging (controller-runtime). Key fields:

| Field | Description |
|---|---|
| `controller` | Which controller emitted the log |
| `namespace` | Resource namespace |
| `name` | Resource name |
| `reconcileID` | Unique ID per reconcile invocation |

```bash
kubectl -n krakend-operator-system logs deploy/krakend-operator-controller-manager -f
```

Filter for errors:

```bash
kubectl -n krakend-operator-system logs deploy/krakend-operator-controller-manager | jq 'select(.level == "error")'
```
