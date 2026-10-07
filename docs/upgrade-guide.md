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
   It prints one line per object or conflict, including a backend host or an
   endpoint path that holds whitespace or a control character. No output means
   none of the checks found anything; it does not cover other reserved paths under
   `/__debug`, `/__echo` and `/__health` (a GET on the gateway's own health
   path is reported), unnamed `/*` wildcards on CE gateways, unknown
   `urlPattern` placeholders or cross-method `auto_options` clashes. Fix or
   knowingly accept each line before upgrading. What a listed object blocks
   depends on the rule it breaks:
   - A stored value that breaks a field rule (a pattern, an enum, a minimum, a
     length, a `tmpSizeLimit` outside the allowed forms, an endpoint
     `timeout` or `cacheTTL` that is not a duration, or a backend host or
     endpoint path with whitespace) keeps being accepted on unrelated updates;
     only a change to that field must fix it. Items of a
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
     Only a changed entry, a new `policyRef` to such a policy, a changed root
     `extraConfig`, a changed policy `raw`, a changed `spec.redis` or
     `spec.config.documentation`, or enabling `spec.openapi` or
     `spec.dragonfly`, is rejected; so is switching the
     gateway to CE while the last two are enabled. A stored use that stays
     keeps being accepted: editing the settings of a stored, enabled
     `spec.openapi` or `spec.dragonfly` on a CE gateway only warns, and turning
     one off is admitted.
6. **Audit what the AutoConfig controller will now hold, adopt or delete.**
   Run the audits under *Unreleased — AutoConfig per-operation failures,
   ownership and readiness* (read-only, `kubectl get` and `jq`):
   - the ownership audit lists label-matched endpoints the AutoConfig will
     adopt, and controlled endpoints whose labels no longer name their
     controller; it is the one change that can delete endpoints the
     AutoConfig does not own today. An AutoConfig that recovers from
     `CUEEvaluationFailed` also deletes the stale endpoints it kept while it
     was failing (see the recovery list in that section);
   - the override audit lists every AutoConfig's overrides (it needs `curl`,
     and `yq` for YAML specs, to list each spec's duplicated operationIds);
     match them against the duplicates and the operation's backend count, to
     find the ones that will fail closed: an operationId the spec declares
     more than once, or a backend index out of range;
   - the `KrakenDAutoConfig ns/name: endpoints share a route…` line that the
     admission audit in item 5 prints lists same-shape endpoint pairs of one
     AutoConfig. After the upgrade such an AutoConfig reports
     `OperationsFailed` and keeps its stale endpoints until the pair is
     resolved.

7. **Check the chart values and the operator's new defaults** (Helm
   installs; read-only). This prints only the values you set, so `null` means the
   chart default applies, and this release changes the `replicaCount` default
   to 2:
   ```bash
   helm -n krakend-operator-system get values krakend-operator -o json \
     | jq '{replicaCount, leaderElection, webhooks: {enabled: .webhooks.enabled, failurePolicy: .webhooks.failurePolicy}, metrics}'
   ```
   - A release with `leaderElection.enabled: false` and a `replicaCount` of
     `null` (not set explicitly to 1) no longer renders, because `replicaCount`
     now defaults to 2. Set `replicaCount: 1` or re-enable leader election before
     upgrading. Otherwise the operator Deployment goes from 1 to 2 pods and a
     PodDisruptionBudget appears. On a single-node cluster, `kubectl drain`
     blocks on the second operator pod because of that PodDisruptionBudget: set
     `podDisruptionBudget.enabled: false` or `replicaCount: 1` there.
   - If you set `resources` for the operator in your own values or manifests,
     raise the memory limit to 512Mi (the default was 256Mi): up to three
     `krakend check` runs share the container.
   - Metrics scrapes start succeeding once the scraper's ServiceAccount is
     bound to the `<fullname>-metrics-reader` ClusterRole.
   - OLM installs gain the admission webhooks (`failurePolicy: Fail`), so
     writes to the four KrakenD kinds are rejected while the operator is
     unavailable.

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

### RBAC (ClusterRole) copies are generated

The manager's ClusterRole exists in three places, all generated from the
`+kubebuilder:rbac` markers:

- `operator/config/rbac/role.yaml` — `make manifests` (controller-gen);
- the CSV's `clusterPermissions` (`make bundle`), from the above;
- `charts/krakend-operator/files/manager-role.yaml` — copied by
  `make manifests`; `templates/clusterrole.yaml` renders its rules.

`make verify-manifests` (the CI `Manifests Drift` job) fails when
`config/rbac/role.yaml`, the chart copy or the OLM bundle's
ClusterServiceVersion is stale, and
`TestManagerRoleGrantsOnlyUsedVerbs` fails when the generated role differs
from the hand-kept table of verbs the controllers use. The integration suite
runs its manager under that role against K3s, with the VirtualService CRD
installed, so a verb that is missing for a path the suite runs fails there.
Dragonfly, ExternalSecret and the optional kinds' `delete` are covered by the
table alone.

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
   kubectl get events -A -o json | jq -r '
     .items[] | select(.reason == "GatewayRootInvalid" or .reason == "CombinedConfigInvalid" or .reason == "InvalidEndpointsExcluded" or .reason == "RolloutFailed")
     | "\(.lastTimestamp)\t\(.involvedObject.namespace)/\(.involvedObject.name)\t\(.reason)"' | sort
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

> **Downgrading the operator past *Operator RBAC, caching and availability*.**
> v0.14.0 needs `update` on ConfigMaps and on `krakendendpoints/status`, and
> `patch` on `krakendgateways` and `krakendgateways/status` (the Enterprise
> license monitor). The trimmed ClusterRole grants none of them. `helm rollback` restores the older
> role together with the older operator. `make deploy IMG=…:<previous>` run
> from a current checkout applies the trimmed role to the older binary, which
> then fails with `forbidden: User …`. Check out the previous release's
> manifests first:
>
> ```bash
> git checkout v<previous-version>
> make deploy IMG=ghcr.io/mycarrier-devops/krakend-operator:<previous-version>
> ```
>
> `make deploy` also re-applies that release's CRDs, because `config/default`
> includes them. With kustomize the older CRD therefore prunes
> `status.configEdition` on the older operator's next write, as described in
> the *Gateway reconcile correctness* note above.

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
  the next reconcile or resync. While the AutoConfig is in `Error`, no stale
  endpoint is deleted. A failure before the endpoint writes leaves every
  endpoint as it is. Held operations or failed writes leave their own
  endpoints unchanged while the other operations still converge (see
  *Unreleased — AutoConfig per-operation failures, ownership and readiness*).
- Steady state writes nothing: a reconcile that finds no change writes no
  status and emits no event. `status.lastSyncTime` and the
  `EndpointsGenerated` event update only when the spec/CUE-definitions/
  generation inputs or the generated endpoints changed; the
  `SpecWarning`, `DuplicateOperationId`, and
  `AdditionalEndpointOverride` warning events fire only when those inputs
  differ from the last successful sync's. A failed sync doesn't record its
  inputs, so they repeat on each retry of a failing sync whose inputs
  changed; a spec fetch failure emits `SpecFetchFailed` instead. A sync that
  only holds operations (`OperationsFailed`) records its inputs like a
  successful one, so its warnings are not repeated on the next resync.
- Endpoint write failures fail the sync: when a generated endpoint can't be
  created, updated, or deleted for a reason other than a write conflict (e.g.
  the API server or an admission webhook times out or returns a server
  error), the AutoConfig goes to `status.phase: Error`
  with `Synced=False`, reason `EndpointReconcileFailed`, and a matching
  `Warning` event. This always retries with backoff, on both `OnChange` and
  `Periodic` AutoConfigs — a `Periodic` AutoConfig no longer waits a whole
  `spec.periodic.interval` to retry what's usually a transient write error.
  Since the per-operation change, a write the API server rejects as invalid,
  or a name another object controls, holds only that operation
  (`OperationsFailed`) and is not retried with backoff. Only other write
  errors fail the sync this way.
- Write conflicts retry quietly, not as a failure: a stale-cache `Conflict` on
  a status write, or a `Conflict`/`AlreadyExists` on an endpoint write (this
  reconcile raced another and lost), requeues one second later with no error
  log, no event, and no status change — it does not set
  `EndpointReconcileFailed` and does not touch `status.phase`. The
  `SpecWarning`/`DuplicateOperationId`/`AdditionalEndpointOverride`
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
  warnings, not failures, listed in `status.warnings`. See *Before upgrading*
  below.
- A `$ref`-shaped value inside example data is no longer fetched. The value
  of an `example` field, the content of an `examples` field and everything
  inside an Example Object that an `examples` entry points to (its `value`,
  or a raw payload file) are data, so a `{"$ref": "…"}` in them is neither
  resolved nor rewritten, and a fetch that cannot succeed no longer fails the
  sync (this fixes a regression in the fail-closed behaviour above: such a
  spec went to `SpecFetchFailed` on every resync). An Example Object's own
  `$ref`, an entry of `examples` or of `components.examples` that is itself a
  `{"$ref": "…"}`, is still fetched and still fails the sync closed, as does
  a link of a root `$ref` chain in the fetched target. The target is inlined
  under `components/examples`, not `components/schemas`, at its sanitized
  name, or with a `_2`, `_3` suffix when the spec's own `components.examples`
  already holds that name. A
  schema, response, header or other member of a name-keyed map (for example
  `components.schemas.example`) is an object whatever it is named, so it is
  still resolved too.
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
  `Synced` or `Error`, and the phase is empty before the first sync.
- Spec fetch, CUE, unmatched-override, ambiguous-override, and
  additional-endpoint scope failures retry via controller-runtime's
  exponential backoff (`OnChange`) or at `spec.periodic.interval`
  (`Periodic`), same as before; endpoint write failures and write conflicts
  follow the different rules described above, and held operations
  (`OperationsFailed`) are not errors (see *Unreleased — AutoConfig per-operation failures, ownership and readiness*).

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
A `$ref` inside example data (an `example` or `examples` field) is not fetched
and can be ignored, unless it is an `examples` entry that is itself a
`{"$ref": "…"}`, which is still resolved.

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
an override on an `operationId` the spec declares more than once: it fails
the sync with reason `AmbiguousOverride` instead of landing on one of the
operations (see *Unreleased — AutoConfig per-operation failures, ownership and readiness*).

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
overrides are matched on the operationId contract. The `_overrides` keys are
the `SanitizeName` form of each override's `operationId`: lowercased, every
character outside `a-z`, `0-9` and `-` replaced by `-`, and leading and
trailing `-` trimmed (`getUser_v2` is `getuser-v2`). A definition that looks
an override up by `strings.ToLower(op.operationId)` misses any id containing
another character; use the same form (with `regexp` and `strings` imported):

```cue
import "regexp"
import "strings"

_overrides[strings.Trim(regexp.ReplaceAll("[^a-z0-9-]", strings.ToLower(op.operationId), "-"), "-")]
```

Separately, an operation that fails evaluation (for example, one that failed
to convert) is no longer dropped silently with only an event: it is held and
listed in `status.failedOperations` (see *Unreleased — AutoConfig per-operation failures, ownership and readiness*).

