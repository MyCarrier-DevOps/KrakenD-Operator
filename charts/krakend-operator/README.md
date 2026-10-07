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
| `replicaCount` | Number of operator pods. More than one requires `leaderElection.enabled` | `2` |
| `image.repository` | Operator image repository | `ghcr.io/mycarrier-devops/krakend-operator` |
| `image.tag` | Operator image tag (defaults to chart appVersion) | `""` |
| `leaderElection.enabled` | Enable leader election | `true` |
| `podDisruptionBudget.enabled` | Create a PodDisruptionBudget (`maxUnavailable: 1`) when `replicaCount` > 1 | `true` |
| `affinity` | Pod affinity; when empty, replicas prefer different nodes | `{}` |
| `metrics.enabled` | Expose Prometheus metrics | `true` |
| `metrics.serviceMonitor.enabled` | Create a Prometheus Operator `ServiceMonitor` for the operator's metrics (needs the `monitoring.coreos.com` CRDs) | `false` |
| `metrics.serviceMonitor.additionalLabels` | Extra labels for the `ServiceMonitor` (for example the one your Prometheus selects on) | `{}` |
| `webhooks.enabled` | Serve the validating admission webhooks. `false` runs the operator without a webhook server; only render-time validation then protects gateways | `true` |
| `webhooks.caBundle` | CA bundle (PEM or base64-encoded PEM) for the webhook, used when `webhooks.certManager.enabled` is `false` | `""` |
| `autoconfig.maxConcurrentReconciles` | KrakenDAutoConfigs reconciled at once; each reconcile fetches its OpenAPI spec over the network, so a slow upstream delays only its own AutoConfig | `4` |
| `telemetry.otlp.endpoint` | OTLP collector the operator exports traces, metrics and logs to (`OTEL_EXPORTER_OTLP_ENDPOINT`). Empty exports nothing | `""` |
| `telemetry.otlp.nodeCollector` | `enabled` exports to the OpenTelemetry collector on the pod's node, at `http://<node IP>:<port>` (`port` is `4318` for `http/protobuf`, `4317` for `grpc`), and labels records with `k8s.node.name`, `k8s.pod.uid` and `k8s.pod.ip`. Takes the place of `endpoint`; setting both fails the render | `enabled: false`, `port: 4318` |
| `telemetry.otlp.protocol` | `grpc` or `http/protobuf` | `http/protobuf` |
| `telemetry.otlp.headersSecret` | Secret `name` and `key` holding `OTEL_EXPORTER_OTLP_HEADERS` (`key1=value1,key2=value2`) | `name: ""`, `key: headers` |
| `telemetry.otlp.signals` | Which of `traces`, `metrics`, `logs` are exported when an endpoint is set | all `true` |
| `telemetry.traces.sampler`, `telemetry.traces.samplerArg` | `OTEL_TRACES_SAMPLER` and its argument | `""` (parent-based, always on) |
| `telemetry.resourceAttributes` | Extra `OTEL_RESOURCE_ATTRIBUTES` | `{}` |
| `telemetry.logs.format` | stdout log format: `json` or `pretty` | `json` |
| `resources` | CPU/memory requests and limits | See values.yaml |

## Scraping metrics

The metrics endpoint (HTTPS, port `metrics.service.port`) authorizes each
request. The chart lets the operator create the TokenReviews and
SubjectAccessReviews this needs, and ships a `<fullname>-metrics-reader`
ClusterRole. `<fullname>` is `fullnameOverride` when set. Otherwise it is the
release name when that already contains `krakend-operator` (or `nameOverride`,
when set), so the install command above gives `krakend-operator-metrics-reader`,
and `<release>-krakend-operator` when it does not. Bind that role to the
ServiceAccount that scrapes the operator:

```bash
kubectl create clusterrolebinding krakend-operator-metrics-reader \
  --clusterrole=<fullname>-metrics-reader \
  --serviceaccount=<prometheus-namespace>:<prometheus-serviceaccount>
```

With `metrics.serviceMonitor.enabled=true` the chart also creates a
`ServiceMonitor` that selects only the metrics Service
(`app.kubernetes.io/component: metrics`), not the webhook Service.

The `ServiceMonitor` scrapes over HTTPS with TLS verification off
(`insecureSkipVerify: true`), because the metrics server presents a self-signed
certificate, and it sends the Prometheus ServiceAccount's token as the bearer
token. Anything that can answer on the metrics endpoint can therefore capture
that token. Enable `metrics.serviceMonitor` only where the pod network is
trusted.

## Telemetry

The operator uses OpenTelemetry for its logs, traces and metrics.

- **Logs** are written to stdout as JSON, one record per line. Each record carries `TraceID` and `SpanID` when it was logged inside a reconcile or an admission request.
- **Metrics** are served on the metrics endpoint as before, with the same names and labels.
- **Traces and OTLP.** With `telemetry.otlp.endpoint` set, traces, metrics and logs are also exported over OTLP. Each reconcile and each admission request is one trace, with every krakend run and Kubernetes API call below it. Without an endpoint nothing is exported.
- **Node-local collector.** If a collector runs on every node, set `telemetry.otlp.nodeCollector.enabled=true` instead of an endpoint. The operator then reaches the collector on its node's IP and labels its telemetry with the node and pod. For a gRPC collector also set the port and protocol:

  ```bash
  helm upgrade --install krakend-operator ./charts/krakend-operator \
    --set telemetry.otlp.nodeCollector.enabled=true \
    --set telemetry.otlp.nodeCollector.port=4317 \
    --set telemetry.otlp.protocol=grpc
  ```

- **Headers.** Put collector credentials in a Secret and name it in `telemetry.otlp.headersSecret`.
- **Duplicate logs.** If a log agent already collects the pod's stdout, set `telemetry.otlp.signals.logs=false` so log records are not delivered twice.
- **External hosts.** Spec fetches to external hosts never carry the trace context.

## Uninstall

```bash
helm uninstall krakend-operator -n krakend-operator-system
```

CRDs are not removed on uninstall. To remove them manually:

```bash
kubectl delete crd krakendgateways.gateway.krakend.io krakendendpoints.gateway.krakend.io krakendbackendpolicies.gateway.krakend.io krakendautoconfigs.gateway.krakend.io
```
