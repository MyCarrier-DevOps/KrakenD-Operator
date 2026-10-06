# KrakenD Operator Helm Chart

Installs the [KrakenD Operator](https://github.com/MyCarrier-DevOps/KrakenD-Operator) for managing KrakenD API Gateway instances on Kubernetes via Custom Resources.

## Prerequisites

- Kubernetes 1.33+
- Helm 3.18 or later (older releases default `helm template` and `helm lint` to Kubernetes capabilities below 1.33 and refuse the chart's `kubeVersion` unless given `--kube-version`)

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
| `metrics.serviceMonitor.enabled` | Create a Prometheus Operator `ServiceMonitor` for the operator's metrics (needs the `monitoring.coreos.com` CRDs) | `false` |
| `metrics.serviceMonitor.additionalLabels` | Extra labels for the `ServiceMonitor` (for example the one your Prometheus selects on) | `{}` |
| `webhooks.enabled` | Serve the validating admission webhooks. `false` runs the operator without a webhook server; only render-time validation then protects gateways | `true` |
| `webhooks.caBundle` | CA bundle (PEM or base64-encoded PEM) for the webhook, used when `webhooks.certManager.enabled` is `false` | `""` |
| `autoconfig.maxConcurrentReconciles` | KrakenDAutoConfigs reconciled at once; each reconcile fetches its OpenAPI spec over the network, so a slow upstream delays only its own AutoConfig | `4` |
| `resources` | CPU/memory requests and limits | See values.yaml |

## Scraping metrics

The metrics endpoint (HTTPS, port `metrics.service.port`) authorizes each
request. The chart lets the operator create the TokenReviews and
SubjectAccessReviews this needs, and ships a `<fullname>-metrics-reader`
ClusterRole (`<fullname>` is `<release>-krakend-operator` unless you override
it). Bind that role to the ServiceAccount that scrapes the operator:

```bash
kubectl create clusterrolebinding krakend-operator-metrics-reader \
  --clusterrole=<fullname>-metrics-reader \
  --serviceaccount=<prometheus-namespace>:<prometheus-serviceaccount>
```

With `metrics.serviceMonitor.enabled=true` the chart also creates a
`ServiceMonitor` that selects only the metrics Service
(`app.kubernetes.io/component: metrics`), not the webhook Service.

## Uninstall

```bash
helm uninstall krakend-operator -n krakend-operator-system
```

CRDs are not removed on uninstall. To remove them manually:

```bash
kubectl delete crd krakendgateways.gateway.krakend.io krakendendpoints.gateway.krakend.io krakendbackendpolicies.gateway.krakend.io krakendautoconfigs.gateway.krakend.io
```
