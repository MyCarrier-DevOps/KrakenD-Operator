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
on). Key metrics:

| Metric | Type | Description |
|---|---|---|
| `config_renders_total` | Counter | Total config renders |
| `config_validation_failures_total` | Counter | Config validation failures |
| `rolling_restarts_total` | Counter | Deployment writes that changed the pod template, once per write (a creation does not count; drift in the template the operator reverts does) |
| `license_expiry_seconds` | Gauge | Seconds until license expiry (per gateway) |
| `endpoints` | Gauge | Number of endpoints (per gateway) |
| `dragonfly_ready` | Gauge | Dragonfly readiness (1/0 per gateway) |
| `reconcile_duration_seconds` | Histogram | Reconcile loop duration |
| `gateway_info` | Gauge | Gateway metadata labels (edition, version); one series per gateway |
| `gateway_config_valid` | Gauge | 1 while the gateway's newest config passed validation, 0 while it is rejected or unjudged (per gateway); removed when the gateway is deleted |
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

# A gateway's newest config is rejected or could not be validated (it keeps
# serving the last applied one)
- alert: KrakenDGatewayConfigRejected
  expr: krakend_operator_gateway_config_valid == 0
  for: 15m
  labels:
    severity: warning

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
| `Error` | `False` | Configuration rejected (`ConfigValid=False`), a plugin ConfigMap missing (`PluginsResolved=False`), rollout failed or the Deployment lost availability (`Available=False`), or license expired without CE fallback |

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
| `Ready` | Summary: the Deployment is available and the applied config is rolled out to all replicas; its reason names the blocking condition |
| `ConfigValid` | `True`: the rendered config passed `krakend check`. `False` (`ConfigValidationFailed`): rejected; the last applied config keeps serving. `Unknown` (`ValidatorUnavailable`): krakend check could not run; retried with backoff |
| `PluginsResolved` | `True` (`ConfigMapsFound`) when every plugin ConfigMap exists; `False` (`ConfigMapNotFound`) naming the missing ones while the Deployment is held. Absent without ConfigMap plugin sources |
| `Available` | The Deployment is available; `False` when it loses its minimum replicas (for example all pods crash-looping) or its rollout fails |
| `Progressing` | A rollout is in progress: the Deployment was created, its pod template was written (a config, image, plugin or license change, but also resources, probes or drift the operator reverted), or old pods remain beside updated ones. It stays `True` until the Deployment has observed the change and every replica is updated and available. A replica change alone (an HPA scale) does not raise it, though the brief `Available=False` a scale-up can cause is still mirrored |
| `DragonflyReady` | DragonflyDB instance is operational |
| `IstioConfigured` | VirtualService has been reconciled |
| `LicenseValid` | EE license state: `True` (`LicenseOK`), `True` (`LicenseExpiringSoon`) inside the warning window, `False` (`LicensePreExpiry`, `LicenseExpired`), or `Unknown` (`LicenseSecretMissing`) while the license cannot be read, unless the last known expiry is already inside the safety buffer or past |
| `LicenseExpired` | `True` once the license is expired or inside the 1 h safety buffer; without `fallbackToCE` the gateway reports phase `Error` |
| `LicenseDegraded` | `True` (`LicenseFallbackCE`) while the gateway runs CE because its license expired or entered the 1 h safety buffer |
| `LicenseSecretUnavailable` | License secret could not be read or parsed; `LicenseValid` is `Unknown` meanwhile. The stage is still judged from the last known expiry (`status.licenseExpiry`), so a `fallbackToCE` gateway falls back to CE when that expiry enters the 1 h safety buffer. Once that expiry is inside the buffer or past, `LicenseValid` shows the stage (`False`, reason `LicensePreExpiry` or `LicenseExpired`) instead of `Unknown` |
| any of `DragonflyReady`, `IstioConfigured`, `LicenseSecretUnavailable` with reason `CRDNotInstalled` | The feature is enabled but its CRD (Dragonfly Operator, Istio, External Secrets Operator) is not installed. Install it, then restart the operator so it also watches the kind |

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
- **`Accepted=False`, reason `EndpointConflict`:** older KrakenDEndpoints
  serve every one of this endpoint's (path, method) pairs, so none is served
  (when only some are lost the reason is `PartiallyAccepted`, below). Rename
  the route or remove the duplicate.
