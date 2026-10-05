# Upgrade Guide — KrakenD Operator

## General Upgrade Procedure

### Via Helm

```bash
helm repo update
helm upgrade krakend-operator krakend-operator/krakend-operator \
  -n krakend-operator-system
```

### Via Kustomize

```bash
cd operator
make deploy IMG=ghcr.io/mycarrier-devops/krakend-operator:<new-version>
```

---

## Pre-Upgrade Checklist

1. **Read the release notes** for breaking changes
2. **Back up CRD resources**:
   ```bash
   kubectl get krakendgateways,krakendendpoints,krakendbackendpolicies,krakendautoconfigs -A -o yaml > pre-upgrade-backup.yaml
   ```
3. **Check current operator health**:
   ```bash
   kubectl -n krakend-operator-system get pods
   kubectl -n krakend-operator-system logs deploy/krakend-operator-controller-manager --tail=20
   ```
4. **Verify all gateways are in `Running` phase** before upgrading:
   ```bash
   kubectl get krakendgateways -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase
   ```
5. **Audit stored objects against the admission rules** (read-only: it only
   runs `kubectl get`; needs `jq` 1.7). Run it from a checkout of the
   operator repository, with `kubectl` pointing at the cluster you are
   upgrading:
   ```bash
   operator/hack/audit-admission-rules.sh
   ```
   It prints one line per object or conflict. No output means none of the
   checks found anything; it does not cover other reserved paths under
   `/__debug`, `/__echo` and `/__health` (a GET on the gateway's own health
   path is reported), unnamed `/*` wildcards on CE gateways, unknown
   `urlPattern` placeholders or cross-method `auto_options` clashes. Fix or
   knowingly accept each line before upgrading. What a listed object blocks
   depends on the rule it breaks:
   - A stored value that breaks a field rule (a pattern, an enum, a minimum, a
     length, a `tmpSizeLimit` outside the allowed forms, or an endpoint
     `timeout` or `cacheTTL` that is not a duration) keeps being accepted on
     unrelated updates; only a change to that field must fix it. Items of a
     list without per-item keys (an entry's `backends`, a KrakenDAutoConfig's
     `overrides` and `additionalEndpoints[].backends`) are re-checked on any
     edit to that list.
   - A duration that matches its pattern but does not fit in 64 bits of
     nanoseconds raises an evaluation error that is never ratcheted: every
     update to the object is rejected until it is fixed.
   - A rule written on `spec` itself (the Enterprise license sources and the
     OpenAPI port of a gateway; a KrakenDAutoConfig's `hostMapping`, Periodic
     interval and base path rules) is re-checked on any change to the spec. A
     KrakenDAutoConfig's source and auth rules sit on `spec.openapi`, so they
     are re-checked only when `spec.openapi` changes.
   - A KrakenDAutoConfig name over 63 characters is rejected on every write,
     including label, annotation and status writes; the object can only be
     deleted and recreated.
   - A `passwordFromSecret` on an Enterprise gateway keeps being accepted
     until you change the value or the edition.
   - A `more than 1024 entries` line blocks any change to that object's entry
     list, which re-checks `maxItems`.
   - A `duplicate entry` or `duplicate additionalEndpoint` line does not block
     writes: list-type uniqueness is skipped when the stored object already
     fails it, and server-side apply tolerates live duplicates. Remove the
     duplicate before it fails a fresh apply.
   - A listed route conflict is rejected when either conflicting entry is
     added or changed, or its object moves to another gateway; the same holds
     for an endpoint on its gateway's health path. `overrides collide` ratchets
     with the rest of the AutoConfig webhook rules.
   - A line naming an Enterprise-only namespace or field on a CE gateway is a
     feature KrakenD CE ignores today: remove it, or move the gateway to EE.
     Only a changed entry, a changed root `extraConfig`, a changed policy
     `raw`, a changed `spec.redis` or `spec.config.documentation`, or enabling
     `spec.openapi` or `spec.dragonfly`, is rejected; so is switching the
     gateway to CE while the last two are enabled. A stored use that stays
     keeps being accepted: editing the settings of a stored, enabled
     `spec.openapi` or `spec.dragonfly` on a CE gateway only warns, and turning
     one off is admitted.

---

## CRD Upgrades

CRDs are installed in the Helm chart's `crds/` directory. Helm installs CRDs on first install but **does not update them on upgrade** by default.

To update CRDs manually:

```bash
kubectl apply -f https://raw.githubusercontent.com/MyCarrier-DevOps/KrakenD-Operator/v<version>/charts/krakend-operator/crds/gateway.krakend.io_krakendgateways.yaml
kubectl apply -f https://raw.githubusercontent.com/MyCarrier-DevOps/KrakenD-Operator/v<version>/charts/krakend-operator/crds/gateway.krakend.io_krakendendpoints.yaml
kubectl apply -f https://raw.githubusercontent.com/MyCarrier-DevOps/KrakenD-Operator/v<version>/charts/krakend-operator/crds/gateway.krakend.io_krakendbackendpolicies.yaml
kubectl apply -f https://raw.githubusercontent.com/MyCarrier-DevOps/KrakenD-Operator/v<version>/charts/krakend-operator/crds/gateway.krakend.io_krakendautoconfigs.yaml
```

Or from a local checkout:

```bash
kubectl apply -f charts/krakend-operator/crds/
```

### RBAC (ClusterRole) copies must be kept in sync manually

The manager's ClusterRole exists in **three** places, only two of which are
kept in sync automatically:

- `operator/config/rbac/role.yaml` — generated by `make manifests`
  (controller-gen, from `+kubebuilder:rbac` markers).
- `operator/bundle/manifests/*_clusterrole.yaml` — generated by `make bundle`
  from the above (via kustomize).
- `charts/krakend-operator/templates/clusterrole.yaml` — the Helm chart's
  copy. **This one is hand-maintained.** Nothing regenerates or diffs it
  against `config/rbac/role.yaml`; `make verify-manifests` (see the
  Makefile) cannot catch it drifting, by construction (Helm charts sit
  outside the `config/`/`bundle/` kustomize-and-operator-sdk generation
  pipeline). Whenever a `+kubebuilder:rbac` marker changes (new API group,
  new verb, new resource), update `charts/krakend-operator/templates/
  clusterrole.yaml` BY HAND to match `config/rbac/role.yaml` as part of the
  same change — do not rely on CI to catch a missed update here.

---

## Post-Upgrade Verification

1. **Operator pod is running**:
   ```bash
   kubectl -n krakend-operator-system get pods
   ```

2. **Health probes pass**:
   ```bash
   kubectl -n krakend-operator-system port-forward deploy/krakend-operator-controller-manager 8081
   curl -s http://localhost:8081/healthz
   curl -s http://localhost:8081/readyz
   ```

3. **All gateways reconcile successfully**:
   ```bash
   kubectl get krakendgateways -A
   ```

4. **Check for error events**:
   ```bash
   kubectl get events -A --field-selector reason=ConfigValidationFailed,reason=RolloutFailed --sort-by='.lastTimestamp'
   ```

---

## Rollback

### Via Helm

```bash
helm rollback krakend-operator -n krakend-operator-system
```

### Via Kustomize

Redeploy the previous version:

```bash
make deploy IMG=ghcr.io/mycarrier-devops/krakend-operator:<previous-version>
```

> **Note:** CRD changes cannot be rolled back via Helm. If a CRD schema change is incompatible, restore from backup.

> **Downgrading the operator past *Gateway reconcile correctness*.** Expect
> these effects on every gateway:
>
> - The pods roll once more. The older operator builds a pod template that
>   mounts the ConfigMap named after the gateway and carries no
>   `krakend.io/image` annotation.
> - The older operator recreates the `<gateway>` ConfigMap. It does not know
>   the `<gateway>-config-<hash>` ConfigMaps, so it never collects them. They
>   stay until the gateway is deleted, because the gateway owns them.
> - `status.configEdition` is dropped. The CRD keeps the field, but the older
>   operator does not write it, and its next status write removes it.

> **Downgrading the operator past *Complete admission*.** The operator now puts
> the finalizer `gateway.krakend.io/policy-protection` on every
> KrakenDBackendPolicy. An older operator does not know it, so a policy that is
> deleted afterwards stays `Terminating` for good, and so does a namespace that
> holds one. Roll back first, then remove the finalizer from every policy: the
> current operator re-adds it on any reconcile, so removing it before the older
> operator runs does not last. Run the same command after you uninstall the
> operator while policies remain. It is idempotent:
>
> ```bash
> kubectl get krakendbackendpolicies -A -o json \
>   | jq -r '.items[] | .metadata as $m
>       | ($m.finalizers // [] | index("gateway.krakend.io/policy-protection")) as $i
>       | select($i != null) | "\($m.namespace) \($m.name) \($i)"' \
>   | while read -r ns name i; do
>       kubectl patch krakendbackendpolicy "$name" -n "$ns" --type=json -p "[
>         {\"op\":\"test\",\"path\":\"/metadata/finalizers/$i\",\"value\":\"gateway.krakend.io/policy-protection\"},
>         {\"op\":\"remove\",\"path\":\"/metadata/finalizers/$i\"}]"
>     done
> ```
>
> The `test` operation makes a patch fail, rather than remove another
> finalizer, if the list changed in the meantime.
>
> Removing the finalizer from a policy that is already `Terminating` and still
> referenced deletes it at once: its endpoints then report `PolicyNotFound` and
> drop out of the render. Repoint those endpoints first (the runbook lists them).

---

## Version Compatibility

| Operator Version | Kubernetes | KrakenD CE | Go |
|---|---|---|---|
| 0.x (alpha) | 1.33+ | 2.13+ | 1.26+ |

---

## Unreleased — AutoConfig continuous reconciliation, drift repair and resync

The AutoConfig controller is now watch-driven with a resync backstop, and
every reconcile runs the full pipeline (fetch, evaluate, filter, generate,
converge), instead of reacting once per spec change and otherwise sitting
idle.

**Reconciliation contract:**

- Watched: the `KrakenDAutoConfig`'s own spec (generation), label, and
  annotation changes (status-only updates — the phase/condition writes the
  controller makes to itself — are ignored); an owned `KrakenDEndpoint`'s
  spec changes and deletions; the OpenAPI spec ConfigMap
  (`spec.openapi.configMapRef`), which is now watched alongside the CUE
  definition ConfigMaps.
- Resync: `trigger: OnChange` AutoConfigs are additionally re-polled every 5
  minutes even with no watch event; `trigger: Periodic` AutoConfigs continue
  to resync at `spec.periodic.interval`. Expect an HTTP fetch of the OpenAPI
  spec roughly every 5 minutes per `OnChange` AutoConfig going forward —
  previously it fetched only on a watched change.
- Drift repair: every reconcile converges owned endpoints to the desired
  state while the AutoConfig syncs successfully, so a generated
  `KrakenDEndpoint` that was deleted or hand-edited out of band is restored on
  the next reconcile or resync. While the AutoConfig is in `Error`, existing
  endpoints are left as they are until a sync succeeds (an endpoint write
  failure stops convergence at the endpoint that failed).
- Steady state writes nothing: a reconcile that finds no change writes no
  status and emits no event. `status.lastSyncTime` and the
  `EndpointsGenerated` event update only when the spec/CUE-definitions/
  generation inputs or the generated endpoints changed; the
  `CUEEvaluationWarning`, `DuplicateOperationId`, and
  `AdditionalEndpointOverride` warning events fire only when those inputs
  differ from the last successful sync's. A failed sync doesn't record its
  inputs, so they repeat on each retry of a failing sync whose inputs
  changed; a spec fetch failure emits `SpecFetchFailed` instead.
- Endpoint write failures fail the sync: when a generated endpoint can't be
  created, updated, or deleted for a reason other than a write conflict (e.g.
  an admission webhook rejects it, or a `KrakenDEndpoint` of that name is
  controlled by another owner), the AutoConfig goes to `status.phase: Error`
  with `Synced=False`, reason `EndpointReconcileFailed`, and a matching
  `Warning` event. This always retries with backoff, on both `OnChange` and
  `Periodic` AutoConfigs — a `Periodic` AutoConfig no longer waits a whole
  `spec.periodic.interval` to retry what's usually a transient write error.
- Write conflicts retry quietly, not as a failure: a stale-cache `Conflict` on
  a status write, or a `Conflict`/`AlreadyExists` on an endpoint write (this
  reconcile raced another and lost), requeues one second later with no error
  log, no event, and no status change — it does not set
  `EndpointReconcileFailed` and does not touch `status.phase`. The
  `CUEEvaluationWarning`/`DuplicateOperationId`/`AdditionalEndpointOverride`
  warning events above are recorded only once the reconcile's own status
  write succeeds, so a reconcile that loses to a conflict doesn't re-emit
  them on its retry. The exception is a failed sync whose own status write
  conflicts: the sync still failed, so it keeps the failure's retry (backoff,
  or `spec.periodic.interval` where that applies) with no event from that
  attempt — a one-second requeue would reset the backoff.
- External `$ref` fetch/decode failures now fail closed: previously, a failed
  fetch or decode of an externally-`$ref`'d document was only a warning, and
  the raw `$ref` was left in the generated config — a transient failure could
  silently change the rendered endpoints. It now fails the sync the same way
  as an OpenAPI spec fetch failure: `status.phase: Error`, `SpecAvailable=False`
  and `Synced=False`, both with reason `SpecFetchFailed` and a message
  prefixed `resolving external $refs: `; existing `KrakenDEndpoints` are left
  as they were (last-good) until the reference is reachable again. A relative
  `$ref` inside an external document now resolves against that document's
  URL, not the main spec's (a `schemas/pet.json` that refers to
  `category.json` fetches `schemas/category.json`). A pointer not found, a
  resolution cycle, or a schema-name collision between two `$ref`s remain
  warnings, not failures. See *Before upgrading* below.
- A spec fetch failure now also sets `Synced=False` (reason
  `SpecFetchFailed`) alongside `SpecAvailable=False`, instead of leaving the
  last successful sync's `Synced=True`, so health checks that read
  conditions see the failure.
- New metric `krakend_operator_autoconfig_synced` (gauge, labels `namespace`,
  `name`): 1 after a successful sync, 0 while the AutoConfig is failing; the
  series is removed when the AutoConfig is deleted.
- Deletion: a terminating AutoConfig is not reconciled, so under foreground
  deletion (`kubectl delete --cascade=foreground`) the controller doesn't
  recreate generated endpoints while garbage collection deletes them.
- `status.phase` no longer transitions through `Fetching`/`Rendering` — those
  enum values remain for compatibility, but the controller now only sets
  `Pending`, `Synced`, or `Error`.
- Spec fetch, CUE, unmatched-override, and additional-endpoint scope failures
  retry via controller-runtime's exponential backoff (`OnChange`) or at
  `spec.periodic.interval` (`Periodic`), same as before; endpoint write
  failures and write conflicts follow the different rules described above.

**Before upgrading — external `$ref`s.** A URL-sourced AutoConfig whose spec
has an external `$ref` the operator can't fetch goes to `Error` after the
upgrade (endpoints kept at their last-good state) instead of syncing with the
raw `$ref`. List the URL-sourced AutoConfigs and their spec URLs:

```bash
kubectl get krakendautoconfigs -A -o json | jq -r '.items[] | select(.spec.openapi.url) | "\(.metadata.namespace)/\(.metadata.name)\t\(.spec.openapi.url)"'
```