`documentation/openapi.audience` must now be a list of strings wherever it's
set: inside `extraConfig` on `spec.overrides[]`, `spec.defaults.endpoint`, or
`spec.additionalEndpoints[]` (AutoConfig), or declared directly on an OpenAPI
operation. `null` (e.g. an `audience:` key with no value in YAML) and `null`
items are rejected too.
The `KrakenDAutoConfig` admission webhook now rejects a non-list `extraConfig`
value at `kubectl apply` time (`must be a list of strings, e.g. ["internal"]`);
a value declared on the operation itself is caught by the default CUE
definitions instead: that operation is held with reason `CUEEvaluationFailed`
in `status.failedOperations` and `Synced` is `False` with reason
`OperationsFailed`.
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
first. That precheck holds the operations that fail and writes the rest (see
*Pre-validation* under *AutoConfig per-operation failures, ownership and
readiness*). The operator's writes of generated endpoints
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
seconds. This is the gateway controller's check. Admission runs the same
checker without `-t` (`krakend check -n`) and relies on the route check for
what `-t` catches (see *Complete admission*).

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
- The operator remembers, in memory and per gateway, each verdict by the
  content it judged (the render's checksum, the edition and the check), and
  runs no check again for content it already judged. It checks again when the
  content changes, including a switch to or from CE fallback, and once after an
  operator restart or leader failover. See "Unreleased — Per-object
  validation".
- The Warning event (`GatewayRootInvalid` or `CombinedConfigInvalid`) fires
  when the verdict or its message changes, not on every reconcile.
- `krakend_operator_config_validation_failures_total` counts a rejection once
  per change of what the gateway controller checks, not
  once per reconcile, and, since per-object validation, only the rejections of a gateway root, a policy or an
  endpoint checked on its own (see "Unreleased — Per-object validation"). An alert on its rate, such as the runbook's former
  `KrakenDConfigValidationFailures`
  (`rate(krakend_operator_config_validation_failures_total[5m]) > 0` for 10 minutes),
  therefore no longer fires for a single lasting rejection. The runbook alerts per gateway on
  `krakend_operator_gateway_config_valid` (`KrakenDGatewayConfigRejected`) and
  on `krakend_operator_gateway_excluded_endpoints`
  (`KrakenDGatewayEndpointsExcluded`). To list the gateways that are rejected
  right now, use the `ConfigValid` query under "Config validation runs
  offline" above.

Tooling that waits for `Rendering` or `Validating` should wait on the
`ConfigValid` condition instead.

### An unavailable validator is retried, not reported as a broken config

Only a completed krakend check run that rejects the config marks it invalid
(`ConfigValid=False`, reason `GatewayRootInvalid` or `CombinedConfigInvalid`,
phase `Error`; or, for one endpoint, `Accepted=False` with reason
`EndpointInvalid` or `PolicyInvalid`). When
the check cannot run to completion (the binary is missing, the 30-second
limit is hit, the process is killed, the temp directory is unwritable or
full, or the validation copy or the route check cannot complete), the gateway
now reports
`ConfigValid=Unknown` with reason `ValidatorUnavailable`, emits one
`ValidatorUnavailable` Warning event, keeps its phase and its applied
config, and retries with exponential backoff. Previously such failures were
reported as an invalid config (and re-run in a status-write loop).

`krakend_operator_config_validation_failures_total` counts only a fresh
rejection of a gateway root, a policy or an endpoint checked on its own (the EE
wildcard rules or the route check applied before it, or
`krakend check`). Failures to prepare the validation copy, other errors
that are not verdicts such as an unavailable validator, and the gateway's full check of its whole render do not increment it.

### Validation messages are capped at 4 KiB

The `ConfigValid` condition message and its Warning event (`GatewayRootInvalid`
or `CombinedConfigInvalid`) now carry at most 4 KiB: a summary line, then the
root's output line by line, as many whole lines as fit, followed by
`(output truncated, N more lines)`. An endpoint's `Accepted` message is cut to
the same 4 KiB. A gateway root that fails is quoted in `ConfigValid` and its
event, and an endpoint that fails in its `Accepted` message; the gateway
controller logs krakend output only for a failure that needs several
endpoints together, cut at 16 KiB, and the AutoConfig controller logs a held
operation's full error at Info. Previously an
output over the CRD's 32768-character limit (for example one bad key in a
policy used by many backends) made the status write fail, so the rejection
was never recorded.

### Terminating gateways are left alone; deleted gateways stop reporting metrics

A `KrakenDGateway` with a `deletionTimestamp` (for example during foreground
deletion) is no longer reconciled, so the operator no longer recreates the
children garbage collection is removing. When a gateway is deleted or starts
terminating, its `krakend_operator_endpoints`,
`krakend_operator_gateway_info`, `krakend_operator_gateway_config_valid`,
`krakend_operator_dragonfly_ready` and
`krakend_operator_license_expiry_seconds` series are removed, so alerts on a
deleted gateway stop firing. Its
`krakend_operator_reconcile_duration_seconds{controller="gateway"}` series
stays until the operator restarts; see the OpenTelemetry section below.

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
  - `False`, reason `EndpointInvalid`: the endpoint fails krakend check on
    its own and is left out of the configuration;
  - `False`, reason `PolicyInvalid`: a policy it references fails on its own,
    or it fails only together with a policy of another namespace.

    Both are described under "Unreleased — Per-object validation".
  - reason `EEFeaturesStripped`: a CE fallback render removed Enterprise-only
    features from it. `True` while some of its entries are still served,
    `False` when all of them were wildcards.
  - `True`, reason `SchemaNameConflict`: it is served, but defines a component
    schema differently from the endpoint the gateway's documentation takes
    that name from.
  - removed: a policy it references is missing, so it is not in the
    configuration (the endpoint controller reports why).
- `Accepted` is written only for a configuration that passed validation, or
  is unchanged since it did, and records the endpoint generation that
  configuration contains. While nothing of a gateway's newest render
  can be applied (its root fails on its own, or its endpoints fail only
  together), no verdict changes, except that on a combined failure an endpoint
  that fails on its own gets Accepted=False with reason EndpointInvalid or
  PolicyInvalid, worded as not served once the gateway next applies its
  config, and an exclusion that no longer holds is lifted. On a root failure no
  endpoint is judged.
- The `EndpointConflict` Warning event fires once per transition instead of
  on every gateway reconcile. A `Normal` `Accepted` event marks a conflict
  that cleared. A `PartiallyAccepted` endpoint gets its own Warning
  (`PartiallyAccepted`) when it becomes partly conflicted. The
  former `EndpointInvalid` event for a missing policy is gone; the reason now
  marks an endpoint that fails krakend check on its own, with a Warning event
  on the change.

### KrakenDEndpoint: `ResolvedRefs`, `Ready` and `phase` (endpoint controller)

| Condition | Written by | True when | Reasons |
|---|---|---|---|
| `ResolvedRefs` | endpoint controller | the gateway and every referenced policy exist | `RefsResolved`, `GatewayNotFound`, `PolicyNotFound` |
| `Accepted` | gateway controller | the endpoint is in the gateway's validated configuration | `Accepted`, `PartiallyAccepted`, `EndpointConflict`, `EndpointInvalid`, `PolicyInvalid`, `EEFeaturesStripped`, `SchemaNameConflict` |
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

- New `Ready` condition: `True` when `SpecAvailable`, `Synced` and
  `EndpointsReady` are all `True`, otherwise `False` with the first failing
  condition's reason (e.g. `SpecFetchFailed`, `UnmatchedOverride`,
  `OperationsFailed`, `EndpointsNotReady`). `Ready` is absent until the first
  status write. A first sync that creates endpoints reports `Ready=False`
  with reason `EndpointsNotReady` until the endpoint controller reports them.
- New `status.observedGeneration`, set on every status write.
- `phase` is derived from `Synced` (`Synced`, `Error`) and is empty before
  the first sync. A new AutoConfig no longer gets a separate `Pending`
  status write and an extra reconcile.
- `lastSyncTime` is documented on the field: it is the last sync that
  changed something, not a heartbeat.
- An AutoConfig whose reconcile fails with an error, which is every
  `OnChange` failure except `OperationsFailed`, and `EndpointReconcileFailed`
  or `ValidatorUnavailable` for either trigger, now
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

Since per-object validation, an endpoint that fails on its own is excluded and
the rest of the gateway applied; see "Unreleased — Per-object validation".

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
  owns sits at `<gateway>-config-<hash>`, or a copy with another payload
  cannot be replaced). `Ready` and `ConfigValid` now read `Unknown` with
  reason `ConfigPublishFailed` in this case, where `Ready` used to stay
  `True` behind the lagging `observedGeneration`.

In both cases the error is logged and the gateway is requeued, so kstatus and
Flux report the gateway as in progress, not as current, until the error
clears; then `observedGeneration` catches up. A stuck `observedGeneration`
behind `metadata.generation`, together with an error in the operator log,
means a child resource or the applied config's ConfigMap cannot be
reconciled. A rejected config (`ConfigValid=False`), an unavailable validator,
a new render that passed validation but cannot be published
(`ConfigPublishFailed`, below) and a missing plugin ConfigMap do not hold it
back: they are verdicts on the current generation, and `Ready` reports them.

### A validated config that cannot be published is reported

A render can pass `krakend check` and still not be applied, because its
content-addressed ConfigMap cannot be published:

- a ConfigMap the gateway does not control, or one whose checksum annotation
  differs, sits at `<gateway>-config-<hash>`;
- a `count/configmaps` ResourceQuota is exhausted;
- the rendered config exceeds the 1 MiB ConfigMap limit;
- a ConfigMap at that name holds another payload and cannot be deleted (see
  *Config ConfigMaps are immutable and content-addressed*).

The gateway used to keep `Ready=True` and `ConfigValid=True`/`ConfigApplied`
for the previous config, with `observedGeneration` current, while only the
operator log and the retry backoff showed that the newest render was not
applied. It now sets `ConfigValid=Unknown` with reason `ConfigPublishFailed`.
The message carries the cause, bounded to 4 KiB, for example `the newest
config passed validation but its ConfigMap could not be published, retrying:
configmap default/gw-config-0123456789 exists but is not controlled by
gateway gw`. `Ready` follows as `Unknown` with the same reason and the
serving phase, whatever drove the render: a gateway spec edit, a
KrakenDEndpoint, an AutoConfig or a policy change. The previous config keeps
serving, a Warning event `ConfigPublishFailed` fires when the gateway enters
the state, and the reconcile is retried with backoff. `observedGeneration` is
not held back, as for `ValidatorUnavailable`. Alerts on `Ready=True` or on a
Healthy kstatus now see the gateway as in progress until the ConfigMap can be
published.

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
  still has or wants pods;
- the revision the Deployment's pod template mounts, which matters while the
  Deployment is held (a missing plugin ConfigMap, or a ServiceAccount another
  controller owns): collection still runs then, so a long hold does not pile
  up revisions.

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

**The payload is verified, not only the metadata.** An existing ConfigMap at
the name is accepted only when the gateway controls it, it carries the
checksum annotation, and its `krakend.json` hashes (SHA-256) to the checksum.
The owner reference and the annotation can be copied by anyone who can create
ConfigMaps in the namespace; the payload cannot. The pods load the payload
whatever the metadata says, so the payload of any ConfigMap at the gateway's
content address is hashed, whoever owns it and whatever its annotation holds. A
ConfigMap that fails the hash is deleted, with a UID precondition, and created
again from the render, once. The operator emits a Warning event
`ConfigMapTampered` on the gateway that names the deleted ConfigMap. If it
cannot be deleted, `ConfigValid` reads `Unknown`/`ConfigPublishFailed`, never
`ConfigApplied`. A ConfigMap that holds the right bytes but is not the gateway's
copy (no owner reference, or another annotation) is not deleted: it is
reported as `ConfigPublishFailed`, and the Deployment is held as it is,
mounting the right bytes. When the applied config's ConfigMap fails the hash on
a pass that has no applied render to create it from (the newest render is
rejected or could not be validated), it is deleted and the Deployment is held
as it is until a render that passes is published (see *Gateway Deployment not
updated* in the runbook).

The payload is read in full once per stored version of a ConfigMap (its UID
and resource version). The operator remembers the versions it has hashed, and
the ones it created itself, so a steady reconcile still reads only the
ConfigMap's metadata. The memory is per gateway, bounded, and lost on restart,
after which each ConfigMap is read once more. The cost is one full read of the
rendered config (up to 1 MiB) per published revision and per operator
restart, plus one when someone edits or recreates a config ConfigMap.

**RBAC:** the operator's ClusterRole gains `list` on `apps/replicasets`. The
Helm chart ships it. If you maintain your own copy of the role, add it.

### An endpoint that fails on its own says so on its own status

krakend check output is no longer mapped back to endpoints. Each endpoint is
judged on its own, and one that fails gets Accepted=False with reason
EndpointInvalid (quoting its own output) or PolicyInvalid (naming the policy),
while the rest of the gateway is applied. ConfigValid quotes the root's own
output for GatewayRootInvalid, and nothing for CombinedConfigInvalid. See
"Unreleased — Per-object validation".

### Partly conflicting endpoints report exactly what is not served

When two KrakenDEndpoints on a gateway declare the same path and method, the
older one's entry is served. The newer one's other entries are still
served. The newer KrakenDEndpoint now reports:

- `Accepted=True` with reason `PartiallyAccepted` when some of its entries
  are served. It is `Accepted=False/EndpointConflict` only when none are.
- `status.conflicts`, a list of `{endpoint, method, winner}`, one item per
  entry that is not served, once for each KrakenDEndpoint it loses to (the
  winner, whose entry may itself be left out).

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
  `GET /v1/users`. The EE router refuses to start in that case. An EE wildcard
  that overlaps another endpoint's route of the same method is resolved
  oldest-first (the newer entry reports EndpointConflict or PartiallyAccepted);
  one that breaks the wildcard rule within its own endpoint gets
  EndpointInvalid. A wildcard endpoint with more
  than one backend is rejected on its own, because EE allows only one.
  Paths are compared as the router registers them: a brace group the
  router does not read as a parameter (`/v1/jobs{x}/*` next to
  `/v1/jobs:x/y`, or `{user.id}` next to `:user.id`) is literal text, so it
  no longer conflicts with the colon spelling, while `/v1/{id}/*` next to
  `/v1/{id}/x` still does.
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
  startup without affecting health. Setting or changing these is rejected
  (see *Complete admission*); a value stored before that rule keeps being
  accepted on unrelated updates, and the gateway webhook warns about it.
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

