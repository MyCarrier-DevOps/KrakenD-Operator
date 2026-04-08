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
```

---

## Gateway Lifecycle

### Phases

| Phase | Meaning |
|---|---|
| `Pending` | Initial state, awaiting first reconcile |
| `Rendering` | Config is being rendered from endpoints |
| `Validating` | Config is being validated via KrakenD CLI |
| `Deploying` | Deployment is rolling out |
| `Running` | Deployment converged, all replicas ready |
| `Degraded` | EE license expired — fell back to CE edition |
| `Error` | Fatal error (validation failure, rollout timeout, license missing) |

### Common Conditions

| Condition | Meaning |
|---|---|
| `ConfigValid` | Config passed KrakenD CLI validation |
| `Available` | Deployment has ready replicas |
| `Progressing` | Deployment rollout in progress |
| `DragonflyReady` | DragonflyDB instance is operational |
| `IstioConfigured` | VirtualService has been reconciled |
| `LicenseValid` | License secret is present and not expired |
| `LicenseExpiringSoon` | License will expire within the safety buffer |
| `LicenseSecretUnavailable` | License secret could not be read |

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
- Liveness probe failing (check `/healthz` endpoint)
- `ProgressDeadlineExceeded` — sets phase to `Error` with `RolloutFailed` event

### Gateway stuck in `Error`

**Diagnosis:**
```bash
kubectl describe krakendgateway <name>
kubectl get events --field-selector involvedObject.name=<name> --sort-by='.lastTimestamp'
```

**Common causes:**
- Config validation failure — check `ConfigValid` condition message
- License missing for EE gateway — provide license secret
- Rollout timeout — check Deployment events

### Endpoint shows `Invalid`

**Diagnosis:**
```bash
kubectl describe krakendendpoint <name>
```

**Common causes:**
- `gatewayRef` points to a non-existent gateway
- `policyRef` references a non-existent policy
- Duplicate endpoint path + method combination (oldest wins)

### AutoConfig not generating endpoints

**Diagnosis:**
```bash
kubectl describe krakendautoconfig <name>
kubectl get events --field-selector involvedObject.name=<name>
```

**Common causes:**
- OpenAPI spec fetch failure (check URL, auth credentials)
- CUE evaluation error (check embedded/custom CUE definitions)
- Filter excludes all operations

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

Set `spec.replicas` on the `KrakenDGateway` resource. If HPA is configured (`spec.autoscaling`), the operator creates an HPA targeting the gateway Deployment.

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