- **`ResolvedRefs=False`:** create the named gateway or policy; the endpoint
  recovers on its own.
- **`Ready=True`, reason `SchemaNameConflict`:** the endpoint is served, but it
  defines a component schema differently from the endpoint the gateway's
  published documentation takes that name from. See
  [Endpoint shows `Ready` reason `SchemaNameConflict`](#endpoint-shows-ready-reason-schemanameconflict).

---

## AutoConfig Status

| Condition | Meaning |
|---|---|
| `SpecAvailable` | The OpenAPI spec was fetched (`SpecFetched`), or not (`SpecFetchFailed`) |
| `Synced` | Generated endpoints are in sync with the spec. `False` with reason `OperationsFailed` while any operation is held, or with the failure's reason (`SpecFetchFailed`, `CUEEvaluationFailed`, `UnmatchedOverride`, `AmbiguousOverride`, `AdditionalEndpointScopeFailed`, `EndpointReconcileFailed`, `ValidatorUnavailable`) |
| `EndpointsReady` | Every generated endpoint's `Ready` is True for its current generation (`AllEndpointsReady`), or `False` (`EndpointsNotReady`) naming up to five endpoints and their reasons (`Pending` while the endpoint controller has not reported a new or changed endpoint) |
| `Ready` | `SpecAvailable`, `Synced` and `EndpointsReady` are True; its reason names the first that is not |

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
- Config validation failure — check the `ConfigValid` condition message. It
  lists each failure on its own line: `namespace/name spec.endpoints[i]: …`,
  `namespace/name: …` when the endpoint is known but no entry of it matches,
  or `gateway: …` when the failure names no endpoint, up to 4 KiB. Runtime
  route clashes read `… (GET /healthz is the gateway's own route)` for an
  endpoint on the custom health path, and `… conflicts with existing wildcard …`
  for clashes under `router.auto_options`. The route check is shared with
  admission and stops after 21 refused routes, ending with `route check stopped
  after 21 refused routes`, so the message and `GatewayConfigRejected` name at
  most the first 21, in a deterministic order. The full
  output is in the operator log, message `validation rejected the rendered config`. The gateway
  keeps serving the last applied config (`status.configChecksum`), and its
  Deployment (unless a plugin ConfigMap is missing, which holds it), Service
  and other resources are still reconciled. Only the rejected render waits for
  a fix.
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

**Symptom:** `ConfigValid` is `Unknown` with reason `ValidatorUnavailable`; the gateway keeps serving its last applied config and a new config is not rolled out (image, plugin and license changes still roll).

**Diagnosis:** the condition message carries the cause. `no such file or directory` means the operator image lacks `/usr/local/bin/krakend`; `context deadline exceeded` means a run exceeded 30 seconds (check the operator pod's CPU throttling and memory); `signal: killed` without `context deadline exceeded` means the process was killed, usually by memory pressure on the operator container (a timeout's message also ends in `signal: killed`); `creating temp file` or `writing config to temp file` means the operator's temp directory is unwritable or full; `preparing validation copy` means the validation copy of the rendered config could not be built.

**Resolution:** fix the environment; the operator retries with exponential backoff and the gateway recovers on its own. Backoff grows up to 5 minutes, so recovery can lag that long after the cause is fixed. Editing the gateway, or restarting the operator, retries at once. Reverting the change that could not be validated also clears the condition: once the render equals the applied configuration again, `ConfigValid` returns to `True`.

### Gateway Deployment not updated: "no ConfigMap holds the applied config"

The operator logs this when the ConfigMap for `status.configChecksum`
(`<gateway>-config-<hash>`) was deleted while a newer render is being
rejected. The Deployment is left as it is: running pods keep their config,
but a deleted Deployment cannot be recreated. Fix the rejected input
(`kubectl describe krakendgateway <name>`, condition `ConfigValid`). The next
config that passes validation is published and rolled out.

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

### Endpoint shows `Accepted=False`, reason `GatewayConfigRejected`

The gateway's newest config was rejected by `krakend check`, and a finding
names this endpoint. The gateway still serves its last applied config, so
this endpoint's latest change is not live.
`kubectl describe krakendendpoint <name>` shows the findings; fix the spec
they point at. The gateway's `ConfigValid` message lists every endpoint
named. The condition is removed as soon as no finding names the endpoint,
and cleared by a config that passes validation. A gateway that has never
applied a config leaves no `Accepted` on endpoints it has not accepted, so
they read `Pending` until one is applied.

### Endpoint shows `Accepted` reason `PartiallyAccepted`

Some of this endpoint's entries are served; the ones in `status.conflicts`
are not, because an older KrakenDEndpoint (`winner`) serves the same method
and route. Paths that differ only in parameter names are the same route, so
the winner may serve a differently named parameter path (for example
`/users/{id}` wins over `/users/{name}`). When `winner` is the endpoint itself,
an earlier entry of the same KrakenDEndpoint serves the route. Remove the
duplicate entry from one of the two, or move it to the KrakenDEndpoint that
should own it.

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

### Admission rejects an endpoint with a krakend finding

**Symptom:** `kubectl apply` of a KrakenDEndpoint fails with `The
KrakenDEndpoint "x" is invalid: spec.endpoints[1]: <message>`, where the
message is what `krakend check` reported for that entry (for example `undefined
output param`, or a wildcard conflict with a route of another endpoint).

**Cause:** The write renders the gateway's config with the change, and that
config failed validation although it passed before the change. The cause is on
the offending entry; a finding about another endpoint or the gateway root is on
`spec.endpoints` and names it (`team-a/orders spec.endpoints[0]: ...`,
`gateway: ...`).

**Resolution:** Fix the entry the cause names. If the denial blames another
endpoint, fix or remove that endpoint's clashing route; your change is only the
trigger. A gateway that already fails without the change (because of another
object, or of the endpoint's own stored version) does not block the write: the
change is judged with the gateway root alone and admitted with a warning
`gateway <ns>/<name> already fails validation without this change`, unless the
candidate fails there when its stored version did not (for a create or a move,
the baseline is the root by itself).
Fix the object the warning names, because until then the controller keeps the
gateway at its last-known-good config.

**`500 Internal Error: validating the gateway config: ...`:** the check could
not run: all three validation slots stayed busy for the 12 s budget, or the
validator itself failed. This is transient and `kubectl` does not retry it, so
run the command again. Controllers and GitOps tools retry on their own. If it
repeats, check the operator pod's CPU and memory.

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
  resolves against the URL of the document that contains it. A pointer not found, a resolution cycle, or
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
  - `ConfigValidationFailed`: the gateway config check failed on the operation's endpoint (the message is the `krakend check` finding), or two generated endpoints share a method and route shape (`has the same route as GET /x in <endpoint>`), or the operation was left unchecked (`not checked: N other operations failed the gateway config check first`; fix the others and it is checked on the next sync). For a route clash the advice in the message is written for KrakenDEndpoint authors and the status cuts it at 256 bytes: exclude one of the operations with `spec.filter`, or fix the upstream paths to use one parameter name. A `urlTransform` that collapses two operations onto one method and path is a misconfiguration the same way: one is published and the other is listed in `status.skipped` as a duplicate, and `spec.filter` cannot separate them, so fix the transform or the paths.
  - `EndpointRejected`: the API server or the webhook refused the write (the message says why, for example a missing `policyRef` policy), another object controls the endpoint's name, a label-matched endpoint could not be adopted, or the gateway is CE and the endpoint uses an Enterprise-only `extra_config` namespace (`not written: gateway ns/name runs CE, which ignores these Enterprise-only namespaces`). Fix the cause, move the gateway to EE, or exclude the operation with `spec.filter`.
- `Synced=False` with reason `ValidatorUnavailable`: `krakend check` could not run (or the reconcile was cancelled while it waited for a check slot), so nothing was written or deleted (it may still have adopted label-matched endpoints, which changes owner references only). It retries with backoff on either trigger. See *Gateway reports `ValidatorUnavailable`*.
- `status.skipped` lists operations given no endpoint by rule: HEAD, OPTIONS and TRACE operations (`UnsupportedMethod`, no event) and duplicates of an earlier operation (`DuplicateOperationId`: the same path and method, operationId or endpoint name). An operation `spec.filter` excludes is not listed.
- `status.warnings` lists spec problems that do not stop a sync but can leave the endpoints or the published documentation wrong: `$ref`s the resolver could not honour, colliding schema names, `#/…` refs inside fetched documents (resolved against the main spec), external `$ref`s in a ConfigMap-sourced spec, parameter `$ref`s that do not resolve (the operation using one is held; an unresolvable path-level ref holds every operation on the path), and schema references `components/schemas` does not define. One root cause can list two notes. A `SpecWarning` event is emitted when the inputs change, at most 20 input warning events per sync (`SpecWarning`, `DuplicateOperationId` and `AdditionalEndpointOverride` together). A dereferenced spec over 10 MiB fails the sync (`SpecFetchFailed`).
- A spec fetch that exceeds the 2-minute deadline for fetching and resolving external `$ref`s fails with `SpecFetchFailed` and `context deadline exceeded`; a single request is bounded at 30 seconds.
- Generated endpoints can't be written for a transient reason: the `Synced` condition is `False` with reason `EndpointReconcileFailed` and the message names up to five endpoints and the API error (for example the API server or an admission webhook timed out). Every endpoint is attempted, no stale endpoint is deleted, and it retries with backoff regardless of `trigger`, at least every 5 minutes. A write the API server rejects as invalid, or a name another object controls, is not this failure: it holds that operation (`OperationsFailed` above). A `Conflict` or `AlreadyExists` from a stale cache is *not* a failure either — it requeues quietly a second later with no error or event, so if the AutoConfig converges a moment later with nothing in between, that's this path working as intended, not a bug.
- `Ready=False` with reason `EndpointsNotReady` while `Synced` is `True`: the endpoints were written but one is not `Ready` (the `EndpointsReady` message names up to five and their reasons: `Pending` while the endpoint controller has not caught up, `EndpointConflict` when an older endpoint serves the route, `GatewayConfigRejected`, `GatewayNotFound`). See [Endpoint Status](#endpoint-status).
- Adoption and ownership: the AutoConfig tracks its endpoints by controller owner reference, not by labels. An endpoint carrying both `gateway.krakend.io/autoconfig=<name>` and `gateway.krakend.io/auto-generated=true` with no controller is adopted on the next reconcile (uncontrolled endpoints are not watched), then converged or deleted. Anyone who can label an uncontrolled endpoint in the namespace can therefore hand it to the AutoConfig, which deletes it if no operation generates it. An uncontrolled endpoint whose name an operation generates is taken over whatever its labels. An endpoint controlled by another object is never changed or deleted.
- Two AutoConfigs reconciling at once can check the same gateway without seeing each other's pending writes. The gateway controller still never publishes a failing render: it keeps its applied config and reports `ConfigValid=False`.
- A rename and a brand-new rejection in the same pass can fail the gateway's own check. The precheck models the stale endpoints as deleted unless the last status already lists a write of this pass as `EndpointRejected`; a rejection that first appears in the same pass stops the delete, so the gateway renders the old and the new endpoints together. The gateway keeps its applied config, `ConfigValid=False` names both entries, and it clears once the cause of the API server's 422 is fixed.
- A rename can show a transient gateway failure. Between the sibling's write and the stale delete, the gateway can render the mixed state of old and new endpoints. Expect a short `ConfigValid=False`, a Warning event and an increment of `krakend_operator_config_validation_failures_total`, then recovery once the stale endpoint is deleted.

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

The chart runs two replicas by default, with a PodDisruptionBudget allowing one voluntary disruption and a preference for different nodes. Leader election (`krakend-operator-leader` Lease) keeps one replica running the controllers; every replica serves the admission webhooks and is Ready only once its webhook server is serving, so a rollout or node drain never leaves admission without a backend. The chart refuses `replicaCount` > 1 with `leaderElection.enabled: false`.

The active replica reconciles up to 4 KrakenDAutoConfigs at once, because each reconcile fetches its OpenAPI spec over the network and a slow upstream should delay only its own AutoConfig. Set `--autoconfig-max-concurrent-reconciles` (chart value `autoconfig.maxConcurrentReconciles`, default `4`; values below 1 mean 1) to change it. The AutoConfig config checks hold at most 1 of the pod's 3 validation slots, and the gateway controller at most 1, so the controllers never hold more than 2 of the 3 slots, however many workers there are. Concurrent admission requests can take the rest.

The operator caches only metadata for Secrets and ConfigMaps (names, labels and owners, without annotations or `managedFields`), so no Secret data or ConfigMap payload is cached and its memory no longer grows with their size (one small metadata entry per object remains). A reconcile reads the content it needs live from the API server: a gateway reads its plugin ConfigMaps and license Secret in full, and an AutoConfig reads its spec and auth sources in full, the namespace's `krakend-cue-definitions` ConfigMap once in full (falling back to the embedded definitions) and the `cue.definitionsConfigMapRef` ConfigMap the same way when it is set. Each of those reads is an API request, so API server latency shows up in reconcile time.

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
clashes. Admission rejects those only on a new or changed object; a stored
`auto_options` or health-path clash makes the controller refuse the render, so
the gateway keeps its last applied config. The lines are:

| Line | Meaning |
|---|---|
| `KrakenDEndpoint ns/name: <field> <value>; ...` | The object breaks the listed CRD rules. The field is a path such as `spec.endpoints[0].timeout`. A duration reads `does not fit in 64 bits of nanoseconds` when it matches the pattern but overflows. |
| `KrakenDGateway ns/name: ...`, `KrakenDAutoConfig ns/name: ...` | The same, for gateways and AutoConfigs. |
| `gateway "ns/name": GET "/a/{id}" (ns/one) vs GET "/a/{name}" (ns/two)` | Two entries a gateway would route as one: the same method and path once parameter names are erased and the path is cleaned. Both entries are listed with their objects. Entries generated by one AutoConfig do not clash with each other here; the next line covers them. |
| `KrakenDAutoConfig ns/name: endpoints share a route and the operator holds all but the one the gateway serves, unless one of them is a rename in flight: GET "/h/{a}" (ns/one) vs GET "/h/{b}" (ns/two)` | Two endpoints one AutoConfig controls share a method and route shape. The gateway serves only one, so after the upgrade the AutoConfig holds every other as `ConfigValidationFailed` (`Synced=False`, `OperationsFailed`) and keeps its stale endpoints until the pair is resolved. The audit cannot tell a stale endpoint from a generated one, so a pair where one is the old endpoint of an operation being renamed is also listed, and is not held: the old one is deleted once the new one is written. |
| `KrakenDEndpoint ns/name: GET /x is the health path of gateway ns/gw` | A GET entry on the gateway's health path. `krakend check` accepts it, and the controller's route check rejects the render, so the gateway keeps its last applied config. |
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