The watch is metadata only: the operator caches each object's name, labels
and owner references, not its spec, so memory does not grow with the size of
the Dragonfly, ExternalSecret and VirtualService objects in the cluster.

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

**How the operator finds what to delete.** For a Dragonfly, ExternalSecret or
VirtualService CRD that was installed when the operator started, the lookup
reads the informer cache, with no API request. A child the cache does not hold
yet cannot be orphaned: its creation event reaches the gateway's watch and
that pass deletes it. For any other optional kind the lookup asks API
discovery whether the CRD exists. An answer of "not installed" is remembered
for one minute, for the delete path only: while the CRD is absent no child can
exist. Enabling a feature always asks discovery again, and a CRD it finds
installed clears the memory. A CRD installed after the operator started, with
its feature off, costs one live GET per reconcile until the operator restarts,
which the startup log already asks for.

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
  for 15 minutes). It stays 1 while endpoints are excluded;
  krakend_operator_gateway_excluded_endpoints counts those.
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

**Health-path and autoOptions clashes no longer reach a pod.** An endpoint on
the gateway's custom health_path is that endpoint's own failure
(EndpointInvalid) and is excluded; endpoints whose routes clash only while
router.auto_options is on are resolved oldest-first. Before this release the
same render was published and new pods panicked at startup. The clashes
are an endpoint on the gateway's custom `health_path` with a GET method, and
endpoints under different methods whose routes would clash in one method's
tree while `router.auto_options` is on, because `auto_options` joins every
method's paths in one tree (for example `GET /users/{id}` with
`POST /users/{userId}/orders`). The route check and the EE wildcard rule each
report at most 21 conflicts and end with a notice line, so a config with many
clashing routes cannot exhaust the operator's memory in the validator. The
render resolves at most 21 clashes between endpoints; past that, admission and
the AutoConfig controller refuse every write that could add one (see "Router
clashes resolve oldest-first").

**The validator binary is pinned by digest.** The operator image takes its
`krakend` binary from KrakenD CE 2.13.11, referenced by digest in the
`Dockerfile`, instead of the floating `krakend:2.13` tag. Admission and the
gateway controller validate every gateway with that binary, whatever its
`spec.version`, and the validator changes only when the pin does. If you build
the operator image yourself, the build argument is now `KRAKEND_IMAGE`; the
former `KRAKEND_VERSION` is gone, and passing `--build-arg KRAKEND_VERSION=...`
no longer has an effect: the build uses the pinned image.

**Rejection messages are the object's own.** An endpoint's EndpointInvalid
message quotes the krakend check output of the gateway root with that endpoint
alone; ConfigValid quotes the root's own output for GatewayRootInvalid and
nothing for CombinedConfigInvalid. The gateway controller gathers, renders and
validates through the same checker the admission webhooks use.

**Admission denials are `422 Invalid` with one cause per rejected field.**
`kubectl` prints `The KrakenDEndpoint "x" is invalid: spec.endpoints[1]: ...`
instead of `403 Forbidden` with a single message. Scripts that matched
`Forbidden` must match `is invalid`. A failed lookup or an unavailable
validator is `500 Internal Error`, a transient server error: retry the
request; controllers and GitOps tools retry on their own. Each webhook call is
now limited to 15 s (`timeoutSeconds`; it was the 10 s default).

**Endpoint writes are checked on their own.** Creating or changing a
KrakenDEndpoint first checks the gateway root alone and each policy the
endpoint references alone; their verdicts are remembered across requests, the
256 most recent per operator pod. Then it checks the endpoint with the root and
the policies it references. A denial quotes only the output of the endpoint's
own check; nothing about another endpoint is quoted. That check renders the
gateway root with the endpoint, so output from a failure that occurs only with
a root setting can reveal that setting to the endpoint's author. When the
gateway root fails on its own, the write is admitted with a
warning that names the gateway and asks its owner to fix it. An update whose
stored version also fails on its own is admitted with a warning, and the
endpoint stays excluded until it passes. A referenced policy that fails on its
own is named, never quoted. The entry rules run
first, and a write they reject is not rendered. That includes the
`documentation/openapi.audience` rule, which keeps rejecting a malformed
audience on a changed entry on every gateway, because a CE render drops an
entry's `documentation/openapi` and the render check would never see it. While
an Enterprise gateway is in CE fallback (its `LicenseDegraded` condition is
true), admission judges the fallback render, which drops the Enterprise-only
content, so such content in a write is first judged when the license returns.
The checks run in the operator pod, three at a time for the whole pod, sharing those
slots with the gateway controller and the AutoConfig controller (each holds at
most one, so together they never hold more than 2 of the 3 slots), and each webhook call stops
its work after 12 s. A request that cannot get a slot in time, or whose check cannot run, is
answered `500 Internal Error`: a transient error that `kubectl` does not retry,
so run the command again (controllers and GitOps tools retry on their own).
A policy write runs 1 check, plus 2 for each gateway that uses it (that
gateway's root, then the endpoints that use it together), plus one per endpoint
it then judges on its own (every such endpoint when they fail together,
otherwise those that lost an entry in that check), plus one more for each of
those that fails.
With the webhooks disabled the controller's checks are the only protection: an
endpoint that fails on its own is excluded, and a root that fails keeps the
gateway at its last applied config.

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
such objects. These rules left the webhook for the CRDs (duplicate entries,
policy minimums, and the license, OpenAPI port, PVC and script rules), and Helm
does not upgrade CRDs, so with the old CRDs still installed nothing enforces
them: run the audit, then apply the new CRDs as described in [CRDs: apply them
before upgrading the operator](#crds-apply-them-before-upgrading-the-operator),
then upgrade the operator. The chart refuses clusters below
1.33 (`kubeVersion`), and the OLM bundle's `minKubeVersion` is 1.33.0. With
Helm, use 3.18 or later: older releases default `helm template` and `helm lint`
to Kubernetes capabilities below 1.33 and refuse the chart unless given
`--kube-version`.

**KrakenDEndpoint schema.** `spec.endpoints` needs one to 1024 entries and each
(endpoint, method) pair at most once; every entry needs a backend; `endpoint`
must start with `/` and contain no `*`, `?`, `&`, `%`, whitespace or control
character except a trailing `/*`; each backend `host` is non-empty and holds
no whitespace or control character; `timeout` and `cacheTTL` must be Go durations (`30s`, `1m30s`); `outputEncoding`,
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

**Backend hosts and endpoint paths hold no whitespace or control
characters.** A backend `host` (on a KrakenDEndpoint, or in the `backends` of
an AutoConfig additional endpoint), and the `endpoint` path of an entry, an
AutoConfig override or an AutoConfig additional endpoint, may no
longer contain a space, tab, newline or other control character (ASCII 0x00 to
0x20 and 0x7F); a host may not be empty. `krakend check` prints both
verbatim in its errors, and the operator attributes those errors to endpoints
by the route they name, so text of that kind let one tenant's invalid value
make its error read as another tenant's endpoint. No valid URL host or path
contains such a character. Stored objects ratchet like every other field rule:
an entry the update leaves unchanged keeps validating, and an edit to the
backends of an entry re-checks every host of that entry. The audit in
step 5 of the Pre-Upgrade Checklist lists existing objects that hold one, so
fix them before the CRDs are applied.

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
with `/`, no `*`, `?`, `&`, `%`, whitespace or control character except a
trailing `/*`; a backend `host` follows the same rule as on a KrakenDEndpoint), and
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
API-key authentication was served without it. The same goes for a backend's
`policyRef` on a CE gateway: it is rejected, at
`spec.endpoints[i].backends[j].policyRef`, when the policy's `raw` carries such
a namespace or key, because the policy's own check ran only when its `raw`
changed. Only a reference the stored object did not already hold is judged (on a
create, or a move to another gateway, every reference is), and the operator's
own writes to AutoConfig endpoints, which inherit `defaults.policyRef`, are
judged too. An entry's `documentation/openapi`
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
Endpoints that already reference the gateway and fail with that root do not
block the create: a warning names the ones that fail with it. On a CE gateway, though, the create is
refused while those endpoints, or the policies they reference, use
Enterprise-only namespaces or `/prefix/*` wildcards (below).
An update that renders the same config as the stored object (a new
`spec.image`, `spec.version`, replica count, resource, probe or
`postRestartJob`) is not checked, so it is never refused with a `500` because
the validator is unavailable. Any other update must keep its root passing on its
own (an update whose stored root fails too only warns) and must not make an
endpoint it serves fail that passed with the stored root: the denial names those
endpoints, never quoting them. A stored root that fails on its own makes every
endpoint fail with it, which says nothing about the endpoints, so it does not
turn a break into a warning for an endpoint the gateway's last applied config
served: failing with the new root is the update's doing, and the update is
refused, naming it. The same goes for a failure that appears only with the
endpoints together: the stored group is not compared, so it is denied. An
endpoint that config did not serve only draws a warning: one never judged, one
changed since it was judged, or one that lost every entry (`EndpointConflict`,
or every entry stripped, `Accepted` False). An update that would make two endpoints' routes
clash in KrakenD's router, such as turning router.auto_options on, is refused,
naming both. A `spec.version` other than 2.13.x gets a warning when it is set
or changed: validation uses the pinned 2.13 binary. **On a CE gateway,
Enterprise-only namespaces are rejected** in `spec.config.extraConfig` when it
is set or changed, and creating a CE gateway, or switching `edition: EE` to
`CE`, is rejected while the gateway's KrakenDEndpoints or their KrakenDBackendPolicies use one, or any of
its endpoints is a `/prefix/*` wildcard, even when the gateway's config already
fails; the denial lists each object, field and namespace. So are the typed Enterprise fields
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

**A post-restart Job that borrows rights or relaxes its security context needs
`create pods`.** The operator creates the `spec.postRestartJob` Job with its own
permissions, so the Job's ServiceAccount, Secret references and security
contexts used to be honored whoever wrote the gateway. The gateway webhook now
answers `403 Forbidden` (`spec.postRestartJob: <user> may not create pods in
namespace <ns>, so the post-restart Job may not run as another ServiceAccount,
read a Secret, or relax the operator's default security context`) when an
enabled `spec.postRestartJob` does any of these, and the requesting user (the
identity the API server authenticated, not the operator) may not create pods in
the gateway's namespace:

- sets a `serviceAccountName` other than the gateway's own name;
- takes `envFrom` from a Secret, or has an `env` `secretKeyRef`;
- sets a `securityContext` or `podSecurityContext` field outside an allow-list
  of settings that grant no privilege, or a
  `container.apparmor.security.beta.kubernetes.io/*` pod annotation other than
  `runtime/default`.

The allow-list is the run-as identity (`runAsUser`, `runAsGroup`,
`runAsNonRoot`), `fsGroup`, `fsGroupChangePolicy`, `supplementalGroups` and
`supplementalGroupsPolicy`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation:
false`, `privileged: false`, `procMount: Default`, a `capabilities` block with
no `add` and a `drop` that keeps `ALL`, and the `RuntimeDefault` seccomp and
AppArmor profiles. Anything else is reviewed, including a field a later
Kubernetes release adds. In particular a custom `drop` without `ALL` replaces
the default drop of `ALL`, so it counts, and `Localhost` profiles and
`seLinuxOptions` are reviewed. Running as root needs no review: it stays an
acknowledged choice (`runAsNonRoot: false`). `podLabels` and `podAnnotations`
other than the AppArmor annotation are not reviewed.

The webhook asks the API server with a SubjectAccessReview, which needs the new
`create` permission on `authorization.k8s.io/subjectaccessreviews` in the
operator's ClusterRole (see *Operator RBAC, caching and availability*). A
post-restart Job on the gateway's own ServiceAccount, with no Secret reference
and only allow-listed security settings, needs no review. The review runs when
the `postRestartJob` is new or changed, so scaling or relabeling a gateway that
already has such a Job is not blocked, and a stored gateway is affected only
once its `postRestartJob` is edited. Before upgrading, check who writes
KrakenDGateways that use these fields. A GitOps controller's ServiceAccount, for
example, needs `create` on `pods` in the gateway's namespace (for instance
through the `edit` ClusterRole), or its sync of that gateway is refused.

**Policy writes are checked alone and in every gateway that uses them.** A
KrakenDBackendPolicy whose `raw` (or typed fields) KrakenD rejects is refused on
its own, before anything references it, unless the stored policy already failed
on its own too (then its gateways decide). A change to a policy that endpoints reference is checked in
each gateway that uses it: first the gateway's root alone, then the root with
the endpoints that use the policy there and are not recorded as excluded, with
every gateway checked before any endpoint is named. When those endpoints fail
together, each is checked on its own with the new policy; when they pass, only
the endpoints that lost an entry in that check. An endpoint that fails with the
new policy is checked with the stored one. If it passes there, the change
breaks it: the write is refused (`breaks gateway ns/name: ...`), naming the
endpoint (`<ns>/<name>`) and never quoting it, since the policy's writer may
not read it. One endpoint that already fails does not hide another that the
change breaks. When every endpoint that fails already failed with the stored
policy, or the gateway's root fails on its own, the write gets a warning
instead. On a create the endpoints are checked against a policy of the same
name that holds nothing, so an endpoint that fails whatever the policy holds
only draws the warning. When the endpoints fail only together (each passes on
its own), the stored policy decides: if they passed together with it, the
change causes the failure and is refused, naming the gateway and quoting
nothing; if they already failed together, the write gets a warning (a create
is refused). The denial lists at most 20 gateways and the warnings
name at most 5, each counting the rest, in at most 4 KiB together (past that
the API server would cut every warning to 256 characters).
A new or changed `raw` with Enterprise-only namespaces, for example `auth/gcp`, or
Enterprise-only keys such as `proxy_address` in `backend/http/client`, is
refused while a CE gateway uses the policy (`gateway ns/name runs CE, which
ignores it silently`); a policy that only sets keys CE honors, such as
`send_body_on_redirect`, is admitted. A stored policy that already does this
keeps accepting unrelated edits. The audit lists the stored ones.

**Updates are ratcheted.** The webhooks never validate a
metadata-only update (the KrakenDAutoConfig name rule, which is a CRD rule, still
runs on label, annotation and status writes). A
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
referencing it or is deleted, and then it disappears. A reference to a
terminating policy that the endpoint did not already hold is rejected
(`policy is being deleted`); an endpoint that already references it keeps being
accepted, because a stored reference is not checked again. The policy webhook
is no longer registered for DELETE, in the Helm chart and in
`operator/config/webhook/manifests.yaml`, so policy and namespace deletion are
accepted without the webhook; they complete once the operator removes the
finalizer. If the operator is down, a policy deletion waits until it is back.
A policy is unprotected until the operator's first reconcile adds the finalizer, which includes the upgrade rollout: the new webhook configuration drops DELETE before the new leader has added finalizers. To roll back or uninstall, remove the finalizer (see [Rollback](#rollback)).

**AutoConfig overrides.** Two `spec.overrides` for the same `operationId`, or
for operationIds that differ only in case or in `_`, `-` and other punctuation
(`get_a` and `get-a`), are rejected: the generator names each endpoint from the
AutoConfig name and the sanitized operationId, so both operations map to one
`KrakenDEndpoint` name and it keeps only the first. Two very long operationIds
that agree up to the 253-character name limit collide the same way. The denial
names the second entry (`spec.overrides[1].operationId`). A stored list that
already does this keeps accepting edits that leave the colliding entries and
their positions unchanged; inserting or removing an override above the pair
shifts it to a new position and is rejected. A stored malformed
`documentation/openapi.audience` is ratcheted the same way, by its position:
inserting or removing an override or an additional endpoint above it gives it a
new position and it is rejected until it is fixed. The audit in the Pre-Upgrade
Checklist lists these lists. A `policyRef` in `defaults`, `overrides` or
`additionalEndpoints` that names no existing policy now produces an admission
warning (at most five, then a count), not a rejection: a release may create the
policy after the AutoConfig, and the generated endpoints are rejected until it
exists (the AutoConfig holds each as `EndpointRejected` in
`status.failedOperations` and reports `OperationsFailed`).

---

## Unreleased — AutoConfig per-operation failures, ownership and readiness

A problem in one OpenAPI operation now stays with that operation. The
AutoConfig's status shows what is held, what was skipped, and whether its
endpoints are serving.

**Behavior changes:**

- **Per-operation failures are held, not fatal.** An operation is held when:
  - its entry fails CUE evaluation (reason `CUEEvaluationFailed`), for
    example a `documentation/openapi.audience` that is not a list of
    strings;
  - its endpoint fails krakend check on its own, or loses a route to an
    older endpoint (the same method and route shape, or a router clash; reason
    `ConfigValidationFailed`);
  - the API server rejects its endpoint, or another object controls its
    name (reason `EndpointRejected`);
  - on a CE gateway, it uses an Enterprise-only `extra_config` namespace in
    an entry or a backend (`EndpointRejected` without a write; the generated
    `documentation/openapi` does not count).

  A held operation keeps its existing `KrakenDEndpoint` unchanged, every
  other operation is still created and updated, and no stale endpoint is
  deleted until every operation converges. An operation that `spec.filter`
  excludes is neither reported nor held.

  The AutoConfig reports `Synced=False` with reason `OperationsFailed`,
  naming up to five operations, and lists them in `status.failedOperations`
  (up to 20 entries, each message cut at 256 bytes). It emits an
  `OperationsFailed` Warning event when the `Synced` condition or
  `status.failedOperations` changes, not when only readiness or
  `status.readyEndpoints` does. The
  `krakend_operator_autoconfig_synced` gauge is `0` while any operation is
  held. These failures are deterministic, so they are not retried with
  backoff: the AutoConfig retries at its resync interval (5 minutes for
  `OnChange`, `spec.periodic.interval` for `Periodic`), or at once when an
  input or a watched dependency changes. Previously, one bad entry failed the
  whole AutoConfig (`CUEEvaluationFailed`), or an undecodable entry was
  dropped and its route deleted while the AutoConfig reported `Synced`.
  `status.failedOperations` can also name a label-matched endpoint the
  AutoConfig could not adopt (the API server answered 422). The list stays as
  the last sync that reached the endpoint writes recorded it when a later
  sync fails earlier, as `status.skipped` and `status.warnings` do. The full
  cause of every held operation, a CUE failure or a rejection, is in the
  operator log at Info, once per change of the causes for each operator
  process, so a restart logs them again; the status cuts each message at 256
  bytes. Count messages agree with their number (`1 operation failed`,
  `1 endpoint ready`).
- **Make-before-break writes.** Creates and updates run first, every one
  attempted. Stale endpoints are deleted only when every write succeeded and
  no operation is held, so a failure never takes a route off the gateway. An
  operation renamed upstream (a new operationId on the same route) converges
  this way: the new endpoint is written first and shares the route with the
  old one, which the webhook admits for endpoints of one AutoConfig, and the
  AutoConfig deletes the old one afterwards. A write error is reported for
  every failed endpoint (the status names five, then counts the rest), not
  just the first. A transient write error fails the sync as
  `EndpointReconcileFailed`, retried with backoff on both triggers. A
  `Conflict` or `AlreadyExists` still requeues quietly after one second.
- **HEAD, OPTIONS and TRACE operations are skipped and reported** in
  `status.skipped` (reason `UnsupportedMethod`), instead of failing every
  sync with `EndpointReconcileFailed`. An override that gives such an
  operation a supported `method` keeps it generated. When the URL transform
  puts two of them on one route, each skip carries its own `operationId` and
  tags, so `spec.filter` `excludeOperationIds` and `includeTags` judge each as
  itself. Duplicate operations
  appear there too (reason `DuplicateOperationId`: the same path and method,
  operationId or endpoint name as an earlier operation), with a
  `DuplicateOperationId` Warning event when the inputs change.
  `status.skippedOperations` counts both, including any beyond the 20 listed.
- **A `urlTransform` that collapses two operations onto one method and path
  is a misconfiguration.** The operator publishes one of them
  deterministically and reports the other as a duplicate in `status.skipped`.
  The published endpoint's name, its `failedOperations` label and filter
  matching can follow the other operation, and `spec.filter` cannot separate
  the two. Fix the transform or the upstream paths.
- **Same-shape routes are held.** Two generated endpoints whose paths differ
  only in parameter names or repeated slashes (`GET /h/{a}` and
  `GET /h/{b}`) share one route, and the gateway serves only one. The
  AutoConfig writes and keeps the one the renderer serves (the existing
  endpoint with the oldest `creationTimestamp`, then the lowest name; a
  new endpoint ranks after existing ones) and holds every other as
  `ConfigValidationFailed`, written or not. Stale endpoints take no part, so
  a rename still converges. The message is the one a KrakenDEndpoint author
  gets, advice included, and the status cuts it at 256 bytes. An AutoConfig
  user instead excludes one of the operations with `spec.filter`, or fixes
  the upstream paths to use one parameter name. An AutoConfig that already
  stores such a pair moves from `Synced=True` to `OperationsFailed` on
  upgrade, and its stale endpoints are kept until the pair is resolved (see
  the checklist audit below).
- **Ambiguous overrides fail closed.** An override whose `operationId` more
  than one operation declares fails the sync with reason `AmbiguousOverride`
  and a Warning event, and is not applied to any of them. An override
  `backends[].index` outside the operation's backends fails it with
  `UnmatchedOverride`, listed as `<operationId> backends[<i>]`. The
  unmatched message now reads `spec.overrides reference operationIds or
  backend indexes not present in the OpenAPI spec: …`. An override on an
  operation that failed CUE evaluation is held with it, not unmatched.
- **Override `operationId`s with `_`, `-` or a leading digit** now work with
  `extraConfig`. Previously CUE failed with `missing ',' in struct literal`.
  The keys of `_overrides` are the `SanitizeName` form of the operationId
  (lowercased, every character outside `a-z`, `0-9` and `-` replaced by `-`,
  leading and trailing `-` trimmed),
  which matters to custom CUE definitions that read it (see *AutoConfig
  fails sync on unmatched overrides* above).
- **Parameter `$ref`s** (`#/components/parameters/…`, and external ones for
  URL sources) are dereferenced, so forwarded query strings and headers are
  complete. Previously a parameter `$ref` failed CUE evaluation for the
  whole AutoConfig. A ref resolves only to a parameter object (one with a
  `name` and an `in`) under `#/components/`. An unresolvable one is listed in
  `status.warnings`, and the operation using it is held; an unresolvable
  **path-level** ref fails every operation on that path. A spec that
  dereferencing would grow past 10 MiB fails the sync (`SpecFetchFailed`).
- **Spec problems are persistent** in `status.warnings` (distinct, sorted,
  capped at 20 entries of 256 bytes), with a `SpecWarning` event when the
  inputs change (a sync emits at most 20 input warning events in all, `SpecWarning`,
  `DuplicateOperationId` and `AdditionalEndpointOverride` together, plus the
  failure or `OperationsFailed` event):
  - `$ref`s the resolver could not honour;
  - name collisions between inlined schemas;
  - `#/…` refs inside fetched documents, which resolve against the main spec;
  - external `$ref`s in a ConfigMap-sourced spec;
  - parameter `$ref`s that do not resolve;
  - schema references that `components/schemas` does not define;
  - additional-endpoint replacements (their event stays
    `AdditionalEndpointOverride`).

  One root cause can produce two notes, such as an external or local ref
  note and a schema-not-defined note. The warning event that reported
  entries the evaluator dropped is gone: a dropped entry is now a held
  operation.
- **Each generated endpoint carries only the component schemas its
  documentation references**, directly or transitively. Previously it
  carried the whole spec's map. On upgrade, each generated endpoint whose
  schema map shrinks is updated once, on its AutoConfig's first sync that
  reaches the writes, and the gateway's published documentation drops schemas
  no operation references. An Enterprise gateway whose documentation changes
  re-renders after each endpoint write and rolls, possibly more than once in
  quick succession.
- **Schema name collisions across endpoints are reported.** An endpoint whose
  component schema differs from the definition the gateway publishes under
  that name is still served: `Accepted` stays `True` with reason
  `SchemaNameConflict`, naming the schemas and the endpoint each is published
  from, and the endpoint's `Ready` stays `True` with that reason and message.
  The winning definition is the first in namespace/name order of the
  gateway's endpoints, not the oldest endpoint (routes are oldest-wins). It
  applies only to gateways that publish documentation (EE, not in CE
  fallback, with `spec.openapi.enabled`), and only when the endpoint would
  otherwise be plain `Accepted`. No event is emitted for it, except the usual
  recovery event when the endpoint was not accepted before.
- **Ownership by controller reference.** Generated endpoints are found by
  their controller owner reference, not their labels:
  - A generated endpoint whose labels were removed is repaired, or deleted
    if no longer desired.
  - A `KrakenDEndpoint` carrying both `gateway.krakend.io/autoconfig=<name>`
    and `gateway.krakend.io/auto-generated=true` with no controller is
    adopted by that AutoConfig, then converged or deleted. Anyone who can
    label an uncontrolled endpoint in the namespace can therefore have the
    AutoConfig converge or delete it, as with a ReplicaSet and its pods.
  - Uncontrolled endpoints are not watched, so adoption happens on the
    AutoConfig's next reconcile, not when the orphan appears.
  - An uncontrolled endpoint whose **name** a generated endpoint wants is
    taken over whatever its labels. That predates this change, so do not
    read unlabelled endpoints as safe.
  - An endpoint controlled by another object is never modified or deleted.
  - Managed labels are merged into existing labels.
  - Label changes on a generated endpoint trigger an immediate reconcile.
- **Readiness.** New `status.readyEndpoints` and condition `EndpointsReady`
  (reasons `AllEndpointsReady`, `EndpointsNotReady`, naming up to five
  endpoints) aggregate the generated endpoints' `Ready` conditions: a new
  or just-changed endpoint reads `Pending` until the endpoint controller
  reports it. The AutoConfig's `Ready` condition requires it, so an
  AutoConfig with a conflicted or detached endpoint reads `Ready=False`. A
  generated endpoint's readiness change triggers an immediate reconcile. A
  failed sync refreshes both from the endpoints the AutoConfig controls at
  that moment; if they cannot be listed, the last values stay.
- **Pre-validation.** Before writing, the controller checks the endpoints it
  is about to write against the gateway's config with the same checker the
  webhooks use. It writes only the operations that pass. Each endpoint it
  checks carries the creation time the cluster will give it (an existing
  endpoint keeps its own, a new one ranks after every existing one), so the
  check picks the same winner for a shared route that the gateway does. The
  stale endpoints it will delete count as gone, unless this pass will not
  delete them: an operation is held, or a write in this pass names an
  endpoint the last status recorded as `EndpointRejected`, which the API
  server rejects again. The candidates that would take part in a new router
  clash are held first. Then the gateway root is checked alone (when it fails,
  no candidate is to blame and all are written), then the root with every
  candidate together. When that fails, each candidate is checked on its own;
  when it passes, only the candidates that lost an entry in it. A hold quotes
  only that operation's own output (cut to fit the 256-byte status entry; the
  operator log keeps the full text; the check renders the gateway root with the
  endpoint, so output from a failure that occurs only with a root setting can
  reveal that setting), or names the policy at fault; no other
  endpoint's content can hold a candidate or reach its message. After a hold,
  the clash check runs again over the remaining candidates, with the stale
  endpoints and the held candidates' stored versions kept, until it holds no
  more. While the gateway has more router clashes than the render resolves,
  every candidate is held. Every krakend check holds a check slot (the clash
  check renders in process and holds none), and when it cannot run the sync
  fails as `ValidatorUnavailable`.
  When the checker is unavailable, nothing is written or deleted, and
  `Synced=False` has reason `ValidatorUnavailable` (retried with backoff);
  it may still adopt label-matched orphans, which changes owner references
  only. A write the API server rejects after a passing check keeps its stale
  endpoints. With several workers, two AutoConfigs can check the same gateway
  at once, each without seeing the other's pending writes. The gateway
  controller still never publishes a render that fails krakend check: it
  excludes the endpoints that fail on their own, and keeps its applied config
  with `ConfigValid=False` when its root fails or its endpoints fail only
  together. A rename and a brand-new rejection (the
  API server answering 422) in the same pass is not predicted when the
  rejection is new: the check models the stale endpoints as deleted, so a
  renamed sibling is written, the rejection stops the delete, and the gateway
  renders the old and the new endpoints together. The mixed render resolves
  oldest-first: the stale endpoint is older and keeps its routes, and the
  newer entry is left out. The whole render passes, so `ConfigValid` stays
  `True` and nothing is counted. The losing endpoint reads `EndpointConflict`
  or `PartiallyAccepted`, `status.conflicts` names the stale endpoint as
  winner, an `EndpointConflict` Warning fires, and the AutoConfig reads
  `EndpointsNotReady`. The moved route is not served until the cause of the 422
  is fixed: this is the accepted write-time remainder. When the last status
  already records the rejection (`EndpointRejected`), the precheck keeps the
  stale endpoints in its model and holds the sibling instead.