Then list each spec's external (non-`#`) `$ref`s, fetching it from somewhere
that can reach the URL, with the same credentials as `spec.openapi.auth` (for
a YAML spec, convert it with `yq -o=json` first):

```bash
curl -s <spec-url> | jq '[.. | objects | select(has("$ref")) | .["$ref"] | select(type == "string") | select(startswith("#") | not)] | unique'
```

`[]` means no external refs. Otherwise check that each referenced document —
resolved against the URL of the document containing the ref — is reachable
from the operator, and repeat for those documents' own external refs.

To force an immediate reconcile — for example right after fixing an upstream
spec — change any annotation on the resource:

```bash
kubectl annotate krakendautoconfig <name> -n <ns> krakend.io/resync="$(date +%s)" --overwrite
```

No CRD or API changes; this is a controller-behavior-only change.

---

## Unreleased — AutoConfig fails sync on unmatched overrides

Previously, a `spec.overrides[]` entry whose `operationId` matched no
generated operation was silently ignored — the sync still reported
`Synced: True` even though the override never took effect. This is now a
fail-closed error.

If any override's `operationId` is not present in the fetched OpenAPI spec,
the AutoConfig sync now fails: `status.phase` becomes `Error`, the `Synced`
condition goes `False` with reason `UnmatchedOverride`, and a matching
`Warning` event is emitted. The generator does not run, so existing
`KrakenDEndpoints` are left as they were (last-good) until the override is
corrected or removed; the resource is re-evaluated on subsequent reconciles
until then.

Every `KrakenDAutoConfig` is re-evaluated once on the operator's first
reconcile after the upgrade, even if its OpenAPI spec, CUE definitions and
`spec` are unchanged — every reconcile now evaluates regardless of checksum
(see *Unreleased — AutoConfig continuous reconciliation, drift repair and
resync* above), so the detection query below is meaningful shortly after the
upgrade. Healthy resources regenerate identical endpoints, so their
`KrakenDEndpoints` are not modified — no endpoint churn. The one exception is
an override on an `operationId` the spec declares more than once: it now
lands on the endpoint that is actually published instead of on a skipped
duplicate.

Find affected resources cluster-wide:

```bash
kubectl get krakendautoconfigs -A -o json | jq -r '.items[] | select(any(.status.conditions[]?; .reason=="UnmatchedOverride")) | "\(.metadata.namespace)/\(.metadata.name)"'
```

Fix by correcting the override's `operationId` or removing the override. If
the target operation has no `operationId` at all: `spec.defaults` applies to
every generated operation; for that single operation, add an `operationId` to
the service's OpenAPI spec, replace the operation with an
`additionalEndpoints` entry (the same endpoint and method replaces the
spec-derived one), use a custom CUE definitions ConfigMap
(`spec.cue.definitionsConfigMapRef`), or have the service declare `audience`
directly on the operation as a list of strings — the default CUE definitions
default it to `["public"]` when absent and require a list of strings when
present (`cue/defaults.cue`), avoiding an override entirely.

Custom CUE definitions that read `_overrides` under a key that doesn't
correspond to an operationId the spec declares will now fail the sync, because
overrides are matched on the operationId contract.

Separately, the evaluator's per-operation warnings (e.g. an operation skipped
because it failed to convert) are now surfaced as `CUEEvaluationWarning`
events on the `KrakenDAutoConfig` resource instead of being silently
dropped; these do not change `status.phase` or conditions.

`documentation/openapi.audience` must now be a list of strings wherever it's
set: inside `extraConfig` on `spec.overrides[]`, `spec.defaults.endpoint`, or
`spec.additionalEndpoints[]` (AutoConfig), or declared directly on an OpenAPI
operation. `null` (e.g. an `audience:` key with no value in YAML) and `null`
items are rejected too.
The `KrakenDAutoConfig` admission webhook now rejects a non-list `extraConfig`
value at `kubectl apply` time (`must be a list of strings, e.g. ["internal"]`);
a value declared on the operation itself is caught by the default CUE
definitions instead and fails the sync with reason `CUEEvaluationFailed`.
Previously a malformed value (e.g. a YAML mapping) passed both checks
unchanged and only surfaced as a `krakend check -t -n -c` failure, which blocks
config updates for every service on that gateway — not just the one with the
bad value.

A `KrakenDEndpoint` gets the same rule on `spec.endpoints[].extraConfig`, with
the same message, for every added or changed entry, on CE and EE gateways
alike. What `krakend check` finds in an entry is reported by the gateway-wide
check described under *Complete admission*, as `spec.endpoints[i]: <finding>`;
the entry rules and the duplicate-route check report their own causes.

Updates are ratcheted (see *Complete admission*): a `KrakenDEndpoint` stored
with a non-list or `null` audience accepts every update that leaves the entry
carrying it unchanged, on the same gateway; a `KrakenDAutoConfig` accepts every
update that leaves the value unchanged at the same list position. Only an
update that touches it must fix it.

The operation-level CUE rule lives in the embedded default CUE definitions. A
namespace's `krakend-cue-definitions` ConfigMap replaces those defaults, so it
doesn't get the rule unless it's updated from the new `cue/defaults.cue`;
definitions from `spec.cue.definitionsConfigMapRef` are unified on top of
whichever defaults apply, so they keep the rule only where the embedded
defaults are in use. Without the rule, a malformed operation-level audience
is still stopped. The `KrakenDEndpoint` webhook's audience rule rejects the
write with a 422 for every writer, the operator included, and it is the only
guard on a CE gateway, where the render drops `documentation/openapi`. On an EE
gateway the AutoConfig controller's own precheck, which validates the
endpoints it is about to write with the checker the webhooks use, catches it
first. That precheck holds the operations that fail and writes the rest; it
does not hold back the whole set. The operator's writes of generated endpoints
skip only the admission render check (see *Complete admission*).

---

## Unreleased — Gateway validation, status and admission fixes

### Config validation runs offline

The operator now validates rendered configs with `krakend check -t -n -c`.
`-n` lints against the JSON schema built into the operator's krakend
binary; the previous `-l` downloaded the schema from the KrakenD website on
every validation. Validation no longer needs egress from the operator pod,
no longer fails while that site is unreachable, and its verdict changes only
with an operator upgrade. The built-in schema gave the same verdict as the
online one on every config tested. Each validation run is limited to 30
seconds.

**On upgrade:** a gateway reporting `ConfigValid=False` only because the
online schema could not be fetched is re-validated on the first reconcile
and, if its config is valid, rolled out. To list the gateways whose config
is currently not valid, and why, before upgrading:

```bash
kubectl get krakendgateways -A -o json | jq -r '
  .items[] | . as $g | (.status.conditions // [])[]
  | select(.type == "ConfigValid" and .status != "True")
  | "\($g.metadata.namespace)/\($g.metadata.name)\t\(.reason)\t\(.message | .[0:160])"'
```

A message containing `failing loading "https://www.krakend.io/schema` marks
a gateway blocked by the schema download rather than by its config.

### A rejected config no longer loops; `Rendering` and `Validating` are gone

A gateway whose rendered config failed validation used to rewrite its status
three times per reconcile (`Rendering`, `Validating`, `Error`), run krakend
check again each time, and emit a `ConfigValidationFailed` event on every
pass, continuously, until the input was fixed. Now:

- `status.phase` is no longer set to `Rendering` or `Validating`, and a new
  gateway is no longer written as `Pending` before its first full reconcile.
  The values remain in the API for compatibility. A value persisted by an
  older operator version stays until the gateway's next verdict replaces it.
- Gateway status is written only when it changes.
- The operator remembers the exact input the validator rejected and does not
  run the validation again for it. It validates again as soon as any input
  changes, including a switch to or from CE fallback, and once after an
  operator restart.
- `ConfigValidationFailed` fires when the verdict or its message changes,
  not on every reconcile.
- `krakend_operator_config_validation_failures_total` counts each rejected input once, not
  once per reconcile. A lasting rejection no longer keeps the counter rising.
  An alert on its rate, such as the runbook's former
  `KrakenDConfigValidationFailures`
  (`rate(krakend_operator_config_validation_failures_total[5m]) > 0` for 10 minutes),
  therefore no longer fires for a single lasting rejection. The runbook now
  alerts per gateway on `krakend_operator_gateway_config_valid`
  (`KrakenDGatewayConfigRejected`; see "Metrics" under "Gateway reconcile
  correctness"). To list the gateways that are rejected right now, use the
  `ConfigValid` query under "Config validation runs offline" above.

Tooling that waits for `Rendering` or `Validating` should wait on the
`ConfigValid` condition instead.

### An unavailable validator is retried, not reported as a broken config

Only a completed krakend check run that rejects the config marks it invalid
(`ConfigValid=False`, reason `ConfigValidationFailed`, phase `Error`). When
the check cannot run to completion (the binary is missing, the 30-second
limit is hit, the process is killed, the temp directory is unwritable or
full, or the validation copy cannot be prepared), the gateway now reports
`ConfigValid=Unknown` with reason `ValidatorUnavailable`, emits one
`ValidatorUnavailable` Warning event, keeps its phase and its applied
config, and retries with exponential backoff. Previously such failures were
reported as an invalid config (and re-run in a status-write loop).

`krakend_operator_config_validation_failures_total` counts only a fresh verdict from the
validator (`krakend check`, or the EE wildcard rules applied before it).
Failures to prepare the validation copy and other errors that are not
verdicts, such as an unavailable validator, do not increment it.

### Validation messages are capped at 4 KiB

The `ConfigValid` condition message and the `ConfigValidationFailed` event
now carry at most 4 KiB: a summary line, then one line per finding, as many
whole lines as fit, followed by `(output truncated, N more lines)`. Each line
reads `namespace/name spec.endpoints[i]: <finding>`, or `gateway: <finding>`
when it names no endpoint. The full output is logged by
the operator as `validation rejected the rendered config`. Previously an
output over the CRD's 32768-character limit (for example one bad key in a
policy used by many backends) made the status write fail, so the rejection
was never recorded.

### Terminating gateways are left alone; deleted gateways stop reporting metrics

A `KrakenDGateway` with a `deletionTimestamp` (for example during foreground
deletion) is no longer reconciled, so the operator no longer recreates the
children garbage collection is removing. When a gateway is deleted or starts
terminating, its `krakend_operator_endpoints`,
`krakend_operator_gateway_info`, `krakend_operator_gateway_config_valid`,
`krakend_operator_dragonfly_ready`, `krakend_operator_license_expiry_seconds`
and `krakend_operator_reconcile_duration_seconds{controller="gateway"}`
series are removed, so alerts on a deleted gateway stop firing.

### Autoscaled gateways keep the HPA's replica count

With `spec.autoscaling` set, the operator no longer writes
`Deployment.spec.replicas` on every reconcile. That write reset the count
the HorizontalPodAutoscaler had chosen and made replicas flap. A new
Deployment starts at `spec.autoscaling.minReplicas` (1 when unset); after
that the HPA alone owns the count. `spec.replicas` is ignored while
autoscaling is set, and the gateway webhook now warns when both are set.

### Updates to objects being deleted are admitted when they don't change the spec

All four validating webhooks (gateway, endpoint, policy and AutoConfig)
admit an UPDATE to an object that has a `deletionTimestamp` when the update
doesn't change its spec, without running any rule. Such updates remove
finalizers (for example `foregroundDeletion`). Rejecting them, for instance
because the referenced gateway had already been deleted, left the object
stuck in `Terminating`. A spec change on a terminating object is still
validated, because the gateway keeps rendering terminating endpoints and
policies until they are gone.

### Helm chart: `webhooks.enabled: false` and `webhooks.caBundle`

- `webhooks.enabled: false` no longer crash-loops the operator. The chart
  passes `--enable-webhooks=false` and leaves out the serving certificate
  path, volume and mount; the operator then does not start its webhook server
  or read serving certificates, and only render-time validation protects
  gateways. That flag needs an operator image from this release or later;
  installs that keep webhooks enabled pass no new flag.
- `webhooks.caBundle` (used when `webhooks.certManager.enabled: false`)
  accepts the PEM CA bundle or its base64 encoding. A base64 value, which
  the chart's comment asked for, used to be encoded a second time, so the
  API server could not verify the webhook's certificate and, with the
  default `failurePolicy: Fail`, every write to the four CRDs failed.

### The gateway webhook is no longer called on DELETE

`vkrakendgateway.kb.io` was registered for DELETE although its handler
validated nothing. With `failurePolicy: Fail`, deleting a gateway, or a
namespace containing one, needed a reachable operator. The registration now
covers CREATE and UPDATE only, in the Helm chart and in
`operator/config/webhook/manifests.yaml`.

---

## Unreleased — Status model: `Ready` conditions and single-writer status

Every kind now has a summary `Ready` condition and a top-level
`status.observedGeneration`, and each condition type has exactly one writer.
`status.phase` is still set where it was, but it is derived from the
conditions on every reconcile and kept only for compatibility. Read `Ready`
instead, for example:

```bash
kubectl wait --for=condition=Ready krakendendpoint/<name> -n <ns> --timeout=2m
```

### CRDs: apply them before upgrading the operator

Helm does not upgrade CRDs (see *CRD Upgrades* above), and this release
changes all four:

- `status.conditions` is a map keyed by `type` (`x-kubernetes-list-type: map`),
  so the API server rejects a status write that introduces two conditions of
  the same type. An object that already holds duplicate types is ratcheted:
  the API server does not reject it while the duplicates are left as they are.
- `KrakenDAutoConfig` and `KrakenDBackendPolicy` gain `status.observedGeneration`.
  With the old CRDs, the API server drops the field the new operator writes,
  and those objects' status is rewritten on every reconcile.
- `kubectl get` shows `Ready` and `Reason` columns; `Phase` moves to
  `kubectl get -o wide`.

As hygiene, check whether any object already carries a duplicate condition
type (no output expected):

```bash
kubectl get krakendgateways,krakendendpoints,krakendautoconfigs,krakendbackendpolicies -A -o json \
  | jq -r '.items[] | select(((.status.conditions // []) | map(.type) | length) != ((.status.conditions // []) | map(.type) | unique | length)) | "\(.kind) \(.metadata.namespace)/\(.metadata.name)"'
```

To remove a duplicate, delete the extra entry from the object's status by its
index in `status.conditions`:

```bash
kubectl patch <kind>/<name> -n <ns> --subresource=status --type=json \
  -p '[{"op":"remove","path":"/status/conditions/<index>"}]'
```

### KrakenDEndpoint: `Accepted` (gateway controller)

- The gateway controller no longer writes an endpoint's `phase` or its
  `Available` condition, and no longer marks endpoints with a missing policy
  `Invalid`. It owns one condition, `Accepted`, which it writes for every
  endpoint of the gateway, and only when the verdict changes:
  - `True`, reason `Accepted`: the endpoint is part of the gateway's
    validated configuration.
  - `True`, reason `PartiallyAccepted`: an older endpoint owns some of its
    (path, method) pairs and the rest are served.
  - `False`, reason `EndpointConflict`: older endpoints own all of its
    (path, method) pairs; none is served. A resolved conflict now clears by
    itself.
  - removed: a policy it references is missing, so it is not in the
    configuration (the endpoint controller reports why).
