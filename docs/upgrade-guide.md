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
   config changes.** The Job's identity checksum now includes the
   `postRestartJob` spec (script, securityContext, workingDir, ...), so
   editing the script (for example) now creates a new Job immediately
   instead of waiting for the next unrelated `krakend.json` change.
4. **The Job no longer silently re-runs on its own periodic ~24h TTL
   cadence.** `gw.status.lastPostRestartJobChecksum` is now read (previously
   write-only) to skip recreation when `TTLSecondsAfterFinished` garbage
   collects a finished Job object for a revision that already ran. If any
   workflow was relying on the previous (undocumented, believed-unintended)
   daily re-run cadence, it will need an explicit periodic trigger instead
   (e.g. a CronJob) — see PR description for the by-design determination.
5. **New optional `spec.postRestartJob.workingDir`** overrides the
   previously-forced `/tmp` working directory. Unset behaves exactly as
   before (defaults to `/tmp`).