- **Concurrency, slots and deadline.** Up to 4 AutoConfigs reconcile at once.
  Configure this with `--autoconfig-max-concurrent-reconciles`, or chart
  value `autoconfig.maxConcurrentReconciles`. The AutoConfig checks hold at
  most 1 of the pod's 3 config-check slots, and the gateway controller at
  most 1, so the controllers never hold more than 2 of the 3 slots.
  Concurrent admission requests can take the rest. Fetching a spec and resolving
  its external `$ref`s is bounded by 2 minutes overall, and each request by
  30 seconds. A stuck upstream fails with `SpecFetchFailed` (`context
  deadline exceeded`) instead of holding a worker.

**Before upgrading — ownership audit.** List the generated endpoints whose
handling changes: label-matched orphans, which will be adopted, and
controlled endpoints whose labels no longer name their controller:

```bash
kubectl get krakendendpoints -A -o json | jq -r '
  .items[]
  | ([.metadata.ownerReferences[]? | select(.controller == true)] | first) as $ctl
  | select(
      ($ctl == null
        and .metadata.labels["gateway.krakend.io/auto-generated"] == "true"
        and .metadata.labels["gateway.krakend.io/autoconfig"] != null)
      or ($ctl.kind == "KrakenDAutoConfig"
        and .metadata.labels["gateway.krakend.io/autoconfig"] != $ctl.name))
  | "\(.metadata.namespace)/\(.metadata.name)\towner=\($ctl.name // "none")\tlabel=\(.metadata.labels["gateway.krakend.io/autoconfig"] // "none")"'
```