- `Accepted` is written only for a configuration that passed validation, or
  is unchanged since it did, and records the endpoint generation that
  configuration contains. While a gateway's rendered configuration fails
  validation, no verdict changes, except that an endpoint a finding names
  gets `Accepted=False` with reason `GatewayConfigRejected` (see "A rejected
  config names the endpoints at fault").
- The `EndpointConflict` Warning event fires once per transition instead of
  on every gateway reconcile. A `Normal` `Accepted` event marks a conflict
  that cleared. A `PartiallyAccepted` endpoint gets its own Warning
  (`PartiallyAccepted`) when it becomes partly conflicted. The
  `EndpointInvalid` event is gone.

### KrakenDEndpoint: `ResolvedRefs`, `Ready` and `phase` (endpoint controller)

| Condition | Written by | True when | Reasons |
|---|---|---|---|
| `ResolvedRefs` | endpoint controller | the gateway and every referenced policy exist | `RefsResolved`, `GatewayNotFound`, `PolicyNotFound` |
| `Accepted` | gateway controller | the endpoint is in the gateway's validated configuration | `Accepted`, `PartiallyAccepted`, `EndpointConflict`, `GatewayConfigRejected`, `EEFeaturesStripped` |
| `Ready` | endpoint controller | both are True, `Accepted` for the current generation | `Ready`, `Pending`, or the failing condition's reason; `SchemaNameConflict` (docs only) keeps it True |

- The `Available` condition is removed. On its first reconcile after the
  upgrade, the endpoint controller drops it and writes `ResolvedRefs` and
  `Ready`. Replace alerts or health checks on `Available` with `Ready`.
- `phase` is derived from `Ready`: `True` → `Active`; `Unknown` → `Pending`;
  `GatewayNotFound` → `Detached`; `EndpointConflict` and `PartiallyAccepted` →
  `Conflicted`; any other `False` reason → `Invalid`. This ends the flip-flop
  in which a conflicted endpoint alternated between `Conflicted` and `Active`.
- `Ready=Unknown` with reason `Pending` ("Waiting for the gateway to accept
  generation N") means the gateway has not yet accepted this generation. That
  is normal for a moment after every change; it persists while the gateway's
  rendered configuration fails validation (check the gateway's `ConfigValid`).
  Right after the upgrade, endpoints can show `Pending` until their gateway's
  first reconcile. Endpoints of a gateway whose current render is already
  rejected stay `Pending` until the gateway accepts a configuration.
- When the referenced gateway is deleted, `Accepted` keeps that gateway's
  last verdict; `Ready` reports `GatewayNotFound` and the phase is `Detached`.

### KrakenDGateway

- New `Ready` condition. The first matching rule gives its value:
  `ConfigValid=False` (its reason), license expired without CE fallback
  (`LicenseExpiredNoFallback`), a missing plugin ConfigMap
  (`PluginsResolved=False`, `ConfigMapNotFound`), `Available=False` (e.g.
  `RolloutFailed`), CE fallback (`EEFeaturesStripped` while the applied
  config is the fallback render, otherwise `LicenseFallbackCE`), no
  configuration validated yet
  (`Unknown`/`Pending`), a configuration that could not be validated
  because the validator was unavailable (`Unknown`/`ValidatorUnavailable`;
  the phase stays at the serving phase: `Pending` before any rollout,
  `Deploying` while a rollout is in progress or the Deployment is not
  available, `Running` otherwise), a rollout in progress (the
  `Progressing` reason, `ConfigDeployed` or `DeploymentUpdated`), and a
  Deployment not yet available (`AwaitingAvailability`). Otherwise it is
  `True`/`Ready`.
- `phase` is derived from the same rules on every reconcile (`Error`,
  `Degraded`, `Pending`, `Deploying`, `Running`) and no longer latches: a
  GitOps revert to the last good configuration now reports
  `ConfigValid=True` and `Running`, and a rollout that recovers after
  `ProgressDeadlineExceeded` returns to `Running`.
- `ConfigValid=True` now has reason `ConfigApplied` (was `ConfigValid`).
- `Ready` now stays `False` for the whole of a config rollout, until the
  Deployment has observed the change and every replica is updated and
  available, and goes `False` (phase `Error`) when the Deployment loses
  availability after a rollout finished, e.g. all replicas crash-looping.
  The Deployment rolls with `maxUnavailable: 0`, so any unavailable replica
  outside a rollout (an HPA scale-up, a pod eviction) also gives a brief
  `Ready=False` with phase `Error`, until the replica is available again.
- `status.observedGeneration` advances on every reconcile that evaluated the
  spec, including a rejected configuration. It stays behind
  `metadata.generation` while the gateway cannot be applied in full (see
  *`observedGeneration` waits until the spec is applied*).
- The gateway controller is the only writer of gateway status; license
  evaluation runs inside its reconcile (see *License checks run inside the
  gateway reconcile*).

### KrakenDAutoConfig

- New `Ready` condition: `True` when `SpecAvailable` and `Synced` are both
  `True`, otherwise `False` with the first failing condition's reason (e.g.
  `SpecFetchFailed`, `UnmatchedOverride`, `EndpointReconcileFailed`).
- New `status.observedGeneration`, set on every status write.
- `phase` is derived from `Synced` (`Synced`, `Error`) and is empty before
  the first sync. A new AutoConfig no longer gets a separate `Pending`
  status write and an extra reconcile.
- `lastSyncTime` is documented on the field: it is the last sync that
  changed something, not a heartbeat.
- An AutoConfig whose reconcile fails with an error, which is every
  `OnChange` failure and `EndpointReconcileFailed` for either trigger, now
  retries at least every 5 minutes (the exponential backoff used to grow to
  about 16.7 minutes), so it recovers within one resync interval once a
  missing gateway, policy or auth Secret appears or its spec source comes
  back. A `Periodic` AutoConfig with any other failure is requeued at
  `spec.periodic.interval`.

### KrakenDBackendPolicy

- `Ready` replaces `PolicyValid`: `True` when the policy's fields are in
  range, `False` with reason `InvalidCircuitBreaker` or `InvalidRateLimit`
  otherwise. The first reconcile after the upgrade removes `PolicyValid`;
  replace any check on it with `Ready`.
- New `status.observedGeneration`.
- The Warning event for an invalid policy now uses the condition's reason
  (`InvalidCircuitBreaker`, `InvalidRateLimit`) instead of `PolicyInvalid`,
  and fires only when the verdict changes.

### Watch scope

- The endpoint controller now reacts to gateways and policies being created
  or deleted only (not to their status updates), and to changes of its own
  spec or of its `Accepted` condition.
- AutoConfig retry backoff is capped at the 5-minute resync interval.
- The policy controller recounts references only when an endpoint is created,
  deleted, or has its spec changed, not on endpoint status writes.

---

## Unreleased — Gateway reconcile correctness

The KrakenDGateway controller now keeps every resource it owns converged
whatever the verdict on the newest rendered config, and it never lets a
config the running binary cannot load reach a pod.

### A rejected config no longer freezes the gateway

Previously, a config that failed `krakend check`, or could not be checked,
stopped the whole reconcile. The Deployment, Service, PDB, HPA, post-restart
Job and optional resources were not reconciled until the input was fixed,
so a deleted Deployment stayed deleted. Reconciliation now has two stages:

- **Config stage:** render, validate, publish. Only a config that passed
  validation becomes the applied config (`status.configChecksum`).
- **Infrastructure stage:** always runs and deploys the applied config. A
  rejected or unjudged render changes nothing the pods see. Every other spec
  change (replicas, image, resources, probes) and drift correction proceed
  as usual. The Deployment is left as it is in three cases only: no config has
  been applied yet, no ConfigMap holds the applied config (it was deleted
  while a newer render is rejected), and a plugin ConfigMap is missing.

Until a first config has passed validation, the Deployment is not created.
The ServiceAccount, Service and PDB are, and so are the HPA, Dragonfly,
ExternalSecret and VirtualService when the gateway configures them. An HPA
reports `FailedGetScale` until the Deployment exists.

While the validator cannot run (`ConfigValid=Unknown/ValidatorUnavailable`),
a gateway that is otherwise healthy reports `Ready=Unknown` with the same
reason. It keeps serving its applied config.

### Ready follows image, plugin and license rollouts

Gateway `Ready` now stays not True until the Deployment has finished rolling
out an image, plugin or license change, as it already did for a config change.
Previously a version bump, an EE recovery or a plugin change could report
`Ready=True` while the old pods were still being replaced.

The post-restart Job applies the same test. It runs, and re-runs after a
failure, only once the Deployment has observed its latest generation and
every replica runs the applied config, image, plugins and license.

**One-time rollout on upgrade.** The pod template gains a `krakend.io/image`
annotation, which records the image the operator set (the operator compares
it rather than the container image, which admission webhooks may rewrite).
Existing gateways roll their pods once when the operator is upgraded, and
every existing gateway reports `Ready` not `True` until that roll completes.

### Rollout status follows the Deployment

`Progressing` is now derived from the Deployment on every pass that
reconciles it, instead of being raised only when the pass notices a config,
image, plugin or license change. It is `True` while any of these holds:

- the pass created the Deployment (a deleted Deployment is recreated);
- the pass's write changed the pod template, including changes no annotation
  tracks: resources, probes, drift the operator reverts, the migration to the
  content-addressed ConfigMap;
- the Deployment's pod template is not the wanted one;
- old pods remain beside updated ones (`updatedReplicas < replicas`).

It goes `False`/`RolloutComplete` once the Deployment has observed its latest
generation and every replica is updated and available. A change in the
replica count alone, an HPA scale-up included, is not a rollout and does not
raise `Progressing`. The brief `Available=False` a scale-up can cause is still
mirrored as described in the status-model section above.

What changes for you:

- A gateway with no Enterprise license rolls its pods on upgrade (the pod
  template gains the image annotation and the new ConfigMap name). It now
  reports `Progressing=True`, phase `Deploying` and `Ready` not `True` until
  that roll completes, as this guide always said.
- A deleted Deployment reads `Deploying` while it is recreated, not
  `Ready=True`/`Running` followed by `Error`.
- A rollout stays reported when a status write fails or the cache is behind,
  and a pass whose Deployment update fails (a stale-cache Conflict, say)
  leaves `Progressing` and `Available` as they were. The one gap is the few
  milliseconds between a template write and the Deployment controller
  observing it: a later pass inside that window can read the Deployment as
  converged until the watch fires on the observation.
- The reason is `ConfigDeployed` or `DeploymentUpdated`, whichever the change
  gave; a rollout already reported keeps its reason, and otherwise it is
  `DeploymentUpdated`.
- `RolloutFailed` (`Progressing=False`, `Available=False`) now describes the
  current rollout only. A fix pushed while a rollout is stuck on
  `ProgressDeadlineExceeded` reads `Progressing=True` and phase `Deploying`
  until it converges or misses its deadline again, with no second
  `RolloutFailed` Warning, and `Available` no longer reads `RolloutFailed`.

### `observedGeneration` waits until the spec is applied

`status.observedGeneration`, and the `observedGeneration` of the `Ready`
condition, stay at their previous value while this pass could not apply the
spec in full:

- a child resource could not be reconciled (an HPA, Job, Dragonfly,
  ExternalSecret or VirtualService rejected by an admission webhook, say, or
  old config ConfigMaps that cannot be deleted), or
- the Deployment is held because no ConfigMap can hold the applied config
  although the render is the applied config (a ConfigMap that someone else
  owns sits at `<gateway>-config-<hash>`).

In both cases the error is logged and the gateway is requeued, so kstatus and
Flux report the gateway as in progress, not as current, until the error
clears; then `observedGeneration` catches up. A stuck `observedGeneration`
behind `metadata.generation`, together with an error in the operator log,
means a child resource or the applied config's ConfigMap cannot be
reconciled. A rejected config (`ConfigValid=False`), an unavailable validator
and a missing plugin ConfigMap do not hold it back: they are verdicts on the
current generation, and `Ready` reports them.

The infrastructure stage now attempts every independent child on each pass
and reports all the errors together, so one failing child no longer stops
the others from being reconciled. The post-restart Job, ConfigMap collection
and the deletion of an HPA the gateway no longer wants still wait for the
Deployment step to succeed.

### Gateway events fire on transitions only

`RolloutFailed` and `IstioVirtualServiceCreated` are now recorded once, when
the condition behind them changes, instead of on every reconcile.
`IstioVirtualServiceCreated` is recorded when `IstioConfigured` becomes True
after being False, not when the VirtualService is first created. Alerts that
counted these events per interval see one event per transition.
`DragonflyNotReady` was already recorded on transitions only. Its message is
now the condition's (`Dragonfly phase: <phase>`, or `Dragonfly CR not yet
created`), and a `DragonflyReady` Normal event marks the recovery.

### Config ConfigMaps are immutable and content-addressed

The rendered `krakend.json` is no longer rewritten in place in a ConfigMap
named after the gateway. Each applied config revision gets its own immutable
ConfigMap named `<gateway>-config-<first 10 hex characters of the checksum>`:

- label `krakend.io/config-revision`;
- annotation `krakend.io/checksum-config`, holding the full checksum.

The Deployment mounts that ConfigMap by name. A pod of an older ReplicaSet
that restarts during a stalled rollout therefore still starts on the
last-known-good config.

**On upgrade:**
- Every gateway rolls once, because its pod template now mounts
  `<gateway>-config-<hash>` instead of `<gateway>`. It is an ordinary
  rolling update (`maxSurge: 1`, `maxUnavailable: 0`). The config checksum
  is unchanged, so this change does not run the post-restart Job again. A
  gateway with `spec.redis` or Dragonfly renders a new config on upgrade
  anyway, and its Job runs once for that revision (see *Redis and Dragonfly
  connection pools are now actually configured*).
- A gateway may have its newest render rejected at upgrade time. The
  operator then copies the applied config out of the old `<gateway>`
  ConfigMap, but only when that content hashes to `status.configChecksum`,
  and keeps serving it.
- The old `<gateway>` ConfigMap is deleted once nothing can mount it (see
  below), which normally means right after the migration rollout completes.

**Garbage collection.** A config ConfigMap is deleted once nothing can
mount it. The operator keeps:

- the three most recently created revisions, the applied one included
  (revisions created in the same second are ordered by name);
- any revision mounted by a ReplicaSet of the gateway's Deployment that
  still has or wants pods.

Old revisions keep whatever the rendered config embeds, credentials
included. A revision is kept for up to three config changes, plus any a live
ReplicaSet still mounts, so a credential embedded in the rendered config
outlives its rotation by up to two config changes. Keep secrets out of the
rendered config, or restrict who can read ConfigMaps in the gateway's
namespace.

The old `<gateway>` ConfigMap never counts toward the three. Rolling a
gateway back means reverting its CRs. `kubectl rollout undo` to an old
ReplicaSet is not supported: the operator restores its own pod template, and
the undone revision's ConfigMap may already be gone.

**RBAC:** the operator's ClusterRole gains `list` on `apps/replicasets`. The
Helm chart ships it. If you maintain your own copy of the role, add it.

### A rejected config names the endpoints at fault

When `krakend check` rejects the rendered config, the gateway keeps serving
the last applied config (endpoints are not quarantined). It now also tells
you *which* KrakenDEndpoint to fix:

- The gateway's `ConfigValid=False` message starts with the KrakenDEndpoints
  the findings name, for example `Rejected by krakend check; findings name
  KrakenDEndpoint(s) team-a/orders.`, followed by one line per finding,
  `team-a/orders spec.endpoints[1]: <finding>` (`gateway: <finding>` when it
  names no endpoint), bounded to 4 KiB.
- Each named endpoint gets `Accepted=False` with reason
  `GatewayConfigRejected` and the findings that name it. Findings are mapped
  from `/endpoints/<i>/…` pointers and from `METHOD /path` or `path '…'` in
  router errors. A route conflict names both endpoints involved.
- Endpoints no finding names keep the verdict of the last applied config,
  except that a `GatewayConfigRejected` left by an earlier rejection is
  removed once no finding names the endpoint any more (its `Ready` is then
  derived afresh). This also holds when the validator is unavailable.
- A gateway that has never applied a config has no verdict to keep. On a
  rejected or unjudged render the operator removes `Accepted` from every
  endpoint that no finding names and that still carries it, for example one
  left by an earlier gateway of the same name, so no endpoint reads `Ready`
  while nothing serves it.
- Findings about gateway-level settings or plugins name no endpoint. When
  some findings name an endpoint and others do not, the gateway message says
  how many name none; when none names an endpoint, it says so.
- The blame clears as soon as a config passes validation.

### Partly conflicting endpoints report exactly what is not served

When two KrakenDEndpoints on a gateway declare the same path and method, the
older one's entry is served. The newer one's other entries are still
served. The newer KrakenDEndpoint now reports:

- `Accepted=True` with reason `PartiallyAccepted` when some of its entries
  are served. It is `Accepted=False/EndpointConflict` only when none are.
- `status.conflicts`, a list of `{endpoint, method, winner}`, one item per
  entry that is not served, naming the KrakenDEndpoint that serves it.

A `PartiallyAccepted` endpoint is not `Ready`, and its phase is
`Conflicted`. Becoming `PartiallyAccepted` emits a `Warning` event with that
reason, and returning to fully `Accepted` emits a `Normal` `Accepted` event.

### License checks run inside the gateway reconcile

The separate license monitor is gone. Each EE gateway's license is evaluated
on every gateway reconcile. The gateway is requeued at the next boundary
(the start of the warning window, the start of the 1 h safety buffer, and
expiry) and at least every 5 minutes. License Secret changes are picked up
immediately.

- The first check happens at operator startup, not 5 minutes later.
- The operator no longer writes the `gateway.krakend.io/license-check`
  annotation to your KrakenDGateway. It writes only the gateway's status.
  Existing annotations are harmless; remove them with
  `kubectl annotate krakendgateway <name> gateway.krakend.io/license-check-`.
- `LicenseValid` is now always present on an EE gateway with a license:
  - `True` with reason `LicenseOK`;
  - `True` with reason `LicenseExpiringSoon` inside the warning window;
  - `False` with reason `LicensePreExpiry` or `LicenseExpired`.
- An expired or pre-expiry license sets `LicenseExpired=True`. Without
  `fallbackToCE` the gateway reports phase `Error`; with it, the operator
  also sets `LicenseDegraded=True` (reason `LicenseFallbackCE`) and the phase
  is `Degraded`.
- `LicenseExpiringSoon`, `LicenseFallbackCE`, `LicenseExpiredNoFallback`,
  `LicenseSecretMissing` and `LicenseRestored` events fire once per
  transition. `LicenseExpiringSoon` is no longer repeated every 24 hours.
- The `krakend_operator_license_expiry_seconds` series of a deleted or terminating gateway is
  removed and not recreated.
- An EE gateway whose license cannot be read or parsed reports
  `LicenseValid=Unknown` (reason `LicenseSecretMissing`) next to
  `LicenseSecretUnavailable=True`. The operator still judges the stage from
  the last known expiry (`status.licenseExpiry`), so a `fallbackToCE` gateway
  falls back to CE when that expiry comes inside the 1 h safety buffer even
  while the Secret is missing. Once that expiry is inside the buffer or past,
  `LicenseValid` shows the stage (`False` with reason `LicensePreExpiry` or
  `LicenseExpired`) instead of `Unknown`. With no known expiry, or one still
  ahead of the buffer, it keeps its last fallback decision.
- A gateway switched from EE to CE loses its `License*` conditions,
  `status.licenseExpiry` and `krakend_operator_license_expiry_seconds` series, so a stale
  `LicenseExpired=True` no longer holds it at phase `Error`.
- Turning `fallbackToCE` off while the license is expired emits one
  `LicenseExpiredNoFallback` event as the gateway goes from `Degraded` to
  `Error`.
- A failing gateway reconcile is now retried with a backoff capped at 5
  minutes (it used to grow to about 16.7 minutes), so the license is always
  looked at at least that often.

### A renewed license rolls the EE pods

KrakenD reads its license at startup, and the license file is mounted with
`subPath`, which Kubernetes never refreshes from an updated Secret. Updating
the license Secret in place used to change `LicenseValid` but leave the
running pods on the old file, so with `fallbackToCE: false` an expired
license stayed in force after the renewal.

The pod template now carries `krakend.io/checksum-license`, the SHA-256 of
the license bytes the operator read:

- When the license in the Secret changes, the Deployment rolls and every pod
  starts with the new license. The gateway reports `Progressing=True` (reason
  `DeploymentUpdated`) and is not `Ready` until the new pods are available.
- When the bytes do not change, nothing rolls.
- When the Secret cannot be read, the annotation the Deployment already
  carries is kept, so a transient read failure never rolls pods.
- The annotation is present for every EE gateway with a readable license,
  including one running the CE fallback (the license stays mounted), so a
  fallback toggle alone does not change it. It is absent on a Community
  gateway.
- The post-restart Job does not run again for a license change.

**One-time rollout on upgrade.** Adding the annotation rolls each EE gateway
that mounts a license once. It coincides with the rollouts caused by the
`krakend.io/image` annotation and the config ConfigMap migration above, so an
upgrade still rolls each gateway only once.

### EE wildcard endpoints validate correctly

EE gateways with `/prefix/*` wildcard endpoints were rejected by
validation: the operator checks configs with the embedded CE binary, which
refuses unnamed wildcards. Validation of an EE config now works like this:

- **The EE router's wildcard rule is applied by the operator.** A
  `/prefix/*` endpoint conflicts with any other endpoint of the same method
  whose path starts with `/prefix/`, for example `GET /v1/*` next to
  `GET /v1/users`. The EE router refuses to start in that case. Both
  endpoints are named in the gateway's `ConfigValid` message and get
  `Accepted=False/GatewayConfigRejected`. A wildcard endpoint with more
  than one backend is rejected the same way, because EE allows only one.
- **Everything else is checked by `krakend check`.** For the check only,
  each wildcard is modelled as a path parameter, so the wildcard endpoints'
  backends and `extra_config` are linted and parsed too. That parameter does
  not exist in EE: a backend `url_pattern` that references `{Wildcard}` on a
  wildcard endpoint is rejected, as EE would reject it.

The route check rejects a root `/*` endpoint in both editions (gin refuses an
unnamed wildcard at the root, and EE does too). It used to be dropped from
validation, and is now rejected. Move such an endpoint to `/prefix/*`.

### CE fallback is validated as CE, and the image follows the validated config

`status.configEdition` (`EE` or `CE`) records the edition the applied config
was validated for. A render counts as applied only when both its checksum
and its edition match:

- **License fallback.** When a license expiry switches an EE gateway to
  fallback, the CE render is validated as CE, even when it has the same
  bytes as the EE config.
- **Image.** The Deployment's image follows the applied edition, never the
  edition the license asks for next. While a CE-fallback render is rejected,
  the pods keep the EE image with the EE-validated config. They switch to the
  CE image only once a CE-validated config is applied. While the applied
  edition differs from the current one (a rejected `spec.edition` flip, or a
  CE fallback whose render is rejected), changes to `spec.version`,
  `spec.image` and `spec.ceImage` wait as well. They take effect once a
  render is validated for the new edition.
- **Plugins.** Plugins are not held back by the config verdict: the plugin
  sources, the plugin checksum and `DeploymentUpdated` follow the spec,
  even while a render is rejected. A plugin ConfigMap that does not exist
  does hold the Deployment (see "A missing plugin ConfigMap is reported").

On upgrade, a status without `configEdition` is read as validated for the
edition the gateway renders for now, which is the image it already runs.
The field is saved on the first reconcile, whatever the verdict on that
render, and causes no rollout. For an EE gateway the edition is read from
`status.activeImage` when it equals exactly one of the EE image and the CE
image of the spec. Otherwise (a custom image, or both images equal) the
adoption assumes the license evaluation in that reconcile reaches the same
fallback decision the previous version was running. A gateway with such an
image that is upgraded exactly at a license expiry boundary may be adopted
with an edition other than the one it was running. For example, an EE gateway
adopted as CE gets the CE image for an EE-validated config, until a render is
validated for CE.

### CE fallback removes Enterprise-only features and says exactly which

When an EE license expires (or enters its 1 h safety buffer) and
`spec.license.fallbackToCE` is set, the gateway now renders a CE config
instead of loading the EE config into the CE binary. The CE render removes:

- every EE wildcard endpoint (`/prefix/*`), which the CE router cannot load;
- every Enterprise-only `extra_config` namespace at service, endpoint and
  backend level, for example `auth/api-keys`, `security/policies`, `redis`,
  `qos/ratelimit/service` and `documentation/openapi`. The exception is
  `backend/http/client`, which CE partly honors: only its Enterprise-only keys
  (such as `proxy_address`, `client_tls` and `no_redirect`) are removed and
  listed, while `send_body_on_redirect` stays. A client block that holds only
  `send_body_on_redirect`, or nothing, stays whole and is not listed.
  KrakenD CE silently ignores what is removed, which previously left routes
  unprotected with no trace in status.

A CE-fallback render now keeps `send_body_on_redirect` where it removed the
whole `backend/http/client` block before, so the rendered config differs from
earlier releases (a new checksum, so the pods roll once), and an empty
`backend/http/client` block is no longer listed as stripped.

**What you see:**
- The gateway reports `CEFallbackApplied=True`, reason `EEFeaturesStripped`,
  listing every removed feature. `Ready` is False with the same reason, and
  the phase is `Degraded`.
- Each affected KrakenDEndpoint reports `Accepted` reason
  `EEFeaturesStripped` with its list. It is `True` while some of its entries
  are still served, and `False` when all of them were wildcards.
- Docs-only namespaces (the endpoints' `documentation/openapi` and the
  component schemas the docs publish) are dropped too, but they are not
  listed on the endpoints and never make one not Ready: they change nothing
  the gateway serves. The gateway's `CEFallbackApplied` lists only the
  gateway-level `documentation/openapi`, which exists when an endpoint has
  component schemas or `spec.config.documentation` is set. An entry's own
  `documentation/openapi` is dropped without being listed anywhere.
- The OpenAPI export init container and the `openapi-serve` sidecar do not
  run on any CE render (a CE-edition gateway, or a CE fallback), because the
  CE binary cannot export OpenAPI; in a fallback with `spec.openapi` enabled,
  `CEFallbackApplied` lists that too. They return with EE.
- Everything is restored once an EE render is validated and applied after
  a valid license is back.

**One-time rollout on upgrade.** Any CE-edition gateway whose entries carry
`documentation/openapi`, or whose endpoints have component schemas, now
renders a different config. On operator upgrade that means one rollout plus
one post-restart Job run, because the Job is keyed by config checksum. A
CE-edition gateway with `spec.openapi` enabled also rolls once, because its
pod template loses the export init container and the `openapi-serve`
sidecar (and its Service loses the `openapi` port); those pods could not have
started the export, which needs the Enterprise binary. The admission webhook
warns about a `spec.openapi` that is stored enabled on a CE gateway and stays
enabled; enabling it, or switching a gateway that has it enabled to CE, is
rejected.

### Redis and Dragonfly connection pools are now actually configured

The operator rendered Redis settings under `extra_config["backend/redis"]`,
which is not a KrakenD namespace, and removed that key before validation.
KrakenD ignored it, so the previous key had no effect: `spec.redis` and
Dragonfly configured no connection pool at all, and shared Redis state such
as cluster-wide rate limits never applied. The operator now renders the
documented Enterprise service-level namespace `redis`. The pool does nothing
by itself: Enterprise components that reference `connection_name: "default"`
now reach Redis and start enforcing shared state. The operator renders:

- one `connection_pools` entry named `default` for a single address (or
  the Dragonfly Service);
- one `clusters` entry named `default` when `spec.redis.connectionPool.addresses`
  lists several.

The namespace is validated like the rest of the config. Reference the pool
from Enterprise components by name, for example
`"qos/ratelimit/router/redis": {"connection_name": "default", …}`. A gateway
that falls back to Community lists `redis` among the Enterprise features it
dropped.

- `readTimeout` and `writeTimeout` have no equivalent in KrakenD and are no
  longer rendered; the gateway webhook warns when they are set.
- **On upgrade**, every gateway with `spec.redis` or Dragonfly renders a new
  config. It is validated and rolled out once, and its post-restart Job runs
  once for the new config revision, although the ConfigMap migration alone
  does not run it (see *Config ConfigMaps are immutable and content-addressed*).
- `spec.redis.connectionPool.password` and `.tls` are still not rendered, and
  neither is `dragonfly.authentication.passwordFromSecret` into KrakenD's pool.
  The pool connects without them. A Dragonfly that requires a password (the
  operator sets it from `dragonfly.authentication.passwordFromSecret`) refuses
  the now-active pool with `NOAUTH`, and an unreachable pool only logs at
  startup without affecting health. The gateway webhook warns when these are set.
- A raw `backend/redis` key in `spec.config.extraConfig` is no longer stripped
  before validation, so it now fails with `additional properties
  'backend/redis' not allowed`. Remove it and use `spec.redis` or Dragonfly.
- With no address (`spec.redis.connectionPool.addresses` empty and no
  Dragonfly), no pool is rendered, and the other pool settings are dropped.

### `spec.config.dnsCacheTTL` renders the documented root field

`spec.config.dnsCacheTTL` was rendered as `extra_config["qos/dns"]`, which
KrakenD 2.13 rejects ("additional properties 'qos/dns' not allowed"). It now
renders the root `dns_cache_ttl` field. The value must be a single unit with
an integer value, `^[0-9]+(ns|ms|us|µs|s|m|h)$`, for example `30s`; values
like `1h30m` or `1.5s` are rejected by KrakenD.

A gateway that sets it was failing validation and held at its last-known-good
config, so on upgrade it applies all of its pending changes at once. No live
gateway is known to set it.

### Dragonfly, ExternalSecret and VirtualService are watched

When their CRDs are installed at operator startup, the operator now watches
the Dragonfly, ExternalSecret and VirtualService objects it creates:

- an edited or deleted VirtualService or ExternalSecret is restored at once;
- Dragonfly becoming ready is reflected in `DragonflyReady` at once.

**A CRD installed after the operator started is not watched until the
operator restarts.** The operator logs `optional CRD not installed at
startup` for each such kind. Its objects are still created and corrected on
every gateway reconcile, just not on their own changes. Restart the operator
(`kubectl rollout restart deployment -n <operator-namespace> <operator-deployment>`)
after installing Istio, External Secrets Operator or the Dragonfly Operator.

### Disabling a feature deletes what it created

When a feature is turned off, the operator now deletes the resource it
created for it, but only a resource the gateway controls:

| Turned off | Deleted |
|---|---|
| `spec.autoscaling` removed | the HPA `<gateway>` |
| `spec.dragonfly.enabled: false` or removed | the Dragonfly `<gateway>-dragonfly`, plus `DragonflyReady`, the `krakend_operator_dragonfly_ready` series and `status.dragonflyAddress` |
| `spec.license.externalSecret.enabled: false` or removed | the ExternalSecret `<gateway>-license`, and the `<gateway>-license` Secret it created (its creation policy is `Owner`) |
| `spec.istio.enabled: false` or removed | the VirtualService `<gateway>`, plus `IstioConfigured` |

Previously these stayed behind. An orphaned HPA kept scaling the Deployment,
and an orphaned VirtualService kept claiming its hosts.

- **Replicas.** Once the HPA is deleted the Deployment returns to
  `spec.replicas`. While the Deployment is held it keeps the HPA's last
  replica count until the hold ends. It is held when no config has been
  applied yet, when the applied config's ConfigMap is missing while a newer
  render is rejected, and when a plugin ConfigMap is missing. If
  `spec.replicas` is unset that is one pod, so set `spec.replicas` before
  upgrading or before removing `spec.autoscaling`. Setting `spec.replicas` while
  `spec.autoscaling` is still set gives the expected admission warning
  "spec.replicas is ignored while spec.autoscaling is set: the
  HorizontalPodAutoscaler manages the replica count".
- **License.** A gateway that disabled the ExternalSecret but whose
  `license.secretRef` points at the `<gateway>-license` Secret loses its
  license when that Secret is garbage-collected. Point `secretRef` at a Secret
  you manage first.
- **Dragonfly.** The instance is deleted at once. Pods still running the
  previous config, which points at Dragonfly's Redis address, lose that
  connection until the rollout completes, or until a rejected render is fixed.

**Check before upgrading** that nothing relies on a leftover object. The first
loop lists the children gateways control; the second lists which features each
gateway still enables. A child from the first loop whose feature is false in
the second listing is deleted by the upgrade.

```bash
for kind in horizontalpodautoscalers.autoscaling dragonflies.dragonflydb.io externalsecrets.external-secrets.io virtualservices.networking.istio.io; do
  kubectl get "$kind" -A -o json 2>/dev/null | jq -r --arg kind "$kind" '
    .items[] | select(any(.metadata.ownerReferences[]?; .kind=="KrakenDGateway" and .controller==true))
    | "\($kind)\t\(.metadata.namespace)/\(.metadata.name)\towner=\(.metadata.ownerReferences[] | select(.kind=="KrakenDGateway") | .name)"'
done
kubectl get krakendgateways -A -o json | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name)\tautoscaling=\(.spec.autoscaling != null)\tdragonfly=\(.spec.dragonfly.enabled // false)\texternalSecret=\(.spec.license.externalSecret.enabled // false)\tistio=\(.spec.istio.enabled // false)"'
```

### A missing optional CRD is reported in status

A feature enabled without its CRD installed now shows as a condition with
reason `CRDNotInstalled`:
- `DragonflyReady=False` for Dragonfly;
- `IstioConfigured=False` for Istio;
- `LicenseSecretUnavailable=True` for the license ExternalSecret.

It is no longer a `CRDNotInstalled` Warning event on every reconcile. The
event fires once, when the condition is first set.

A newly created gateway with Dragonfly enabled now records one
`DragonflyNotReady` Warning on its first reconcile, while its Dragonfly CR is
created.

### A missing plugin ConfigMap is reported, and nothing is rolled out that cannot start

A `spec.plugins.sources[].configMapRef` naming a ConfigMap that does not
exist now sets `PluginsResolved=False` with reason `ConfigMapNotFound`,
naming the ConfigMap. `Ready` goes False with the same reason and the phase
is `Error`. The Deployment is held as it is, and a new gateway gets no
Deployment yet. No `Progressing` rollout or `ConfigDeployed` event is
reported while it is held. The Service, PDB, HPA, Dragonfly, ExternalSecret
and VirtualService are still reconciled.
Previously the operator rolled out a pod template that mounted the missing
ConfigMap, and new pods hung in `ContainerCreating`. Creating the ConfigMap
releases the hold. When a config was applied during the hold, or the
gateway has no Deployment yet, that config rolls out and is reported as
`Progressing=True` with reason `ConfigDeployed`, with its event; otherwise
nothing rolls, unless the plugin bytes changed (`DeploymentUpdated`).
`PluginsResolved` exists only while the gateway has ConfigMap plugin
sources.

### Metrics

- New gauge `krakend_operator_gateway_config_valid{namespace,name}`: 1 while
  the gateway's newest config passed validation, 0 while it is rejected or
  could not be checked. Like the other per-gateway series, it is removed when
  the gateway is deleted. Alert on it instead of the unlabelled
  `krakend_operator_config_validation_failures_total`: the runbook's
  `KrakenDConfigValidationFailures` rule is replaced by
  `KrakenDGatewayConfigRejected` (`krakend_operator_gateway_config_valid == 0`
  for 15 minutes).
- `krakend_operator_gateway_info` now keeps one series per gateway. A
  version or edition change replaces the series instead of adding one.
- `krakend_operator_rolling_restarts_total` now counts Deployment writes that
  changed the pod template, once per write. It no longer counts the detection
  of a config, image, plugin or license change, so a write that fails, or a
  change detected again after a failed status write, is not counted twice. A
  creation is not a restart. Drift in the pod template that the operator
  reverts now counts as a restart. Every gateway adds one count when the
  operator is upgraded (the migration rollout).

---

## Unreleased — Complete admission

**Route-shape conflicts resolve at render time.** Two KrakenDEndpoints on one
gateway whose paths differ only in parameter names (for example
`GET /users/{id}` and `GET /users/{name}`), or only in repeated slashes
(`GET /a//b` and `GET /a/b`), used to reach the rendered config together, and
`krakend check` rejected the whole gateway (`':name' ... conflicts with
existing wildcard ':id'`). The renderer now treats them as the same route: the
older KrakenDEndpoint wins and the newer one is reported with
`Accepted=False/EndpointConflict`, or `Accepted=True/PartiallyAccepted` when
some of its entries are still served, and `status.conflicts[]`, exactly as for
exact duplicates. Between two entries of one KrakenDEndpoint the earlier spec
entry wins, and `status.conflicts[].winner` then names the endpoint itself.
Admission rejects such pairs outright (below); this covers objects stored
before the upgrade and concurrent applies.

**Duplicate routes are rejected.** A KrakenDEndpoint entry whose method and
path another KrakenDEndpoint on the same gateway already defines is rejected
(`Duplicate value: "GET /users": already defined by KrakenDEndpoint
ns/name`). Until now it was admitted with a warning, then reported as
`EndpointConflict`/`PartiallyAccepted` with `status.conflicts` and not served.
So is an entry whose path differs from another only in parameter names
(`/users/{id}` and `/users/{name}`) or in repeated slashes, on the same method,
including two entries of one KrakenDEndpoint. Against other KrakenDEndpoints
only a route new to the stored object is checked (every entry, when the object
moves to another gateway), so a conflict stored before the upgrade does not
block unrelated edits, including edits to the body of the entry that is
served; same-shape entries inside one KrakenDEndpoint are always checked when
an entry changes. The pre-upgrade audit lists stored conflicts. The denial names the endpoint the renderer
serves. After the upgrade, the newer
endpoint of such a stored pair keeps listing the lost entry in
`status.conflicts` (`Accepted=False/EndpointConflict` when it has no other
entry, otherwise `Accepted=True/PartiallyAccepted`). Endpoints with the same
controller (two endpoints one KrakenDAutoConfig generated, while it renames an
operation) may share a route: the renderer serves the older one until the
AutoConfig deletes it. For the same reason, entries of one AutoConfig whose
paths differ only in parameter names (`/h/{a}` and `/h/{b}`) are caught by no
admission rule; the renderer reports the newer one as a conflict. An endpoint
whose `policyRef` names a missing policy is not rendered, but admission still
counts its routes as taken, so a route can be rejected that the gateway would
serve until the policy exists. Renaming a path parameter across several routes needs
them in one KrakenDEndpoint; see the runbook.

**Gateways with a health-path or `autoOptions` clash freeze instead of
crash-looping.** The controller's route check now rejects them. Such a gateway
keeps its last-known-good config with `ConfigValid=False`. Before this release
the same render was published and new pods panicked at startup. The clashes
are an endpoint on the gateway's custom `health_path` with a GET method, and
endpoints under different methods whose routes would clash in one method's
tree while `router.auto_options` is on, because `auto_options` joins every
method's paths in one tree (for example `GET /users/{id}` with
`POST /users/{userId}/orders`).

**The validator binary is pinned by digest.** The operator image takes its
`krakend` binary from KrakenD CE 2.13.11, referenced by digest in the
`Dockerfile`, instead of the floating `krakend:2.13` tag. Admission and the
gateway controller validate every gateway with that binary, whatever its
`spec.version`, and the validator changes only when the pin does. If you build
the operator image yourself, the build argument is now `KRAKEND_IMAGE`; the
former `KRAKEND_VERSION` is gone, and passing `--build-arg KRAKEND_VERSION=...`
no longer has an effect: the build uses the pinned image.

**Rejection messages name the entry.** The `ConfigValid` message and an
endpoint's `Accepted=False/GatewayConfigRejected` message now name the entry
that failed. `ConfigValid` lists one line per finding, as
`team-a/orders spec.endpoints[1]: <finding>`, `team-a/orders: <finding>` when
the endpoint is known but no entry of it matches, or `gateway: <finding>`. An
endpoint's `GatewayConfigRejected` message reads `... findings naming this
endpoint: spec.endpoints[1]: <finding>`, with its findings joined by `; `. When
one finding blames several entries of an endpoint, its line lists them
(`spec.endpoints[0], spec.endpoints[1]: <finding>`). The gateway controller
gathers, renders and validates through the same checker the admission
webhooks use.

**Admission denials are `422 Invalid` with one cause per rejected field.**
`kubectl` prints `The KrakenDEndpoint "x" is invalid: spec.endpoints[1]: ...`
instead of `403 Forbidden` with a single message. Scripts that matched
`Forbidden` must match `is invalid`. A failed lookup or an unavailable
validator is `500 Internal Error`, a transient server error: retry the
request; controllers and GitOps tools retry on their own. Each webhook call is
now limited to 15 s (`timeoutSeconds`; it was the 10 s default).

**Endpoint writes are checked against the whole gateway.** Creating or
changing a KrakenDEndpoint renders its gateway's config with the change and
validates it with `krakend check -n` and the route check, which together cover
what the controller's `krakend check -t -n` finds. The write is rejected only
when the gateway's config passed before the change and fails after it, with one
cause per offending entry (`spec.endpoints[1]: <finding>`); a finding about
another endpoint or the gateway root is reported on `spec.endpoints`. If the
gateway already fails without the change (because of another object, or of the
endpoint's own stored version), the change is judged with the gateway root
alone: it is admitted with a warning that names the existing failure, unless
the candidate fails with the root alone when its stored version did not. For a
create, or a move to another gateway, the baseline is the root by itself, so a
broken root never blocks every new endpoint on its gateway. The entry rules run
first, and a write they reject is not rendered. That includes the
`documentation/openapi.audience` rule, which keeps rejecting a malformed
audience on a changed entry on every gateway, because a CE render drops an
entry's `documentation/openapi` and the render check would never see it. The
checks run in the operator pod, three at a time for the whole pod, sharing those
slots with the gateway controller, and each webhook call stops its work after
12 s. A request that cannot get a slot in time, or whose check cannot run, is
answered `500 Internal Error`: a transient error that `kubectl` does not retry,
so run the command again (controllers and GitOps tools retry on their own).
A policy write is rendered in each gateway that uses it, one after another: 1 + N
checks for N gateways, up to 2 + 2N when gateways already fail, which fits tens
of gateways in the 12 s, and fewer while the controller holds slots. Past that
size the `500` repeats on every retry: it is deterministic, not transient, so
split the use of the policy across more policies.
With the webhooks disabled the controller's check is the only protection: a
config that fails it keeps the gateway at its last-known-good config.

**The operator's own writes to AutoConfig endpoints skip the render check.**
A write the operator makes to a KrakenDEndpoint that a KrakenDAutoConfig
controls (by controller owner reference, never by labels) is not rendered and
linted: the AutoConfig controller validates the endpoints it is about to write
before it writes them, and checking every write again would cost one `krakend
check` per generated operation. Those writes still get the schema, reference,
audience, duplicate route and entry checks. The operator recognises its own requests by
`--operator-username`, which defaults to its ServiceAccount
(`system:serviceaccount:$POD_NAMESPACE:$POD_SERVICE_ACCOUNT`), and the chart
and the kustomize manifests now set both variables from the downward API.
Custom deployments without those variables must set the flag, or every write
gets the render check. The username is compared whole, so a user whose name
only starts with it is not trusted, and an empty value trusts nobody.

**The operator's memory limit is 512Mi** (was 256Mi). Up to three `krakend
check` runs share the container, each peaking near 110 MB. If you set
`resources` in your own values or manifests, raise the limit.

**Kubernetes 1.33 or later is required.** The CRDs now carry schema rules and
CEL validation. Kubernetes 1.33 ratchets CRD validation: an update that leaves
an already-invalid field unchanged is admitted, so most field rules (patterns,
enums, minimums, lengths) let objects stored before the upgrade keep accepting
unrelated changes. The exceptions are listed in the Pre-Upgrade Checklist:
rules on a whole `spec`, evaluation errors, atomic lists, and the
KrakenDAutoConfig name rule. Run the audit in the Pre-Upgrade Checklist to find
such objects. The chart refuses clusters below
1.33 (`kubeVersion`), and the OLM bundle's `minKubeVersion` is 1.33.0. With
Helm, use 3.18 or later: older releases default `helm template` and `helm lint`
to Kubernetes capabilities below 1.33 and refuse the chart unless given
`--kube-version`.

**KrakenDEndpoint schema.** `spec.endpoints` needs one to 1024 entries and each
(endpoint, method) pair at most once; every entry needs a backend; `endpoint`
must start with `/` and contain no `*`, `?`, `&` or `%` except a trailing `/*`;
`timeout` and `cacheTTL` must be Go durations (`30s`, `1m30s`); `outputEncoding`,
a backend's `encoding`, `sd` and `method` must be values KrakenD 2.13 accepts;
`gatewayRef.name` and `policyRef.name` must be non-empty (this also applies to
the same references on a KrakenDAutoConfig). `spec.endpoints` is now a map list
keyed on (endpoint, method), so server-side apply merges entries instead of
replacing the list. The webhook's duplicate-entry and policy field checks were
removed: the schema enforces them, and a rejection now comes from the API
server (`Duplicate value`, `should be greater than or equal to 1`) rather than
from the webhook.

A KrakenDEndpoint `timeout` or `cacheTTL` must also parse as a duration that
fits in 64 bits of nanoseconds and be at most 64 characters: a value such as
`2562048h` matched the pattern but overflowed, and one stored object like it
broke decoding of the whole endpoint list. The rule applies only to values that
match the pattern, so a stored non-duration (`3 seconds`) fails the pattern
alone and keeps ratcheting.

**KrakenDGateway schema.** `config.timeout`, `cacheTTL`, `dnsCacheTTL`,
`cors.maxAge` and `redis.connectionPool.dialTimeout` must be KrakenD durations
(one integer and one unit: `3s`, `12h`, not `12h0m`); `config.port` must be
1-65535; `config.outputEncoding` must be a KrakenD 2.13 value; and
`router.healthPath` must start with `/`. The license, OpenAPI port, single PVC
plugin source and post-restart script rules moved from the webhook to the CRD
with the same messages; an Enterprise license `secretRef` now needs a
non-empty name (a stored Enterprise gateway whose `secretRef.name` is empty can
no longer have its spec edited until the name is set), and a gateway may list at
most 32 plugin sources. `timeout`, `cacheTTL`, `dnsCacheTTL` and `dialTimeout`
must also parse as a duration of at most 64 characters, and
`postRestartJob.tmpSizeLimit` must be a quantity Kubernetes can decode, with a
binary or decimal SI suffix or an `e`/`E` exponent of at most two digits
(`100Mi`, `1.5Gi`, `500M`, `129e6`, `128974848`); a longer exponent is refused
before it is parsed, because parsing one such as `1e2147483648` takes seconds.
The patterns alone admitted values such as `99999999999h` and
`1e99999999999999999999`, which the validator or the Go decode then rejected.
Rule evaluation errors are not ratcheted, so a stored value that matches the
pattern but overflows (a duration KrakenD never accepted) blocks every update to
that object until it is fixed. A stored value that breaks the pattern, such as
`5 seconds`, fails only the pattern and keeps ratcheting. The quantity fields of
the embedded `resources` cannot carry per-field rules; keep the admission
webhooks enabled, because their typed decode rejects such values.

**KrakenDAutoConfig schema.** `spec.openapi` needs exactly one of `url` or
`configMapRef`, and `configMapRef` needs `urlTransform.hostMapping`. `trigger:
Periodic` needs `periodic.interval` of **at least 30s** (previously any
non-zero value; shorter intervals hot-looped the upstream), and the interval
must be a Go duration. The floor applies only while the trigger is `Periodic`:
an `OnChange` object may keep a short interval, but switching it to `Periodic`
is rejected until the interval is raised. `bearerTokenSecret` and `basicAuthSecret` are mutually
exclusive, and a name is at most 63 characters because it becomes a label value
on the generated endpoints. An override `method` is one of GET, POST, PUT, PATCH
or DELETE, `concurrentCalls` is 1 or more, `backends[].index` is 0 or more, and
`spec.overrides` holds at most 1024 items. An override `endpoint` and an
`additionalEndpoints[].endpoint` follow the KrakenDEndpoint path rule (start
with `/`, no `*`, `?`, `&` or `%` except a trailing `/*`), and
`additionalEndpointsBasePath` starts with `/`. The base path `additionalEndpointsBasePath` stays mutually exclusive with
`urlTransform.addPathPrefix`. `additionalEndpoints` holds at most 256 items,
unique on (endpoint, method), with `method` defaulting to `GET` (existing
objects read back with `method: GET`), and an entry sets either `backends` or
the `host`/`backendUrlPattern`/`encoding` shorthand, not both. `timeout` and
`cacheTTL` on `defaults.endpoint`, overrides and additional endpoints are Go
durations that fit in 64 bits of nanoseconds, at most 64 characters. The
`outputEncoding` of `defaults.endpoint`, overrides and additional endpoints, and
the `encoding` and `sd` of `defaults.backend` and the `encoding` of an
additional endpoint's shorthand, must be values the KrakenDEndpoint schema accepts: a typo used to be admitted
here and then fail every generated KrakenDEndpoint write. The webhook no longer
checks these; a rejection now comes from the API server. A stored value that
breaks a field rule (a pattern, an enum, a minimum) keeps being accepted on
unrelated updates. Four things are not ratcheted. The rules on `spec` itself (a
source, `hostMapping`, the Periodic interval, the base path and `addPathPrefix`
exclusivity) are re-checked on any change to the spec. The 63-character name
rule runs on every write to the object, including label, annotation and status
writes, because ratcheting compares the whole node and at the root that is the
whole object; a stored object with a longer name can only be deleted and
recreated. A duration that matches the pattern but overflows raises an
evaluation error, which is never ratcheted. Within `overrides`, an atomic list,
any edit re-checks every item, as for an entry's `backends`;
`additionalEndpoints[].backends` is atomic in the same way.

`additionalEndpoints` was an atomic list and is now a map list keyed on
(endpoint, method), so server-side apply merges entries by key. Existing
server-side apply managers get per-item ownership on their next apply, and no
conflicts are reported until then.

**Redis and Dragonfly credentials are rejected until they are supported.** The
operator has never rendered `spec.redis.connectionPool.password` or `.tls`, so
KrakenD connected without them. Setting or changing either is now rejected
(`not supported yet`). A stored value keeps being accepted on unrelated
updates, and the gateway webhook still warns about it.
`dragonfly.authentication.passwordFromSecret` on an Enterprise gateway is
rejected for the same reason: the Dragonfly instance requires the password and
the rendered KrakenD pool has none. A gateway that already has it keeps being
accepted on unrelated updates; adding it, changing the stored password, or
switching a gateway that has it to Enterprise, is rejected.

**Entry rules the operator enforces are checked at admission.** A
KrakenDEndpoint entry is rejected, with the exact field, when its path is
`/__debug`, `/__echo` or `/__health` or lies under one of them; when it is a
`GET` on the gateway's health path (`spec.config.router.healthPath`, or the
`router` block of `spec.config.extraConfig`, which replaces the typed one:
`disable_health` frees the path); when it is the root wildcard `/*`, which
KrakenD rejects in every edition; when it is a `/prefix/*` wildcard and the
gateway runs CE (`/prefix/*` wildcards need an EE gateway); or when a backend
`urlPattern` placeholder is neither a parameter of the endpoint path,
`respN_...` nor `JWT....`. **On a CE gateway, what a CE render would drop is
rejected** in an entry's or backend's `extraConfig`: an Enterprise-only
namespace (for example `auth/api-keys` on an entry or `auth/gcp` on a backend),
or the Enterprise-only keys of `backend/http/client` (a block with only
`send_body_on_redirect`, which CE honors, is admitted). KrakenD CE accepts them
in `krakend check` and then drops them silently, so a route that asks for
API-key authentication was served without it. An entry's `documentation/openapi`
is still admitted, because AutoConfig generates it on every endpoint and a CE
render drops it. Only added or changed entries are checked, and moving the
object to another gateway checks every entry again, so an entry stored before
the upgrade does not block unrelated edits. The pre-upgrade audit lists stored
`GET` entries on a health path and stored Enterprise-only namespaces on CE
gateways; it does not check the other rules. The operator validates every
render, Enterprise included, with the embedded CE `krakend` binary, so these
checks also refuse features only the Enterprise binary accepts, such as
`{input_headers.X}` and `{input_query_strings.x}` placeholders and reserved-path
variants the Enterprise binary allows.

**Gateway writes are checked too.** A new KrakenDGateway's root config
(`spec.config`, including `extraConfig`) must pass `krakend check` on its own.
An update is rejected if it turns the gateway's passing config, with its
endpoints, into a failing one, for example a `router.healthPath` onto an
existing route, or switching `edition: EE` to `CE` while `/prefix/*` endpoints
exist. The denial puts the root findings on `spec.config` (at most 20, each cut
to a bounded length) and the endpoints the change breaks, which can belong to
other objects, on `spec`. When the gateway's config already fails, the update
is judged on the root alone, against the stored root, and the existing failure
is a warning. A `spec.version` other than 2.13.x gets a warning when it is set
or changed: validation uses the pinned 2.13 binary. **On a CE gateway,
Enterprise-only namespaces are rejected** in `spec.config.extraConfig` when it
is set or changed, and switching `edition: EE` to `CE` is rejected while the
gateway's KrakenDEndpoints or their KrakenDBackendPolicies use one; the denial
lists each object, field and namespace. So are the typed Enterprise fields
`spec.redis`, `spec.config.documentation`, `spec.openapi.enabled: true` and
`spec.dragonfly.enabled: true` on a CE gateway (`Forbidden`): `spec.redis` and
`spec.config.documentation` when set or changed, the export and Dragonfly when
enabled, and any of them kept through an EE to CE switch. CE ignores the first
two, the CE binary cannot run the OpenAPI export, so a CE gateway runs without
it, and a CE gateway has no Redis connection pools to use Dragonfly. A stored
use on a CE gateway keeps being accepted: an unchanged `spec.redis` or
`spec.config.documentation` is not judged again, and an edit of the settings of
a stored, enabled export or Dragonfly is admitted with a warning (the export
has no effect, and the Dragonfly instance runs for nothing); turning either off
is admitted. An entry's `documentation/openapi`
(which AutoConfig generates) is not refused: a CE render drops it.

**Policy writes are checked alone and in every gateway that uses them.** A
KrakenDBackendPolicy whose `raw` (or typed fields) KrakenD rejects is refused on
its own, before anything references it, unless the stored policy already failed
on its own too (then its gateways decide). A change to a policy that endpoints reference is rendered in each
gateway of those endpoints and refused if it breaks one that passed
(`breaks gateway ns/name: ...`); a gateway that already fails for another reason
gets a warning instead. The denial lists at most 20 gateways and the warnings
name at most 5, each counting the rest.
A new or changed `raw` with Enterprise-only namespaces, for example `auth/gcp`, or
Enterprise-only keys such as `proxy_address` in `backend/http/client`, is
refused while a CE gateway uses the policy (`gateway ns/name runs CE, which
ignores it silently`); a policy that only sets keys CE honors, such as
`send_body_on_redirect`, is admitted. A stored policy that already does this
keeps accepting unrelated edits. The audit lists the stored ones.

**Updates are ratcheted.** A metadata-only update is never validated. A
reference (`gatewayRef`, `policyRef`) is checked only when it is added or
changed. A field rule rejects an update only if the error is new, so an object
stored before a rule existed keeps accepting unrelated edits. KrakenDEndpoint
entries are matched by (endpoint, method), so reordering `spec.endpoints`
changes nothing, and moving the object to another gateway checks every entry
again. A gateway's sidecar probe that changes is always checked again.

**Deleting a referenced policy is accepted and completes once nothing
references it.** Every KrakenDBackendPolicy gets the finalizer
`gateway.krakend.io/policy-protection` on the operator's first reconcile after
the upgrade. `kubectl delete` of a policy that endpoints still reference now
succeeds instead of being refused. The policy keeps serving (`deletionTimestamp`
set, `status.referencedBy` above 0, and a `DeletionBlocked` warning event that
names the endpoints) until the last referencing KrakenDEndpoint stops
referencing it or is deleted, and then it disappears. A new reference to a
terminating policy is rejected (`policy is being deleted`). The policy webhook
is no longer registered for DELETE, in the Helm chart and in
`operator/config/webhook/manifests.yaml`, so policy and namespace deletion are
accepted without the webhook; they complete once the operator removes the
finalizer. If the operator is down, a policy deletion waits until it is back.
A policy is unprotected until the operator's first reconcile adds the finalizer, which includes the upgrade rollout: the new webhook configuration drops DELETE before the new leader has added finalizers. To roll back or uninstall, remove the finalizer (see [Rollback](#rollback)).

**AutoConfig overrides.** Two `spec.overrides` for the same `operationId`, or
for operationIds that differ only in case or in `_`, `-` and other punctuation
(`get_a` and `get-a`), are rejected: the override engine keys overrides on the
sanitized operationId and could not keep them apart. The denial names the
second entry (`spec.overrides[1].operationId`). A stored list that already does
this keeps accepting edits that add no new collision, and the audit in the
Pre-Upgrade Checklist lists it. A `policyRef` in `defaults`, `overrides` or
`additionalEndpoints` that names no existing policy now produces an admission
warning, not a rejection: a release may create the policy after the
AutoConfig, and the generated endpoints are rejected until it exists.

---

## v0.14.0 — openapi-serve liveness probe (one-time rollout)

The `openapi-serve` sidecar now renders with a liveness probe. It previously
had only a readiness probe, so a wedged sidecar could sit unready forever with
no way to recover; because pod readiness is the AND across containers, that
also held the main `krakend` container out of the Service endpoints
indefinitely. The new probe restarts the sidecar instead.

Two things to know before rolling this out.

1. **Every gateway with `spec.openapi.enabled: true` performs a one-time
   rollout on the first reconcile after the upgrade, with no CR change.**
   Adding a probe changes the PodTemplateSpec, which changes the
   pod-template-hash and produces a new ReplicaSet. It is zero-downtime by
   construction (`MaxSurge: 1 / MaxUnavailable: 0`), but it is unannounced, so
   schedule it like any other production rollout. Affected today: the dev,
   preprod and prod `api-gateway` gateways.

   It does **not** re-fire the post-restart Job. The pod-template annotation is
   the config checksum, and Job identity is the config checksum plus the
   postRestartJob-spec projection — a probe change touches neither.

2. **The sidecar can now be killed and restarted by the kubelet.** A container
   that could previously only sit wedged will now show `RESTARTS` and, if the
   failure persists, `CrashLoopBackOff`. See *ReadMe Publishing
   (postRestartJob)* in `docs/runbook.md` for what that looks like and how to
   triage it.

The default probe is a shallow TCP check on the openapi port
(`initialDelaySeconds: 15`, `periodSeconds: 20`, `timeoutSeconds: 2`,
`failureThreshold: 6`), deliberately slacker than the readiness default so
readiness reacts first.

The kubelet acts on the Nth *consecutive* failure, so time-to-action is
`initialDelay + (failureThreshold - 1) x period + timeout`: about **117s** to
restart for the liveness default, against about **23s** to mark unready for the
readiness default. That ordering only holds while
`spec.openapi.readinessProbe` is also left unset — if you override readiness
with a budget slower than ~117s, the liveness default will pre-empt it and
restart the sidecar before readiness has pulled it out of the endpoints.

Override it with `spec.openapi.livenessProbe`. There is no way to disable it —
unset means "use the default", and `livenessProbe: {}` specifies no handler and
is now rejected at admission. Both probe fields are validated by the gateway
webhook; see the field documentation via
`kubectl explain krakendgateway.spec.openapi.livenessProbe`.

---
## v0.13.4 — postRestartJob hardening (breaking behavior change)

This release changes the post-restart Job (`spec.postRestartJob`) in ways
that affect every gateway with it enabled, including production gateways
running `podSecurityContext.runAsUser: 0`. Read this before upgrading any
cluster with `postRestartJob.enabled: true`.

1. **Container hardening now applies by default.** The post-restart Job
   container now defaults to `readOnlyRootFilesystem: true` and
   `capabilities.drop: ["ALL"]` (mirroring the gateway's own container), with
   a `/tmp` `emptyDir` (256Mi `sizeLimit`) mounted to keep the working
   directory writable. **If your script writes anywhere outside `/tmp` (or
   `spec.postRestartJob.workingDir` if overridden) — e.g. `npm install -g`,
   which writes to the image's global npm prefix on the root filesystem —
   it will now fail.** Override `spec.postRestartJob.securityContext.readOnlyRootFilesystem: false`
   for that script, or update the script to install/write under `/tmp`
   (e.g. `npm config set prefix /tmp/npm-global` first). **Known affected
   consumer:** the production `api-gateway` postRestartJob script in
   `AppCluster-Infrastructure/single_cluster/production-csp/config/api-gateway/mycarrier-prod.yaml`
   runs `npm install -g rdme` with no container `securityContext` override —
   this MUST be updated (either the script or an explicit
   `readOnlyRootFilesystem: false` override) before this operator version is
   rolled out to prod, or the job will fail on every run. (Since 2026-08 the
   gateway CRs use the pre-baked `ci/rdme-publisher` image — rdme is no
   longer installed at runtime; see *ReadMe Publishing (postRestartJob)* in
   docs/runbook.md.)
2. **`securityContext`/`podSecurityContext` now MERGE instead of REPLACE.**
   Previously, setting any field in `spec.postRestartJob.securityContext` or
   `podSecurityContext` discarded ALL hardened defaults (e.g. prod's
   `runAsUser: 0` silently dropped `drop: ["ALL"]` and the seccomp profile).
   Now only the fields you set are overridden; unset fields keep the
   hardened default. This is strictly safer than before, but review your
   existing overrides — fields you were implicitly relying on being *unset*
   (e.g. no seccomp profile) will now inherit the operator default.
   **Caveat — `capabilities.drop` is still a wholesale REPLACE, not a
   union.** The field-by-field merge only applies at the object level
   (`securityContext`, `capabilities`, etc.); `capabilities.drop` itself is a
   plain list, and explicitly setting it (e.g.
   `securityContext.capabilities.drop: ["NET_ADMIN"]`) replaces the hardened
   `drop: ["ALL"]` baseline entirely rather than adding to it. If you need
   your own drop list AND want to keep the `ALL` baseline, include `"ALL"`
   in your own list explicitly (e.g. `drop: ["ALL", "NET_ADMIN"]` is
   redundant but harmless; omitting `ALL` silently loses the hardening).
3. **The Job now re-triggers on `postRestartJob` spec changes, not just
   config changes.** The Job's identity checksum now includes a projection
   of the execution-relevant `postRestartJob` spec fields (script, command,
   image, workingDir, env, both securityContexts, serviceAccountName,
   resources), so editing the script (for example) now creates a new Job
   immediately instead of waiting for the next unrelated `krakend.json`
   change. `ttlSecondsAfterFinished` and `podAnnotations`/`podLabels` are
   excluded from the checksum because they are purely operational/cosmetic —
   they never affect whether the script itself completes. `activeDeadline
   Seconds`, `backoffLimit`, and `tmpSizeLimit` are ALSO excluded from the
   checksum (editing them alone never creates a new Job under a new name),
   but — unlike the truly-cosmetic fields — they DO affect whether the
   script can complete: a too-short `activeDeadlineSeconds`, too-low
   `backoffLimit`, or too-small `tmpSizeLimit` all produce a failure
   indistinguishable from a genuine script bug. To keep that fixable without
   reintroducing the pre-PR over-trigger, the reconciler's failure-signal
   gate (item 4 below) re-runs the CURRENT Job in place — same checksum,
   same name — when one of those three knobs changes on a Job that FAILED;
   it never does so for a Job that SUCCEEDED.

   **Upgrade-time side effect:** the checksum *basis* changed, so the value
   already stored in `status.lastPostRestartJobChecksum` (a bare config
   checksum written by <= v0.13.3) can never match the new combined
   checksum. Every gateway with `postRestartJob.enabled: true` therefore
   runs its script exactly once immediately after the new operator starts,
   with no config or spec change — fix the prod `npm install -g rdme`
   script (item 1) *before* rolling the image, not after; this is a hard
   prerequisite of shipping the image, not a trailing follow-up. Expect the
   pre-upgrade Job object to linger alongside the new one until its 24h TTL
   expires — both share the `app.kubernetes.io/component: post-restart-job`
   label, so dashboards will show duplicates for up to 24h. **Rolling back
   to v0.13.3 is symmetric:** the stored combined checksum is unmatchable by
   the old config-only logic, so the script runs once more on rollback.
4. **The Job no longer silently re-runs on its own periodic ~24h TTL
   cadence — this is now a strict "once per revision, ever" gate.**
   `gw.status.lastPostRestartJobChecksum` is now read (previously
   write-only) to skip recreation when `TTLSecondsAfterFinished` garbage
   collects a finished Job object for a revision that already ran. If any
   workflow was relying on the previous (undocumented, believed-unintended)
   daily re-run cadence, it will need an explicit periodic trigger instead
   (e.g. a CronJob) — see PR description for the by-design determination.

   Two remediation paths people may be used to no longer work the same way:
   - **`kubectl delete job <name>` is now a no-op.** The delete re-enqueues a
     reconcile, but once the Job object is gone the reconciler can no longer
     observe whether the deleted Job succeeded or failed, so it always skips
     — no Job, no Event, just a `PostRestartJobSkipped` status Condition
     explaining why (`kubectl describe krakendgateway <name>`).
     **Cross-reference with the knob-edit retry path directly below: doing
     BOTH — deleting the Job AND bumping `activeDeadlineSeconds`/
     `backoffLimit`/`tmpSizeLimit` — does not retry either.** The knob-edit
     retry re-creates the Job only by comparing the new knob values against
     the EXISTING failed Job object (the reconciler Gets the Job by name
     first, and only reaches the knob comparison if that Get succeeds); once
     the Job is deleted, that Get returns not-found and the reconciler takes
     the unobservable-outcome skip branch above instead — it never reaches
     the knob comparison at all. Combining a manual delete with a knob edit
     therefore dead-ends the SAME way a bare delete does (permanently
     skipped for this revision); use the escape hatch below to force a
     re-run once you've deleted the Job.
   - **A Job that exhausts `backoffLimit` due to a transient external
     failure (npm registry outage, DNS blip, ...) will not retry on its
     own** just from the passage of time. Pre-PR this self-healed within
     ~24h via TTL GC + recreate; post-PR the reconciler DOES distinguish
     "ran and failed" from "ran and succeeded" for a still-observable Job
     object (it reads `status.conditions[type=Failed/Complete]`), so a
     targeted retry is available: bump `activeDeadlineSeconds`,
     `backoffLimit`, or `tmpSizeLimit` on the gateway (whichever addresses
     the failure) and the next reconcile deletes and re-creates the FAILED
     Job under the same name/checksum. A Job that SUCCEEDED is never
     re-created this way, even if you edit one of those three knobs
     afterward — that would reintroduce the pre-PR over-trigger. If the Job
     object has already been TTL-GC'd or manually deleted, this retry path
     is unavailable (its outcome is no longer observable) — use the escape
     hatch below instead.

   **Escape hatch — force a re-run without a config or spec change:**
   ```bash
   kubectl patch krakendgateway <name> --subresource=status --type=merge \
     -p '{"status":{"lastPostRestartJobChecksum":""}}'
   ```
5. **New optional `spec.postRestartJob.workingDir`** overrides the
   previously-forced `/tmp` working directory. Unset behaves exactly as
   before (defaults to `/tmp`). **If you override it to a path outside the
   `/tmp` emptyDir, your script's CWD lands on the read-only root
   filesystem** under the new `readOnlyRootFilesystem: true` default (item
   1) — the container still starts fine (the directory is created before
   the read-only remount), but relative-path writes then fail with a bare
   `EROFS`. Use an absolute path under `/tmp`, or set
   `spec.postRestartJob.securityContext.readOnlyRootFilesystem: false`
   deliberately if you need a writable directory elsewhere.
6. **New optional `spec.postRestartJob.tmpSizeLimit`** overrides the
   previously-hardcoded 256Mi `/tmp` `emptyDir` size limit. Increase this if
   your script writes more than 256Mi under `/tmp` — exceeding the limit
   gets the pod `Evicted` mid-run by the kubelet (not a clean script-level
   failure), so size this generously if your script's write volume is
   uncertain.
7. **Dragonfly's `securityContext`/`podSecurityContext` now MERGE instead of
   REPLACE, matching the Job's behavior (item 2 above).** This closes the
   follow-up tracked by a previous release of this guide.
   `spec.dragonfly.podSecurityContext` / `spec.dragonfly.containerSecurityContext`
   are now handled by `mergeDragonflyPodSecurityContext`/
   `mergeDragonflyContainerSecurityContext` in
   `internal/resources/dragonfly.go`, which strategic-merge a user override
   on top of the hardened defaults (`runAsNonRoot: true`, `runAsUser`/
   `runAsGroup: 999`, and — pod scope only — `fsGroup: 999`) via the same
   `strategicMergeSecurityContext` helper the Job uses. **Previously,
   setting ANY field in either securityContext discarded the ENTIRE
   hardened default, including `fsGroup: 999`** — a live ownership hazard
   for PVC-backed Dragonfly instances (the image's built-in `dfly` uid/gid
   999 process loses group-write access to `--dir=/dragonfly/snapshots` and
   crashloops). Now only the fields you set are overridden; unset fields
   keep the hardened default, so a `spec.dragonfly.podSecurityContext`
   override that sets only `runAsUser` (for example) keeps `fsGroup: 999`
   automatically. **This is strictly safer than before for fields the
   container-scope default does not itself pin (e.g. `fsGroup`, or a
   `podSecurityContext.runAsUser`/`runAsGroup` override for OTHER
   containers/sidecars in the pod) — one exception:** setting only
   `podSecurityContext.runAsUser`/`runAsGroup` and expecting it to change
   the `dragonfly` container's OWN effective uid/gid is not "safer", it is
   simply *ineffective*, both before and after this change — see "Pod-scope
   `runAs*` fields do NOT change the Dragonfly container's effective
   identity" below for why, and use `containerSecurityContext` instead.
   Concrete example: `containerSecurityContext: {allowPrivilegeEscalation:
   false}` (no `runAsUser` set) combined with `podSecurityContext:
   {runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}` still renders
   `containerSecurityContext.runAsUser: 999` / `runAsGroup: 999` on the
   emitted Dragonfly CR — the container-scope default's `999` pins win
   regardless of the pod-scope override, because setting *any* field in
   `containerSecurityContext` does not implicitly carry your pod-scope
   `runAsUser`/`runAsGroup` values down into it. **If `runAsUser`/
   `runAsGroup` must differ from `999` for the `dragonfly` container
   itself, set them explicitly in `spec.dragonfly.containerSecurityContext`**,
   not (only) in `podSecurityContext`. Otherwise, review your existing
   overrides — fields you were implicitly relying on being *unset* will now
   inherit the operator default instead of being absent. **If your spec was
   relying on the previous replace-to-clear behavior** (e.g. setting
   `podSecurityContext: {runAsUser: 1234}` specifically to end up *without*
   `runAsNonRoot: true`), you must now set that field explicitly (e.g.
   `runAsNonRoot: false`) to get the old effective result.

   **(Fix-round follow-up, post-initial-release) — the escape hatch now
   genuinely works, at BOTH scopes.** The initial merge behavior above left
   a gap: dragonfly's container-level default *pins*
   `runAsNonRoot`/`runAsUser`/`runAsGroup: 999` (unlike the Job's container
   default, which leaves `runAsUser`/`runAsNonRoot` unset), and
   container-scope security-context fields always override the pod-scope
   value for the SAME field at the kubelet. So setting only
   `containerSecurityContext.runAsUser: 0` — with no explicit
   `runAsNonRoot` — always merged against the inherited container-scope
   `runAsNonRoot: true` default, no matter what `podSecurityContext` said,
   rendering the kubelet-rejected `{runAsUser: 0, runAsNonRoot: true}` pair.
   `mergeDragonflyContainerSecurityContext` now carries the same kind of
   post-merge fixup the Job's pod-scope merge already had (see item 2):
   if you set `containerSecurityContext.runAsUser: 0` without also setting
   `containerSecurityContext.runAsNonRoot`, the inherited `runAsNonRoot: true`
   default is dropped, so `podSecurityContext.runAsNonRoot: false` (or an
   explicit `containerSecurityContext.runAsNonRoot: false`) now actually
   takes effect and renders a startable container. Setting
   `runAsNonRoot: false` at EITHER scope (container or pod) now works as
   the escape hatch it was always documented to be.

   **Corrected claim — "only the fields you set are overridden" was
   incomplete; here is the exact projected field set.** The paragraph above
   is true of the MERGE step, but the merge result is not what reaches the
   Dragonfly CR verbatim: `BuildDragonfly`'s `buildPodSecurityContext`/
   `buildSecurityContext` (`internal/resources/dragonfly.go`) project only a
   FIXED WHITELIST of fields onto the emitted `spec.podSecurityContext` /
   `spec.containerSecurityContext` maps:
   - **Pod scope:** `runAsNonRoot`, `runAsUser`, `runAsGroup`, `fsGroup`.
   - **Container scope:** `runAsNonRoot`, `runAsUser`, `runAsGroup`,
     `allowPrivilegeEscalation`.

   Any other field you set in `podSecurityContext`/`containerSecurityContext`
   (e.g. `seccompProfile`, `capabilities`, `sysctls`) is accepted by the CRD
   schema (the Go API type embeds the full upstream `corev1.SecurityContext`/
   `PodSecurityContext`) and survives the merge step internally, but is
   **silently NOT propagated** to the rendered Dragonfly CR — a pre-existing
   projection gap, not something this fix-round changed; tracked as a
   follow-up. **This supersedes the previous "plain list fields still
   REPLACE wholesale, not union" caveat**: `capabilities.drop` specifically
   is currently discarded entirely for Dragonfly (it is outside the
   whitelist above), not merely subject to the Job's list-replace caveat —
   there is no Dragonfly hardened `capabilities.drop` default to replace or
   union with in the first place.

   **Pod-scope `runAs*` fields do NOT change the Dragonfly container's
   effective identity — set `containerSecurityContext` instead.** Because
   the container-scope default pins `runAsUser`/`runAsGroup: 999` and
   container-scope always wins over pod-scope per-field at the kubelet,
   `podSecurityContext.runAsUser`/`runAsGroup`/`runAsNonRoot` never change
   what uid/gid the `dragonfly` container itself actually runs as — only
   `containerSecurityContext` does. (Pod-scope values still apply normally
   to any OTHER container/init-container in the pod that doesn't set its
   own override, and `podSecurityContext.fsGroup` still governs volume
   ownership regardless of container-scope settings.) A direct consequence:
   **`spec.dragonfly.podSecurityContext.runAsUser: 0` with `runAsNonRoot`
   left unset is now REJECTED at admission for any NEW or CHANGED spec.**
   **Corrected history (round-2 review):** an earlier revision of this
   entry claimed this shape "was previously silently accepted and
   self-healed at build time" — that is false; the pre-merge (`main`)
   builder had no self-heal at all. Before this merge-semantics change,
   `spec.dragonfly.podSecurityContext` (when non-nil) fully REPLACED the
   hardened default wholesale, so a pod-scope `runAsUser: 0` request
   rendered EXACTLY as the user wrote it — `{runAsUser: 0}`, with no
   `runAsNonRoot` key at all, because the default `runAsNonRoot: true` was
   never in the picture to merge against in the first place. The real delta
   introduced by the merge change (item 7 above) is that the default now
   merges in ALONGSIDE a user's uid0 request and re-introduces the
   kubelet-invalid `{0, true}` pair — a regression the merge change created,
   not a pre-existing self-heal it removed. A later fix-round restored a
   build-time fixup in `mergeDragonflyPodSecurityContext` (now cross-scope
   aware — see the next paragraph) specifically to keep GRANDFATHERED specs
   (ones that reach the builder without going through this admission check,
   via the update-ratchet or a bypassed webhook) rendering at that same
   `main`-branch parity, rather than newly breaking on the merge change.
   Since a pod-scope-only root request can never grant a real capability for
   the dragonfly container itself (only for other sidecars, silently), there
   was no legitimate NEW case to preserve at admission time, so the webhook
   requires you to acknowledge the choice explicitly (`runAsNonRoot: false`
   at either scope) rather than masking it. The update-ratchet (see item 9)
   still grandfathers a pre-existing pod-scope-`runAsUser: 0` CR unchanged
   on an unrelated update.

   **The build-time fixup restored above is now CROSS-SCOPE AWARE (round-2
   fix), not just pod-scope-keyed.** `mergeDragonflyPodSecurityContext` also
   drops the pod-level `runAsNonRoot` default when the user's
   `containerSecurityContext` alone sets `runAsUser: 0` with its own
   `runAsNonRoot` left unset — otherwise the container-scope fixup (which
   clears the CONTAINER-level default) would leave the container falling
   back to a still-`true` POD-level default, silently reintroducing the same
   broken pair one level up. This fixup is deliberately NOT applied when the
   user's own `podSecurityContext` is `nil`: that legacy shape
   (`containerSecurityContext` uid0 alone, no `podSecurityContext` at all)
   already rendered the broken `{0, true}` pair on `main` too (full-replace
   only ever protected a non-nil user `podSecurityContext`), so leaving it
   broken is the correct parity outcome, not a regression to fix here. An
   explicit user `runAsNonRoot` value at either scope is never touched by
   this fixup.

   More generally (not just the pod-scope-unset case above): the admission
   webhook hard-rejects any `spec.dragonfly` whose EFFECTIVE
   `{runAsUser, runAsNonRoot}` pair resolves to `{0, true}` (container-scope
   value falling back to pod-scope, mirroring item 9 below) — set
   `runAsNonRoot: false` (container or pod scope) to acknowledge running as
   root and pass validation. **Round-2 correction to the opt-out rule:** an
   opt-out is only a valid acknowledgment from the SCOPE THAT PRODUCED the
   effective root request, or from pod scope (which always inherits down to
   every container that sets nothing of its own). A pod-scope
   `runAsNonRoot: false` opt-out is therefore always accepted, regardless of
   which scope's `runAsUser: 0` triggered the check — but a CONTAINER-scope
   `runAsNonRoot: false` opt-out is accepted only when the root request
   itself came from that same container scope; it can no longer mask a
   POD-scope `runAsUser: 0` request, since other sidecars/extra containers
   in the pod that set nothing of their own still silently inherit the
   pod-scope pair regardless of what this one container opted out of.
   **One exception — the documented container-root recipe:** when the
   container scope carries its OWN `runAsUser: 0`, the whole spec routes
   through the container-path rules, so the container's
   `runAsNonRoot: false` acknowledges it and the spec is ADMITTED even when
   `podSecurityContext` also requests `runAsUser: 0`
   (`containerSecurityContext: {runAsUser: 0, runAsNonRoot: false}` +
   `podSecurityContext: {runAsUser: 0}` is equivalent to
   `podSecurityContext: {runAsUser: 0, runAsNonRoot: false}` alone) —
   including with an explicit pod-scope `runAsNonRoot: true`, which is then
   rendered as the kubelet-invalid `{runAsUser: 0, runAsNonRoot: true}`
   pod-level pair exactly as written (see "What stays unprotected" below).
   These admitted shapes are still reported by the
   `DragonflyRunAsRootUnacknowledged` status condition as
   `RunAsRootUnacknowledged`: the condition is deliberately stricter than
   admission (a pod-scope root request is only ever acknowledged by a
   pod-scope `runAsNonRoot: false`), so a `True` condition on an admitted
   spec is by design, not a bug. Conversely, a pod-scope `runAsUser: 0`
   alongside a NON-zero container-scope `runAsUser`, or alongside an
   explicit `runAsNonRoot: true` with no container-scope root request, is
   now REJECTED on any NEW or CHANGED spec even though the dragonfly
   container's own effective uid is not 0 — if you carry such a spec, add
   `podSecurityContext.runAsNonRoot: false` or drop the pod-scope
   `runAsUser: 0`; stored specs are unaffected until their
   `securityContext`/`podSecurityContext` fields change (update-ratchet).

   **OnDelete recovery — an upgrade to this fix-round does NOT, by itself,
   heal an already-crashlooping Dragonfly instance.** If a Dragonfly pod is
   already crashlooping because it lost `fsGroup: 999` under the pre-item-7
   full-replace bug (see the ownership hazard described above), upgrading
   the operator corrects what the CONTROLLER will render for the Dragonfly
   CR's `spec.podSecurityContext` going forward, but the Dragonfly
   StatefulSet's `updateStrategy` is `OnDelete` — existing pods are NOT
   automatically recreated on a spec change. After upgrading, manually
   recycle the affected pod(s) to pick up the corrected
   `podSecurityContext`:
   ```bash
   kubectl delete pod <dragonfly-pod-name> -n <namespace>
   ```
   The StatefulSet controller then recreates it from the now-corrected spec.

   **This admission protection exists ONLY in the ValidatingWebhookConfiguration
   — it is silently bypassed whenever the webhook isn't actively enforcing**
   (mirrors item 9's warning for `postRestartJob`): `webhooks.enabled: false`
   in the Helm chart, cert-manager absent (the webhook's serving certificate
   never issues, so the webhook pod never becomes Ready), or plain webhook
   pod downtime. When the webhook isn't enforcing, the ONLY remaining
   protection against a Dragonfly spec rendering the kubelet-invalid
   `{runAsUser: 0, runAsNonRoot: true}` pair is the runtime builder fixups in
   `internal/resources/dragonfly.go`:
   - `mergeDragonflyContainerSecurityContext`'s container-scope fixup, which
     always applies when `containerSecurityContext.runAsUser: 0` is set with
     `runAsNonRoot` left unset — this is unconditional, not
     grandfathered-only.
   - `mergeDragonflyPodSecurityContext`'s cross-scope fixup (round-2), which
     covers a `podSecurityContext.runAsUser: 0` request and a
     `containerSecurityContext.runAsUser: 0`-with-unset-`runAsNonRoot`
     request, as long as the user's own `podSecurityContext` is non-nil.

   **What stays unprotected even with these builder fixups in place** (i.e.
   still renders a kubelet-invalid pair with the webhook bypassed):
   - **An explicit `runAsNonRoot: true` set alongside a `runAsUser: 0` in the
     SAME scope** renders exactly as written — neither fixup touches a field
     the user explicitly set, by design. On its own (no acknowledgment at the
     OTHER scope), the webhook rejects this shape outright — it is a
     deliberate, self-contradictory user choice the admission check would
     normally catch. But when the OTHER scope's `runAsNonRoot: false`
     acknowledges the root request and admits the spec through that scope's
     rules (see the container-root recipe exception above), the same-scope
     invalid pair is still admitted and rendered exactly as written — both
     `containerSecurityContext: {runAsUser: 0, runAsNonRoot: false}` +
     `podSecurityContext: {runAsUser: 0, runAsNonRoot: true}` and
     `containerSecurityContext: {runAsUser: 0, runAsNonRoot: true}` +
     `podSecurityContext: {runAsNonRoot: false}` fall into this admitted,
     invalid-pair-as-written category.
   - **The legacy `containerSecurityContext` uid0-alone shape with
     `podSecurityContext` entirely `nil`** stays exactly as kubelet-broken
     with the webhook bypassed as it was on `main` before this fix-round —
     see the "Corrected history" note above for why this is intentional
     parity, not a residual bug.

   Ensure the webhook is actually installed and Ready before relying on a
   Dragonfly `runAsUser: 0` spec being caught at admission time; the builder
   fixups above are defense-in-depth for grandfathered/bypassed paths only,
   not a substitute for admission-time validation on new or changed specs.

   Dragonfly has no config-checksum identity analogous to the Job's (item 3
   above uses a checksum to decide whether to re-trigger the Job; Dragonfly
   CRs are reconciled continuously and have no equivalent re-trigger gate),
   so this change carries no checksum/re-trigger side effect.
8. **The Job now carries two distinct checksum annotations**, not one
   overloaded key. `krakend.io/checksum-config` keeps its original meaning
   (the raw, invertible krakend.json config checksum — traceable back to a
   config revision); a new `krakend.io/checksum-postrestart` carries the
   combined identity checksum that drives Job naming/idempotency. If any
   external tooling was reading `krakend.io/checksum-config` off the Job
   (not the Deployment) expecting the combined value from an earlier
   pre-release of this change, update it to read
   `krakend.io/checksum-postrestart` instead.
9. **The admission webhook now hard-rejects `postRestartJob` specs whose
   EFFECTIVE `{runAsUser, runAsNonRoot}` pair resolves to `{0, true}`**
   (container-scope value falling back to pod-scope, exactly as the
   kubelet resolves it) — this covers both a container-level
   `securityContext.runAsUser: 0` and a pod-level
   `podSecurityContext.runAsUser: 0` combined with an explicit
   `runAsNonRoot: true` at either scope. Set `runAsNonRoot: false`
   (container or pod scope) to acknowledge running as root and pass
   validation. **Ratcheted on Update:** a CR that already carried
   `runAsUser: 0` from before this check existed (accepted by an older
   operator version) keeps working on unrelated updates as long as the
   relevant securityContext fields are unchanged from the stored spec;
   only a Create, or an Update that actually introduces/changes the
   offending combination, is rejected. **In practice this means ordinary
   GitOps reconciliation (ArgoCD/Flux re-applying an unchanged manifest, or
   editing unrelated fields) is unaffected by the ratchet** — the exposure is
   confined to a genuine Create (a brand-new CR, or one deleted and
   recreated) or a deliberate edit to `securityContext`/`podSecurityContext`
   on a CR carrying this shape.

   **Round-2 correction to the opt-out rule (shared with Dragonfly's item 7
   above — both scopes use the same `validateRunAsRootConflict` helper):**
   a pod-scope `runAsNonRoot: false` opt-out is always accepted, regardless
   of which scope's `runAsUser: 0` triggered the check (pod scope inherits
   down to every container that sets nothing of its own). A container-scope
   `runAsNonRoot: false` opt-out is accepted only when the effective
   `runAsUser: 0` itself came from that same container scope — it no longer
   masks a pod-scope-originated root request on its own. In practice this
   rarely changes an outcome for `postRestartJob` specifically, since the
   Job has only a single container: the pod-scope-unset self-heal carve-out
   (`allowPodScopeUnsetSelfHeal: true`, see the builder-fixup paragraph
   below) already admits the common pod-scope-root-with-unset-`runAsNonRoot`
   shape through a different path. It matters for the self-contradictory
   edge case of an EXPLICIT pod-scope `runAsNonRoot: true` alongside a
   container-scope `runAsNonRoot: false` opt-out — that combination is now
   rejected instead of silently masked.

   **This protection exists ONLY in the ValidatingWebhookConfiguration —
   it is silently bypassed whenever the webhook isn't actively
   enforcing:** `webhooks.enabled: false` in the Helm chart, cert-manager
   absent (the webhook's serving certificate never issues, so the webhook
   pod never becomes Ready), or plain webhook pod downtime. (Note:
   `make deploy`, the Kustomize path, DOES install the webhook by
   default — this is not a Kustomize-vs-Helm default gap, only an
   "is the webhook actually reachable and Ready" gap.) When the webhook
   isn't enforcing, the ONLY remaining protection against `runAsUser: 0`
   hanging the Job pod Pending until `activeDeadlineSeconds` expires is
   the runtime builder fixup in `internal/resources/job.go`
   (`mergePodSecurityContext`) — and that fixup only self-heals the
   pod-scope, `runAsNonRoot`-left-entirely-unset case; it does NOT protect
   the container-scope case or the pod-scope-with-explicit-`runAsNonRoot:
   true` case. Ensure the webhook is actually installed and Ready before
   relying on `runAsUser: 0` being caught at admission time.

   **Ratchet correction (round 4):** the ratchet above only ever applies to
   a previously-**enabled** spec. A spec stored with `postRestartJob.
   enabled: false` (whatever its `securityContext`/`podSecurityContext`
   happened to contain — it was never validated, since a disabled Job never
   reaches this check) does NOT grandfather those fields when a later
   update flips `enabled: true`; that update gets the full
   `runAsUser`/`runAsNonRoot` check exactly like a fresh Create. If you have
   a disabled `postRestartJob` block carrying `runAsUser: 0` with no
   `runAsNonRoot: false` escape hatch, set `runAsNonRoot: false` (or remove
   `runAsUser: 0`) before enabling it, or the update will be rejected.

10. **`Image` and `serviceAccountName` are now also hashed at their
    EFFECTIVE (post-default) value, not the raw spec field, in the Job
    identity checksum (round 4 correction).** Item 3 above already listed
    `image` and `serviceAccountName` as checksum inputs; what changed here
    is a bug in *how* they were hashed. Round 3 normalized `command` and
    `workingDir` to their effective values so "unset" and "explicitly set to
    the documented default" converge on one checksum — round 3 missed
    `image` (`DefaultPostRestartJobImage`, `bash:5.2`) and
    `serviceAccountName` (defaults to the gateway's own name). Before this
    fix, a gateway with `postRestartJob.image` or `.serviceAccountName`
    unset that later had the value written explicitly to its documented
    default (e.g. by a GitOps tool normalizing manifests, or a user being
    explicit) minted a spurious new checksum/Job name and re-ran the script
    even though the rendered Job was byte-identical. This is now fixed the
    same way `command`/`workingDir` were.

    **Steady state, once a gateway is already running on this fixed
    version:** no action needed. Toggling `image`/`serviceAccountName`
    between unset and "explicitly set to the documented default" is a
    one-time convergence, not a re-trigger — the checksum is the same
    either way.

    **Upgrade transition, for a gateway coming from <= v0.13.3 with
    `image`/`serviceAccountName` left unset:** the claim above does NOT
    apply unchanged. This fix changes WHICH new checksum gets computed
    during the mandatory upgrade-time run already described in item 3 above
    (raw-vs-effective hashing of the unset field feeds a different combined
    checksum than it would have without this fix). It does not change HOW
    MANY TIMES the script runs — item 3's "runs its script exactly once
    immediately after the new operator starts" already accounts for every
    gateway on this upgrade path, including this one; this fix only changes
    which checksum that single mandatory run lands on, still exactly once.

    **Standing rule going forward:** any default value consumed by one of
    the `effective*` helpers in `internal/resources/job.go`
    (`effectivePostRestartCommand`, `effectivePostRestartImage`,
    `effectivePostRestartWorkingDir`, `effectivePostRestartServiceAccountName`)
    is part of the Job identity checksum. TWO kinds of change are fleet-wide
    re-triggers for every gateway that left the field unset, and BOTH must
    add an upgrade-guide entry here, the same way this entry documents this
    release's fixes: (1) changing one of those default values in a future
    release (not just adding a new field); (2) switching a field's hash
    policy between RAW and EFFECTIVE — i.e. changing whether "unset" and
    "explicitly set to the default" converge on one checksum — which has the
    same fleet-wide shape as (1) even though no default value changed, since
    it changes which checksum an unset field's gateway computes on next
    reconcile. This item 10 fix is itself an instance of (2), not (1) — see
    `internal/resources/job.go`'s RULE comment above `DefaultPostRestartJobImage`
    for the code-side statement of this same scope.

11. **Removing a `postRestartJob` knob override (`activeDeadlineSeconds`,
    `backoffLimit`, `tmpSizeLimit`) is invisible to the retry/re-trigger
    logic on a FAILED Job — this is an accepted limitation, not a bug.**
    `postRestartJobExecutionKnobsChanged` only compares a knob when the
    spec field is explicitly set (`spec.BackoffLimit != nil && ...`); if you
    delete a previously-set override entirely (the field goes from set back
    to unset/nil), that guard short-circuits and the knob is treated as
    "not compared", not "compared and found equal" — so a removal can never
    trigger a retry of a failed Job on its own, even though it may change
    the value the Job would actually be built with. **If you want to retry
    a failed post-restart Job by adjusting one of these three knobs, set it
    to a new explicit value rather than removing an existing override** —
    that is unambiguous and always observable to the reconciler. Removing
    an override is not detected as a change in and of itself.
