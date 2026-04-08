# KrakenD Operator Helm Chart

Installs the [KrakenD Operator](https://github.com/MyCarrier-DevOps/KrakenD-Operator) for managing KrakenD API Gateway instances on Kubernetes via Custom Resources.

## Prerequisites

- Kubernetes 1.28+
- Helm 3.x

## Install

```bash
helm repo add krakend-operator https://mycarrier-devops.github.io/KrakenD-Operator
helm repo update
helm install krakend-operator krakend-operator/krakend-operator -n krakend-operator-system --create-namespace
```

## Configuration

See [values.yaml](values.yaml) for the full list of configurable parameters.

| Parameter | Description | Default |
|---|---|---|
| `replicaCount` | Number of operator pods | `1` |
| `image.repository` | Operator image repository | `ghcr.io/mycarrier-devops/krakend-operator` |
| `image.tag` | Operator image tag (defaults to chart appVersion) | `""` |
| `leaderElection.enabled` | Enable leader election | `true` |
| `metrics.enabled` | Expose Prometheus metrics | `true` |
| `resources` | CPU/memory requests and limits | See values.yaml |

## Uninstall

```bash
helm uninstall krakend-operator -n krakend-operator-system
```

CRDs are not removed on uninstall. To remove them manually:

```bash
kubectl delete crd krakendgateways.gateway.krakend.io krakendendpoints.gateway.krakend.io krakendbackendpolicies.gateway.krakend.io krakendautoconfigs.gateway.krakend.io
```