Empty output means the ownership change affects nothing. For each line,
the AutoConfig named by `label` (orphans) or `owner` (controlled) will
converge the endpoint, and delete it if its operation is no longer
generated. Remove the labels from any endpoint you want to keep outside
that AutoConfig.

**Before upgrading — overrides that will now fail closed.** List each
AutoConfig's override operationIds and backend indexes:

```bash
kubectl get krakendautoconfigs -A -o json | jq -r '.items[] | .metadata as $m | .spec.overrides[]? | "\($m.namespace)/\($m.name)\t\(.operationId)\t\([.backends[]?.index] | map(tostring) | join(","))"'
```

Then check each spec for duplicated operationIds. For a YAML spec, convert
it with `yq -o=json` first:

```bash
curl -s <spec-url> | jq -r '[.paths[][]? | objects | .operationId? // empty] | group_by(.) | map(select(length > 1) | .[0])[]'
```

An override on a listed operationId, or a backend index at or above the
operation's backend count (1 with the default CUE definitions), fails the
sync after the upgrade.

**Before upgrading — same-shape routes.** The admission audit in the
Pre-Upgrade Checklist prints a line `KrakenDAutoConfig ns/name: endpoints
share a route and the operator holds all but the one the gateway serves,
unless one of them is a rename in flight` for each AutoConfig that controls
two endpoints with one method and route shape. After the upgrade the
AutoConfig holds all but one of them as described above.

**AutoConfigs that recover.** An AutoConfig whose spec declares HEAD, OPTIONS
or TRACE operations was stuck in `EndpointReconcileFailed`, and one whose
whole evaluation failed (parameter `$ref`s, or override `extraConfig` on
operationIds with `_`, `-` or a leading digit) in `CUEEvaluationFailed`. It now
syncs and lists those operations in `status.skipped`. What else its first sync
does differs by case:

- An AutoConfig stuck in `EndpointReconcileFailed` kept no stale endpoints:
  the previous version deleted them before it wrote, so a failed write left
  them gone. On upgrade the endpoints after its first failed write are
  created, so those routes go live.
- An AutoConfig stuck in `CUEEvaluationFailed` for the causes above deletes
  the stale endpoints it kept while it was failing, unless an operation is
  held. One that failed on a single bad entry moves to `OperationsFailed`
  instead: its healthy operations are written, so the upstream drift it
  accumulated lands at once, and nothing is deleted while the entry is held.

List the AutoConfigs stuck that way:

```bash
kubectl get krakendautoconfigs -A -o json | jq -r '.items[] | select(any(.status.conditions[]?; .type == "Synced" and (.reason == "EndpointReconcileFailed" or .reason == "CUEEvaluationFailed"))) | "\(.metadata.namespace)/\(.metadata.name)\t\([.status.conditions[] | select(.type == "Synced") | .message][0])"'
```

A message naming a HEAD, OPTIONS or TRACE enum violation, a parameter `$ref`
or a CUE syntax error on an override means that AutoConfig recovers on
upgrade. Check which stale endpoints a `CUEEvaluationFailed` one holds before
upgrading; an `EndpointReconcileFailed` one holds none.

**Rollout.** Each generated `KrakenDEndpoint` whose schema map shrinks to the
closure its documentation references is updated once, on its AutoConfig's
first sync that reaches the writes, and each Enterprise gateway whose
published documentation changes re-renders after each write and rolls,
possibly more than once in quick succession. Those writes also run the new
check, which costs two `krakend check` runs per sync that writes (the gateway
root and the candidates together), plus one per candidate when those fail, or
per candidate that lost an entry when they pass, through the 1 slot the
AutoConfigs share, so expect a
short burst after the rollout. Every AutoConfig's status gains `skipped`,
`warnings`, `failedOperations` and `readyEndpoints` and the `EndpointsReady`
condition, and its `Ready` now also requires `EndpointsReady`, so health
checks keyed on `Ready` can read `False` for a conflicted or detached
endpoint. `SpecWarning` events appear, and the evaluator-warning events stop.

The CRDs gain the new optional status fields; apply them before the operator,
as for any CRD change (see *CRD Upgrades*).

---

## Unreleased — Operator RBAC, caching and availability

### The operator's ClusterRole is trimmed to the verbs it uses

The manager ClusterRole no longer grants `create`, `update`, `patch` or
`delete` on the four KrakenD kinds beyond what the controllers call (for
example it can no longer create or delete a `KrakenDGateway`), no longer
grants `update` on Jobs or on ConfigMaps (config ConfigMaps are immutable),
no longer grants `patch` except on events and the KrakenDEndpoint status
subresource, and drops the `finalizers` grants on `KrakenDEndpoint` and
`KrakenDBackendPolicy`, which own nothing. `delete` stays on
HorizontalPodAutoscalers, Dragonfly, ExternalSecret and VirtualService
objects (removed when their feature is disabled), on Jobs and on the
gateway's config ConfigMaps. It also stays on Deployments, Services,
ServiceAccounts and PodDisruptionBudgets, because clusters that enable the
`OwnerReferencesPermissionEnforcement` admission plugin require it when a
gateway adopts a same-named object. `list` on ReplicaSets lets config
ConfigMap garbage collection see which revisions running pods still mount.
The leader-election Role drops ConfigMaps and keeps `get`, `create` and
`update` on Leases. The role also gains one rule: `create` on
`authorization.k8s.io/subjectaccessreviews`, which the gateway webhook uses to
check that a user who sets a post-restart Job's ServiceAccount, Secret
references or a relaxed security context may create pods (see *Complete admission*). The chart used to grant
SubjectAccessReviews only with `metrics.enabled`, for the metrics endpoint; the
manager role now carries the grant on every install.

Upgrading applies the new rules with no other change: `helm upgrade` and
`make deploy` apply them from the chart and the kustomize manifests, and an
OLM upgrade applies the permissions in the new bundle's ClusterServiceVersion.

During `helm upgrade` from v0.14.0, the old leader keeps running under the new
role until the Lease moves to a new pod, about 30 to 60 seconds. In that time
it logs `forbidden: User …` for the verbs it no longer has (for example ConfigMap
`update`, endpoint status `update` and gateway `patch`). That is expected and stops once the new
leader takes over.

### The Helm chart's ClusterRole now tracks the operator exactly

`charts/krakend-operator/templates/clusterrole.yaml` renders its rules from
a copy of the generated role instead of a hand-maintained list, so a chart
release can no longer grant more, or less, than the operator binary needs.

### The OLM bundle matches the kustomize install

