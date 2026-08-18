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

---

## Version Compatibility

| Operator Version | Kubernetes | KrakenD CE | Go |
|---|---|---|---|
| 0.x (alpha) | 1.28+ | 2.13+ | 1.26+ |

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
   rolled out to prod, or the job will fail on every run.
2. **`securityContext`/`podSecurityContext` now MERGE instead of REPLACE.**
   Previously, setting any field in `spec.postRestartJob.securityContext` or
   `podSecurityContext` discarded ALL hardened defaults (e.g. prod's
   `runAsUser: 0` silently dropped `drop: ["ALL"]` and the seccomp profile).
   Now only the fields you set are overridden; unset fields keep the
   hardened default. This is strictly safer than before, but review your
   existing overrides — fields you were implicitly relying on being *unset*
   (e.g. no seccomp profile) will now inherit the operator default.
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
7. **Dragonfly's `securityContext`/`podSecurityContext` REPLACE, they do
   NOT merge — this is the opposite of the Job's behavior (item 2 above).**
   `spec.dragonfly.podSecurityContext` / `spec.dragonfly.containerSecurityContext`
   are still handled by the pre-existing `dragonflyPodSecCtx`/
   `dragonflyContainerSecCtx` full-replace pattern in
   `internal/resources/dragonfly.go` — setting ANY field there (even one
   unrelated to `fsGroup`) discards the ENTIRE hardened default, including
   `fsGroup: 999`. Losing `fsGroup: 999` on an existing PVC-backed Dragonfly
   instance is a live ownership hazard: the image's built-in `dfly` uid/gid
   999 process loses group-write access to `--dir=/dragonfly/snapshots` and
   crashloops. This PR's broader CRD field-exposure sweep (more
   `spec.dragonfly.*` security fields are now settable via Helm/OLM values)
   widens exposure to this pre-existing gap even though the gap itself is
   not new — review any `spec.dragonfly.podSecurityContext`/
   `containerSecurityContext` override for a missing `fsGroup: 999` /
   `runAsUser: 999` / `runAsGroup: 999` before upgrading. Tracked for a
   proper merge-semantics fix (mirroring the Job's
   `strategicMergeSecurityContext`) as a follow-up.
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
   offending combination, is rejected.

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