The bundle's ClusterServiceVersion is regenerated from source and checked by
`make verify-manifests`. It had drifted: an OLM install ran without the
admission webhooks, could not create post-restart Jobs (the `batch/jobs`
permission was missing) and capped the operator at 128Mi of memory. The
bundle now declares the four validating webhooks, grants the Job permission
and uses the same memory (512Mi limit, 128Mi request) and node affinity as the
kustomize install. Like kustomize, OLM keeps one replica, with no
PodDisruptionBudget and no anti-affinity: only the Helm chart gets the
availability defaults below. OLM provides and mounts the webhook certificates,
so the bundle carries no cert-manager dependency.

The Job permission means that, on OLM, the right to write a KrakenDGateway
reaches the operator's `batch/jobs` grant, which an OLM install did not have
before. The gateway webhook now reviews the requester's access for a
post-restart Job that runs as another ServiceAccount, reads a Secret, or
relaxes the operator's default security context (see *Complete admission*), so
a gateway writer cannot use the operator to borrow those rights. It does not
review `podLabels` or the `podAnnotations` other than the AppArmor one.

The gateway no longer takes over an object it does not control. Earlier versions
created or updated a ServiceAccount, Service, PodDisruptionBudget,
HorizontalPodAutoscaler, Deployment, Dragonfly, ExternalSecret or VirtualService
named like the gateway, and adopted one that already existed with no controller.
That let a user who may create KrakenDGateways in a namespace, but not pods, run
`spec.image` as any unowned ServiceAccount of that namespace by giving the
gateway its name, and redirect an unowned Service to the gateway's pods. The same
holds on every install method. Now the gateway writes an existing object only
when:

- it already controls it (its owner reference names this gateway), or
- it has no controller and carries the gateway's selector labels
  `app.kubernetes.io/instance: <gateway name>` and
  `app.kubernetes.io/managed-by: krakend-operator`.

Only someone who can write the object can set those labels, so the labels are
the hand-over. An object that `kubectl delete --cascade=orphan` left behind
still carries them, so a gateway recreated after an orphan delete takes its
children back. An object that another controller owns is never taken over, and
the labels do not change that.

Nothing is needed on an existing install: the objects earlier versions created
or adopted already carry the gateway's owner reference. What changes is a gateway
whose name matches an object that predates it and carries neither the owner
reference nor the labels: the object is left exactly as it is, and the gateway
reports it.

- `ResourcesControlled` is `False` with reason `ResourceNotControlled`, and its
  message names each refused object's kind and `<namespace>/<name>` and its
  controller if it has one, then the remedy: rename the gateway, or label the
  object to hand it over. `Ready` carries the same reason, with phase `Error`.
  The condition is `True` while nothing is refused.
- A ServiceAccount the gateway does not control holds the Deployment and the
  post-restart Job, whether another controller owns it or nothing does. The
  operator logs `holding the Deployment and the post-restart Job:
  serviceaccount <ns>/<name> is not controlled by gateway <name>`, and still
  reconciles the other children. A Deployment that already runs as that
  ServiceAccount is held as it is, and its pods keep running until the conflict
  is resolved.
- A refused Service, PodDisruptionBudget or HorizontalPodAutoscaler is left
  alone while the rest is reconciled. A refused Deployment is left alone and
  the post-restart Job waits for it.
- The refusal returns an error, like any other failed child, and the reconcile
  is retried with backoff. Nothing watches an object the gateway does not
  control, so after you label one, edit the gateway or restart the operator to
  retry at once.

To hand over an object you pre-created on purpose (for example a ServiceAccount
that carries a workload-identity annotation), label it:

```bash
kubectl label serviceaccount <name> -n <ns> \
  app.kubernetes.io/instance=<gateway name> \
  app.kubernetes.io/managed-by=krakend-operator
```

The gateway then takes it over on the next reconcile: it becomes the controller,
replaces the labels with its own, and keeps the annotations. Deleting the
gateway then deletes the object, as for any child it created.

Earlier versions may already have adopted a ServiceAccount you did not
intend. Adopted ServiceAccounts are the ones a KrakenDGateway controls although
they did not come from the gateway: they carry annotations the operator never
sets, such as `azure.workload.identity/client-id` or `eks.amazonaws.com/role-arn`:

```bash
kubectl get serviceaccounts -A -o json | jq -r '
  .items[]
  | select(any(.metadata.ownerReferences[]?; .kind == "KrakenDGateway" and .controller == true))
  | select((.metadata.annotations // {}) | keys | any(test("workload.identity|role-arn")))
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

Compare each hit with the RoleBindings and ClusterRoleBindings that name it as a
subject. To give one back, remove the gateway's owner reference and the
`app.kubernetes.io/instance` and `app.kubernetes.io/managed-by` labels from it,
and rename the gateway so it does not take the object again.

For OLM users: the bundle now supports only the `AllNamespaces` install mode.
The operator watches every namespace, and under `OwnNamespace` or
`SingleNamespace` OLM would scope the webhooks to the target namespaces only.
Writes to the four KrakenD kinds are now validated before they
are stored, and the webhooks use `failurePolicy: Fail`. While the operator is
unavailable, creating or updating those resources is rejected.

### Secrets and ConfigMaps are no longer cached

The operator used to cache every Secret and ConfigMap in the cluster, data
included. It now watches them as metadata only and reads the content it needs
directly from the API server, so no Secret `data` or ConfigMap payload is
cached, and operator memory no longer grows with their size. No configuration
change is needed, and the RBAC is unchanged: a metadata watch still needs `list` and
`watch`.

The metadata the operator does keep is stripped of its annotations and
`managedFields` before it is cached. That matters because an object applied
client-side (`kubectl apply`) repeats its whole body, data included, in the
`kubectl.kubernetes.io/last-applied-configuration` annotation. What remains
cached is the name, namespace, labels and owner references of every Secret and
ConfigMap in the cluster (one small entry per object): the watches cannot select
the user-named license Secrets and plugin ConfigMaps they follow.

Each reconcile pass now makes these live API requests:

- Gateway: the plugin ConfigMaps and the license Secret in full; the config
  ConfigMap's owner and checksum annotation, and the list used to collect old
  config ConfigMaps, as metadata only. A config ConfigMap's payload is read in
  full once per stored version, to hash it (see *Config ConfigMaps are
  immutable and content-addressed*). The children of disabled optional
  features come from the informer cache, or from API discovery with a
  one-minute memory of an absent CRD (see *Disabling a feature deletes what it
  created*).
- AutoConfig: the namespace's `krakend-cue-definitions` ConfigMap once, in full,
  falling back to the embedded definitions when it is absent, and the
  `cue.definitionsConfigMapRef` ConfigMap once, in full, when it is set (a
  missing one fails the sync with `CUEEvaluationFailed`, with no fallback); the
  spec and auth sources in full.

### Helm chart: metrics can be scraped

Chart installs served metrics that failed every scrape (HTTP 500): the
operator lacked permission to create the TokenReview and
SubjectAccessReview its authenticated metrics endpoint performs. The chart
now grants them when `metrics.enabled` is true, adds a
`<fullname>-metrics-reader` ClusterRole to bind to your scraper, labels the
metrics Service `app.kubernetes.io/component: metrics`, and can create a
`ServiceMonitor` (`metrics.serviceMonitor.enabled`, off by default) that
selects only that Service.

### The operator is ready only once its webhook server is serving

`/readyz` now includes a `webhook` check that passes once the pod's
admission webhook server accepts TLS connections. During rollouts and
restarts, admission requests are no longer sent to a pod whose webhook server
is not listening (with `failurePolicy: Fail` those requests used to be
rejected). Readiness does not cover cache sync: a request that arrives before
the caches have synced waits up to the 12 s budget, then fails closed with a
500.

### Every replica reloads the webhook and metrics certificates

With `--webhook-cert-path` or `--metrics-cert-path` set, the webhook and
metrics servers now start their own certificate watchers, which run on every
replica. Before, the operator added a watcher to the manager, which ran only
on the leader: with two replicas, a standby kept the certificate it read at
startup, and after a cert-manager renewal about half of the admission
requests failed with x509 errors under `failurePolicy: Fail`. The standby
still showed Ready, because the readiness check does not verify the
certificate. No configuration change is needed. If you are on an older
version with more than one replica, restart the operator pods after each
certificate renewal.

### Helm chart: two replicas, a PodDisruptionBudget and anti-affinity by default

`replicaCount` now defaults to 2, with a PodDisruptionBudget
(`maxUnavailable: 1`, `podDisruptionBudget.enabled`) and a soft preference
for different nodes when `affinity` is empty. A single operator pod made
every write to the four CRDs fail during its rollouts and node drains. The
chart now refuses to render `replicaCount` > 1 with
`leaderElection.enabled: false`. To keep one replica, set `replicaCount: 1`
(no PodDisruptionBudget is created then).

---

## Unreleased — Per-object validation

Every KrakenDGateway root, KrakenDEndpoint and KrakenDBackendPolicy is now
judged on its own, in admission and in the gateway and AutoConfig
controllers. A stored endpoint that fails on its own is left out of its
gateway while the rest of the gateway is applied.

### A stored invalid endpoint is excluded, not the whole gateway frozen

Previously one KrakenDEndpoint whose content failed `krakend check` kept its
whole gateway at the last applied config: no other endpoint's change was
applied until it was fixed. The gateway controller now checks a new render in
this order:

1. the gateway root on its own (the gateway rendered with no endpoint);
2. the whole render, with the full check (`krakend check -t -n`);
3. each endpoint on its own: the gateway root, the endpoint and the policies
   it references (`krakend check -n`). This runs for every endpoint when the
   whole render fails. When it passes, it runs only for the endpoints that
   lost an entry in that render (to an older endpoint's same route, or to a
   router clash, below): a passing render says nothing about the entries it
   left out;
4. the render without the endpoints that fail on their own, with the full
   check again (not run when that render is the config the gateway already
   applies).

An endpoint that fails on its own is **excluded**: none of its routes is
served, even if an earlier version of it was, and every other endpoint is
applied. This is a deliberate change. The endpoint says why on its own status;
the gateway and a metric say that endpoints are excluded. A gateway whose root
fails on its own still keeps its last applied config, as before. A validator
that cannot run excludes no endpoint and lifts no exclusion: the endpoints
keep the verdicts they have until a pass can judge them. The exception is a
gateway that has never applied a config: a pass that applies nothing removes
every `Accepted`, so its exclusions, the condition and the gauge clear.

An unchanged gateway runs no check: the operator remembers each verdict by the
content it judged, per gateway, until the gateway is deleted. The memory is
the operator's own, so it is empty after an operator restart or a leader
failover, and an edit of the gateway root or its edition changes every
verdict's content. After either, a gateway whose whole render fails (it
excludes endpoints) checks each of its endpoints once more (when the render is
the applied config, only the endpoints that lost an entry are judged first, and
the rest runs only if one of them fails), one at a time on
the gateway controller's single worker. On the pinned binary, at the chart's
default CPU limit of 500m, one endpoint's check takes about 0.1 s (0.05 s
unthrottled), so a gateway of 500 endpoints holds the gateway worker for about
50–60 s once, and other gateways' reconciles wait behind it. A gateway whose
whole render passes checks only its root, the whole render (about 1.1–1.2 s)
and the endpoints that lost an entry in it. The checks are not run in
parallel, because the operator keeps a validation slot free for admission, and
the verdicts are not stored in the cluster.

### Upgrading from v0.14.0: a one-time change in what is served

The first reconcile after upgrading from v0.14.0 can change what a gateway
serves, with no change to any object:

- A gateway that v0.14.0 holds at its last applied config
  (`ConfigValid=False`, reason `ConfigValidationFailed`) applies every pending
  change of its endpoints and policies at once and rolls its pods, unless its
  root fails on its own (`GatewayRootInvalid`) or its endpoints fail only
  together (`CombinedConfigInvalid`).
- An endpoint whose current spec fails on its own stops being served, even
  where v0.14.0 still served an earlier version of it.
- Where two endpoints' entries clash in KrakenD's router, the newer
  endpoint's entry is left out.
- An endpoint in phase `Conflicted` whose losing entry fails on its own is now
  excluded whole; v0.14.0 served its other entries.

Before upgrading, list the objects these can touch. The lists are a superset:
only the new operator's checks of each endpoint on its own decide which
endpoints are excluded.

```bash
# 1. Gateways held at their last applied config
kubectl get krakendgateways -A -o json | jq -r '
  .items[]
  | select(any(.status.conditions[]?; .type == "ConfigValid" and .status == "False"
      and .reason == "ConfigValidationFailed"))
  | "\(.metadata.namespace)/\(.metadata.name)"' > frozen-gateways.txt
cat frozen-gateways.txt

# 2. The endpoints of those gateways: each one that fails on its own is excluded
kubectl get krakendendpoints -A -o json | jq -r --rawfile frozen frozen-gateways.txt '
  ($frozen | split("\n") | map(select(. != ""))) as $gws
  | .items[]
  | ((.spec.gatewayRef.namespace // .metadata.namespace) + "/" + .spec.gatewayRef.name) as $gw
  | select($gw | IN($gws[]))
  | "\(.metadata.namespace)/\(.metadata.name)\t\($gw)"'

# 3. Endpoints that lose an entry today: excluded whole when that entry fails on its own
kubectl get krakendendpoints -A -o json | jq -r '
  .items[]
  | select(.status.phase == "Conflicted")
  | "\(.metadata.namespace)/\(.metadata.name)\t\(.spec.gatewayRef.namespace // .metadata.namespace)/\(.spec.gatewayRef.name)"'
```

The `ConfigValid` message of a gateway in the first list quotes the krakend
check output that held it, which usually points at the endpoint at fault. To
find which endpoints are excluded, run this after the upgrade:

```bash
kubectl get krakendendpoints -A -o json | jq -r '
  .items[]
  | select(any(.status.conditions[]?; .type == "Accepted" and .status == "False"
      and (.reason == "EndpointInvalid" or .reason == "PolicyInvalid")))
  | "\(.metadata.namespace)/\(.metadata.name)\t\([.status.conditions[] | select(.type == "Accepted") | .reason][0])"'
```

### New conditions and reasons

- KrakenDEndpoint `Accepted=False`, reason **`EndpointInvalid`**: the endpoint
  fails krakend check on its own. The message quotes its own output, at most
  4 KiB (the check renders the gateway root with the endpoint, so output from a
  failure that occurs only with a root setting can reveal that setting): `Not served by gateway <ns>/<gw>, which serves its other endpoints:
  this endpoint fails krakend check on its own: …`. While the gateway cannot
  apply its newest config (its endpoints fail only together, or its ConfigMap
  cannot be published), the message reads
  `Will not be served when gateway <ns>/<gw> next applies its config: this
  endpoint …` instead: the last applied config, which may hold an earlier
  version of the endpoint, keeps serving until then. `Ready` is False with the
  same reason and `phase` is `Invalid`. A Warning event marks the change.
- KrakenDEndpoint `Accepted=False`, reason **`PolicyInvalid`**: a policy it
  references fails krakend check on its own, or the endpoint fails only
  together with a policy of another namespace. The message names the policy
  and never quotes it. It begins like the `EndpointInvalid` message, in either
  of its two forms.
- KrakenDGateway **`EndpointsExcluded=True`**, reason
  `InvalidEndpointsExcluded`, while one or more endpoints are excluded:
  `<n> KrakenDEndpoint(s) fail validation and are not served: <ns>/<a>, …
  (+<m> more)` (the first 10), or, while the gateway cannot apply its newest
  config (its pass applied nothing new: `ConfigValid` is `False` or `Unknown`),
  `<n> KrakenDEndpoint(s) fail validation and will not be served when the
  gateway next applies its config: …`. On a root failure or a validator outage
  no endpoint is judged, so each endpoint keeps the message it has. It is absent otherwise. A Warning
  event `InvalidEndpointsExcluded` marks it appearing or changing.
  The exclusions themselves change neither `ConfigValid` nor `Ready`: the
  gateway serves every valid endpoint.
- KrakenDGateway `ConfigValid=False`, reason **`GatewayRootInvalid`**: the
  gateway root fails on its own. No endpoint is judged or blamed, and the last
  applied config keeps serving. The message quotes the root's output.
- KrakenDGateway `ConfigValid=False`, reason **`CombinedConfigInvalid`**: the
  endpoints that pass on their own fail the config together. No endpoint is
  blamed and the last applied config keeps serving. The message reads "The
  endpoints that pass krakend check on their own fail it together, so the last
  applied config keeps serving; any endpoint that fails on its own is excluded
  first. The check's output quotes the endpoints' values and is only in the
  operator log (\"the gateway's config fails krakend check only together\")."
  Three paths lead here: a write that admission let through with a warning
  (see Admission, below); a gateway with more router clashes than the render
  resolves (the full check's route stage refuses the rest, so remove the
  clashing endpoints); and the safety re-check after exclusions, which can
  fail even though the excluded endpoints are out. Report any other case with
  that log line.
- **The gateway no longer writes `ConfigValidationFailed`** on `ConfigValid`,
  nor emits it as an event. Alerts, dashboards and scripts that select it, such
  as the event query under [Post-Upgrade Verification](#post-upgrade-verification),
  must select `GatewayRootInvalid` and `CombinedConfigInvalid`. Excluded
  endpoints are alerted on with the new metric. The AutoConfig controller still
  holds an operation with reason `ConfigValidationFailed`; its message now
  quotes only that operation's own output (the check renders the gateway root
  with the endpoint, so output from a failure that occurs only with a root
  setting can reveal that setting).

### New metric, and a narrower failure counter

`krakend_operator_gateway_excluded_endpoints{namespace, gateway, reason}`
(gauge): the endpoints a gateway excludes, by `Accepted` reason (`EndpointInvalid`
or `PolicyInvalid`).
- It is absent while there are none.
- It follows the endpoints' recorded verdicts, and the first pass after an
  operator restart that reaches the exclusion report rebuilds it (an early
  return skips the report).
- It counts an endpoint as soon as its exclusion is recorded, including while
  the gateway cannot apply its newest config.
- It is removed with the gateway.

The gateway label is `gateway`, not `name`. `krakend_operator_gateway_config_valid`
stays `1` while endpoints are excluded, so alert on the new gauge (see
`KrakenDGatewayEndpointsExcluded` in the runbook).

`krakend_operator_config_validation_failures_total` now counts only the
rejections of a gateway root, a policy or an endpoint checked on its own: a
rejection is counted once per change of what the gateway controller checks, not
once per reconcile; content that comes back, the same policy on another gateway
and an operator restart each count again. The whole render's full check and its re-check without
the excluded endpoints are not counted: they still contain the excluded
endpoints, so counting them would add a rejection for every unrelated edit on
the gateway. A failure that needs several endpoints together therefore adds
nothing to the counter; it shows as `ConfigValid=False` with reason
`CombinedConfigInvalid`, its Warning event, and
`krakend_operator_gateway_config_valid == 0`.

### Router clashes resolve oldest-first

Two endpoints' entries that KrakenD's router cannot serve together although
their paths differ in shape no longer fail the gateway's check. Examples:
`GET /users/{id}` with `GET /users/{userId}/orders`; an EE `GET /files/*` with
`GET /files/x`; or paths of two methods while `router.auto_options` is on.
- The entry of the older KrakenDEndpoint (by creation time, then
  namespace/name) is served, and the newer one is left out, as same-shape
  duplicates already were.
- A newer entry is left out while any older entry it clashes with exists,
  whether or not that older entry is served itself. So deleting an endpoint
  can only bring other entries back, never push a served entry out, and an
  update can push an entry out only through a clash its own entries take part
  in, which admission refuses (below).
- With `router.auto_options`, every entry is compared with the left-out
  entries together with the OPTIONS route of its path, even when a served
  entry on that path already registers it. So an entry can be left out next
  to an entry the router would serve it with. For example, when
  `GET /a/{id}` is served and another endpoint's newer `GET /a/{name}/x` lost
  to it, a `POST /a/{id}` created after both is left out, and refused at
  admission: its `OPTIONS /a/{id}` route clashes with `OPTIONS /a/{name}/x`,
  although it shares `OPTIONS /a/{id}` with the served entry. Removing or
  renaming the left-out `GET /a/{name}/x` lets it be served.
- A same-shape duplicate that is left out (two endpoints serving
  `GET /a/{id}` and `GET /a/{key}`) still keeps newer entries out that clash
  with its own parameter names. So an entry can be refused next to the served
  duplicate although that one would accept it: with `GET /a/{id}` served and
  `GET /a/{key}` left out, a newer `GET /a/{id}/y` is left out, and refused at
  admission, because it clashes with `GET /a/{key}`. Otherwise deleting the
  `GET /a/{id}` endpoint would serve `GET /a/{key}` and push the newer entry
  out. Removing the left-out duplicate, or giving it the served entry's
  parameter names, lets it be served.
- The losing endpoint reports `PartiallyAccepted` or `EndpointConflict`, and
  `status.conflicts` lists the entry once for every older endpoint it clashes
  with, naming that endpoint as the winner. A winner's own entry may be left
  out as well.
- The resolution only leaves entries out. A render with no clash between
  endpoints is the same as without it (the renderer's tests pin its checksum),
  so this rolls no gateway that has none.
- Admission refuses a write that would create a new clash, in either
  direction: an endpoint write that would lose a clash, or that would push
  another endpoint's entry out; and a gateway write, such as turning
  `router.auto_options` on. The AutoConfig controller holds such an operation.
- A gateway resolves at most 21 entries that the router refuses next to older
  ones in one render (an entry that clashes with several older endpoints counts
  once, and so does one left in because its clash needs several older routes
  together). Past that, the remaining
  entries are rendered as they are, and the operator can no longer tell which
  write adds a clash. So admission refuses every create or
  update of an endpoint on that gateway and every gateway write that changes
  its render, and the AutoConfig controller holds every write, until the
  existing clashes are fixed. Deletes are still admitted, and they are the way
  out. Only objects stored without admission, or written in a race, can reach
  it. The gateway then reports `CombinedConfigInvalid`, because the full
  check's route stage refuses the entries that were not resolved: remove the
  clashing endpoints. Its `status.conflicts` do not list the clashes past the
  cap, and on a pass that applies nothing they stay as the last applied render
  recorded them.
- A clash between two entries of one endpoint, or with the gateway's own
  health route, is that endpoint's own failure (`EndpointInvalid`).

### Admission

- **Endpoint writes** are judged on their own:
  - First the gateway root alone and each policy the endpoint references
    alone. Their verdicts are remembered across requests, the 256 most recent
    per operator pod.
  - Then the endpoint with the root and the policies it references.
  - A denial quotes only the output of the endpoint's own check. Nothing
    about another endpoint is quoted, but that check renders the gateway root
    with the endpoint, so output from a failure that occurs only with a root
    setting can reveal that setting to the endpoint's author.
  - When the gateway root fails on its own, the write is admitted with a
    warning that names the gateway and asks its owner to fix it.
  - An update whose stored version also fails on its own is admitted with a
    warning; the endpoint stays excluded until it passes.
  - A referenced policy that fails on its own is named, never quoted.
- **Policy writes** are judged alone (the denial quotes the policy's own
  output, unless the stored policy fails alone too, which leaves the decision
  to its endpoints). Then, for each gateway that uses the policy, the gateway's
  root is checked alone and then with the endpoints that use the policy there
  and are not recorded as excluded. Every gateway is checked first; naming
  comes after:
  - When those endpoints fail together, each of them is checked on its own
    with the new policy; when they pass, only the endpoints that lost an entry
    in that check.
  - An endpoint that fails with the new policy is checked with the stored one.
    If it passes there, the change breaks it: the write is denied, naming it
    (`<ns>/<name>`), never quoting it. One endpoint that already fails does
    not hide another that the change breaks.
  - When every endpoint that fails already failed with the stored policy, or
    the gateway's root fails on its own, the write gets a warning instead.
  - On a create there is no stored policy. The endpoints are checked with a
    policy of the same name and namespace that holds nothing, so an endpoint
    that fails whatever the policy holds (for example a bad backend host) only
    draws a warning, `… already fail validation whatever this policy holds`,
    and does not block the policy's owner.
  - When the endpoints fail only together (each passes on its own), the stored
    policy decides: if they passed together with it, the change causes the
    failure and is denied, naming the gateway and quoting nothing; if they
    already failed together, the write gets a warning. A create has no stored
    policy, so it is denied.
- **Gateway writes**:
  - The root must pass on its own, and the denial quotes it. An update whose
    stored root fails too only warns.
  - Then the root is checked with the endpoints it serves now, and the
    endpoints are judged as for a policy write: an update is refused when an
    endpoint fails with the new root and passed with the stored one, naming
    it. When the stored root fails on its own, no endpoint passed with it, so
    an endpoint that the last applied config served (`Accepted` is `True`, or
    `PartiallyAccepted`, for its current generation) is refused when it fails
    with the new root; any other endpoint only draws a warning. Endpoints that
    fail only together are refused too, without comparing them with the stored
    group. On a create, endpoints that already reference the gateway and fail
    with it only draw a warning naming them (or saying they fail only
    together, or that they could not be checked).
  - A render-neutral update (image, replicas, probes) is still not checked.
- Naming stops at 20 endpoints (`(+N more not checked)`) or when a check could
  not run, for example because the 12 s admission budget ran out (`(N not
  checked within the admission time)`). A denial prints at most one of the two.
  The names are bounded by bytes: names that were found broken but do not fit
  are folded into `(+N more)`, and the two not-checked counts are always
  kept. Once an endpoint the change breaks has been found, the answer is that
  denial, even if a later check fails or the budget runs out. When the budget
  runs out, or a check cannot run, before any is found, the answer is
  `500 Internal Error`; retry the request. On a large gateway with many
  endpoints that already fail but are not yet recorded as excluded (for
  example right after they were changed), a policy or gateway write can get
  that `500` until the gateway controller records their exclusion.
- A policy or gateway write that is admitted with a warning can still lead to
  `CombinedConfigInvalid`. Both check the endpoints that already fail on their
  own before they check the endpoints together. So while an endpoint that
  fails on its own is not yet recorded as excluded, a write that makes the
  other endpoints fail only together is admitted with a warning, and the
  gateway then reports `CombinedConfigInvalid`.
- **Cost per request, in `krakend check` runs (most of them remembered):**
  - an endpoint write: the gateway root, each policy it references, and the
    endpoint, plus 1 when it fails and references a policy of another
    namespace, plus the stored version's checks for a failing update;
  - a policy write: 1, plus 2 per gateway that uses it, plus 1 per endpoint
    judged on its own (every endpoint that uses the policy there when they fail
    together, otherwise those that lost an entry) and 1 more for each of those
    that fails, plus 1 for the stored group when none fails on its own;
  - a gateway write: 2, plus 1 for the stored root when the root fails or
    when endpoints are judged on their own, plus the endpoint checks as for a
    policy write, plus 1 for the stored group when none fails on its own and
    the stored root passes.
  - On the pinned binary, at the chart's 500m CPU limit, one endpoint's check
    takes about 0.1 s and 500 endpoints are linted together in about 0.17 s,
    far inside the 15 s webhook timeout; what delays a request is waiting for
    a validation slot.

### Known limits

- **Policy references across namespaces need no consent.** An endpoint may
  reference a KrakenDBackendPolicy of any namespace. That gives its owner two
  levers over the policy's owner: an update of the policy that would break the
  referencing endpoint is refused, and whether that endpoint reports
  `EndpointInvalid` or `PolicyInvalid` tells its owner whether the policy is
  what breaks it. Neither quotes the policy. This predates per-object
  validation; restrict who may create endpoints in namespaces you do not
  trust.
- **CE fallback is read at two different times.** Admission takes CE fallback
  from the gateway's status (`LicenseDegraded`); the gateway controller decides
  it during the reconcile, from the license it reads. Around a license change,
  an endpoint can be admitted and then excluded, or the reverse, until the
  gateway's status catches up.
- **Admission renders no Dragonfly block.** The gateway controller checks the
  root and each endpoint with the Redis address Dragonfly provides; admission
  checks them without it. A root that fails only with that address is
  reported by the controller (`GatewayRootInvalid`), but gateway admission
  does not refuse it.

---

## Unreleased — OpenTelemetry logs, traces and metrics

The operator's logs, traces and metrics now use OpenTelemetry. The telemetry itself changes no CRD, RBAC rule or webhook. What changes on the first new pod is where the logs go and what they look like.

### Logs are OpenTelemetry records, in JSON on stdout

The operator wrote zap's console text to **stderr**. It now writes one OpenTelemetry log record per line, in JSON, to **stdout**, and so do controller-runtime, client-go (klog) and the Go HTTP servers inside it:

```json
{"Timestamp":"2026-10-06T12:00:00.1Z","ObservedTimestamp":"2026-10-06T12:00:00.1Z","Severity":9,"SeverityText":"INFO","Body":{"Type":"STRING","Value":"Serving metrics server"},"Attributes":[{"Key":"bindAddress","Value":{"Type":"STRING","Value":":8443"}}],"TraceID":"00000000000000000000000000000000","SpanID":"0000000000000000","TraceFlags":"00","Resource":[{"Key":"service.name","Value":{"Type":"STRING","Value":"krakend-operator"}}],"Scope":{"Name":"krakend-operator/controller-runtime/metrics","Version":"","SchemaURL":"","Attributes":{}},"DroppedAttributes":0}
```

Update log pipelines **before** upgrading:
- read the container's stdout; a collector that reads only stderr sees nothing but the few lines below;
- the message is `Body.Value`;
- the level is `SeverityText`;
- the logger name is `Scope.Name`: `krakend-operator/` followed by the logger's names joined with `/` (`controller-runtime.metrics` becomes `krakend-operator/controller-runtime/metrics`). The exception is the SDK diagnostics logger, whose scope is the bare `opentelemetry` (grpc-go's records on it are `opentelemetry/grpc`);
- key/value pairs are in `Attributes`; values the log bridge cannot render natively are converted to text (controller-runtime's `reconcileID` is the bare UUID, the same as the span's `controller_runtime.reconcile_id`);
- an error is in the `exception.message` (its text) and `exception.type` (its Go type) attributes; there is no `error` key;
- error records carry no stack trace: the one zap added in development mode is gone.

A record logged inside a reconcile or an admission request carries its trace's `TraceID` and `SpanID`.

| Flag | Now |
|---|---|
| `--zap-log-level` | Same values and meaning (`debug` = verbosity 1; N = verbosity N) |
| `--zap-devel` | Still on by default, so the default level stays debug; `false` logs at info |
| `--zap-encoder` | `json` keeps JSON; `console` selects indented JSON. Prefer `--log-format`. Ignores case |
| `--log-format` | New: `json` (default) or `pretty`. Ignores case |
| `--zap-stacktrace-level`, `--zap-time-encoding` | Accepted and ignored; a startup record names them |

grpc-go's log, which the OTLP gRPC exporters use, goes to stdout only, never to OTLP, as the `opentelemetry/grpc` logger, so a collector that cannot be reached does not queue a record for every reconnect. These still write to stderr directly:
- flag errors (exit status 2) and `--help` (exit status 0);
- an invalid logging flag (exit status 2);
- an unusable `OTEL_*` setting, such as an unsupported exporter or protocol (the operator prints `setting up telemetry: …` and exits with status 1);
- OpenTelemetry's own warnings about its `OTEL_*` variables while the exporters start;
- a failed final flush;
- Go runtime crashes.

### Traces

Each reconcile and each admission request is a trace, with its stages, every `krakend check` run, every Kubernetes API call and every AutoConfig spec fetch below it. See the runbook's Tracing section for the span names.
- Nothing is exported unless an OTLP endpoint is configured.
- The trace context is sent to the Kubernetes API server and continued from it on admission requests. If the API server sends an unsampled context (for example with API server tracing at a low `samplingRatePerMillion`), the default sampler leaves the admission request unrecorded. `OTEL_TRACES_SAMPLER=always_on` (Helm: `telemetry.traces.sampler`) records it regardless.
- It is never sent to OpenAPI spec hosts, or to the hosts of their `$ref` documents.
- Some Kubernetes API traffic is not traced: informer list and watch requests, leader-election lease renewals, the metrics endpoint's TokenReview and SubjectAccessReview, and the discovery the operator does at startup record no span and send no trace header. RESTMapper discovery requests are untraced too: client-go sends them without a span context, including the lazy RESTMapper's lookups during a reconcile, so only the operator's own optional-CRD lookups are spans. Kubernetes events are raised inside a reconcile but written asynchronously, without a span.
- A lookup of an optional CRD during a reconcile is a `k8s.discovery` span of the stage that needs it.
- The `admission.validate <Kind>` span (not the `admission <path>` server span) says whether it was allowed (`admission.allowed`) and with which status code (`admission.code`: 200, the denial's own code, or 500 when the check could not run). It never says why: no span carries a denial's text, a warning's text or krakend's output.
- On an admission request's server span, `client.address` is the first `X-Forwarded-For` value, which any caller in the cluster can set; `network.peer.address` is the real peer.

### Metrics keep their names

`/metrics` serves the same `krakend_operator_*` names, labels, help text and histogram buckets as before. They are now recorded with OpenTelemetry and exported by its Prometheus exporter. No `target_info` series or `otel_scope_*` label is added. controller-runtime's own metrics are unchanged.

One difference: a deleted gateway's `krakend_operator_reconcile_duration_seconds` series stays until the operator restarts. Its gauges are removed as before. With an OTLP endpoint, the metrics are also pushed over OTLP. Turn that off with `OTEL_METRICS_EXPORTER=none` (Helm: `telemetry.otlp.signals.metrics: false`).

### New environment variables and chart values

| Variable | Default | Effect |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` (or `OTEL_EXPORTER_OTLP_{TRACES,METRICS,LOGS}_ENDPOINT`) | unset | Exports that signal over OTLP; unset exports nothing. One that is not a URL stops that signal's export (a startup record names the variable) |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` | `grpc` or `http/protobuf` (a signal's own `OTEL_EXPORTER_OTLP_<SIGNAL>_PROTOCOL` overrides it); for a signal that has an endpoint, anything else stops startup |
| `OTEL_EXPORTER_OTLP_HEADERS` (or a signal's own) | unset | Headers sent to the collector, as URL-encoded `name=value` pairs separated by commas. A malformed list stops that signal's export, not the operator; the startup record names the variable and never its value |
| `OTEL_{TRACES,METRICS,LOGS}_EXPORTER` | `otlp` | `none` turns that signal's export off; any other value stops startup |
| `OTEL_METRIC_EXPORT_INTERVAL` | `60000` (ms) | How often metrics are pushed over OTLP |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | `parentbased_always_on` | Trace sampling |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | `krakend-operator` | Override the reported resource; a malformed attribute is dropped and named at startup |
| `POD_NAME` | from the downward API | Reported as `k8s.pod.name`; set by the chart, kustomize and the OLM bundle |

The Helm chart gains a `telemetry:` block: `otlp.endpoint`, `otlp.nodeCollector`, `otlp.protocol`, `otlp.headersSecret`, `otlp.signals`, `traces.sampler`, `traces.samplerArg`, `resourceAttributes` and `logs.format`.
- With the defaults, nothing new is rendered except `POD_NAME`.
- If a log agent already collects the pods' stdout, set `telemetry.otlp.signals.logs: false` when enabling OTLP, so records are not delivered twice.
- `telemetry.otlp.nodeCollector.enabled` exports to the collector on the pod's node, at `http://<node IP>:<port>` (the node IP comes from the downward API; `port` is 4318 for `http/protobuf` and 4317 for `grpc`, so set `otlp.protocol` to match). It also adds `k8s.node.name`, `k8s.pod.uid` and `k8s.pod.ip` to the resource. It takes the place of `otlp.endpoint`: setting both fails the render. IPv6-primary nodes are not supported, because the node IP would be an unbracketed address in the URL; set `otlp.endpoint` there.
- `telemetry.resourceAttributes` values are percent-encoded for you, keys may not contain `,` or `=`, and a numeric value needs quoting (`"1234567"`) so YAML does not reformat it. A `telemetry.traces.samplerArg` of `0` is kept.
- On a large cluster, start with `telemetry.traces.sampler: parentbased_traceidratio` and `samplerArg: "0.1"`.

A bad collector Secret does not crash-loop the operator: a malformed header or endpoint variable stops that signal's export and is named in a startup record, without its value. A missing headers Secret, or a missing key in it, leaves the export without headers, so the collector may reject it and the rejections appear as `OpenTelemetry pipeline error` records; the operator still starts.

The image now reports its version (`service.version`) from the `VERSION` build argument. `make docker-build` stamps the Makefile's `VERSION`, a plain `docker build` stamps `dev`, and a release stamps its tag.

### Shutdown flushes telemetry for at most 5 seconds

The manager waits at most 4 seconds for its controllers and servers to stop (its graceful shutdown timeout, which controller-runtime defaults to 30 seconds). After it stops, the operator flushes the batched OTLP traces, metrics and logs, waiting at most 5 seconds, so that both fit the pod's 10 second termination grace period. Stdout logs are written as they are logged; only OTLP data can be cut off. A failed flush is printed to stderr. A second signal exits at once, without the flush.

### AutoConfig fetch errors no longer show URL credentials

A fetch error in an AutoConfig's status shows the spec URL without user information or fragment, and with each query value (or bare key) replaced by `REDACTED`. For example, `fetching https://specs.example/api.json?token=REDACTED: …` replaces a message that showed the token. A URL that cannot be shown safely appears as `<unparseable URL>`, and an unsupported-scheme error no longer echoes the scheme.

### Rollback

Rolling back restores zap's console logs on stderr. No state migrates either way.

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
