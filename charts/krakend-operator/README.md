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
| `metrics.certManager.enabled` | Serve metrics with a certificate cert-manager issues from the chart's self-signed Issuer (needs cert-manager); the `ServiceMonitor` then verifies it | `false` |
| `metrics.serviceMonitor.enabled` | Create a Prometheus Operator `ServiceMonitor` for the operator's metrics (needs the `monitoring.coreos.com` CRDs) | `false` |
| `metrics.serviceMonitor.additionalLabels` | Extra labels for the `ServiceMonitor` (for example the one your Prometheus selects on) | `{}` |
| `webhooks.enabled` | Serve the validating admission webhooks. `false` runs the operator without a webhook server; only render-time validation then protects gateways | `true` |
| `webhooks.caBundle` | CA bundle (PEM or base64-encoded PEM) for the webhook, used when `webhooks.certManager.enabled` is `false` | `""` |
| `networkPolicy.enabled` | Create a NetworkPolicy that admits ingress to the operator pods only on the metrics port (from namespaces matching `networkPolicy.metricsNamespaceSelector`) and the webhook port (from anywhere). Needs a CNI that enforces NetworkPolicy | `false` |
| `networkPolicy.metricsNamespaceSelector` | Namespaces allowed to scrape the metrics port | `matchLabels: {metrics: enabled}` |
| `autoconfig.maxConcurrentReconciles` | KrakenDAutoConfigs reconciled at once; each reconcile fetches its OpenAPI spec over the network, so a slow upstream delays only its own AutoConfig | `4` |
| `telemetry.otlp.endpoint` | OTLP collector the operator exports traces, metrics and logs to (`OTEL_EXPORTER_OTLP_ENDPOINT`). Empty exports nothing | `""` |
| `telemetry.otlp.nodeCollector` | `enabled` exports to the OpenTelemetry collector on the pod's node, at `http://<node IP>:<port>` (`port` is `4318` for `http/protobuf`, `4317` for `grpc`), and labels records with `k8s.node.name`, `k8s.pod.uid` and `k8s.pod.ip`. Takes the place of `endpoint`; setting both fails the render | `enabled: false`, `port: 4318` |
| `telemetry.otlp.protocol` | `grpc` or `http/protobuf` | `http/protobuf` |
| `telemetry.otlp.headersSecret` | Secret `name` and `key` holding `OTEL_EXPORTER_OTLP_HEADERS` (`key1=value1,key2=value2`) | `name: ""`, `key: headers` |
| `telemetry.otlp.signals` | Which of `traces`, `metrics`, `logs` are exported when an endpoint is set | all `true` |
| `telemetry.traces.sampler`, `telemetry.traces.samplerArg` | `OTEL_TRACES_SAMPLER` and its argument | `""` (parent-based, always on) |
| `telemetry.resourceAttributes` | Extra `OTEL_RESOURCE_ATTRIBUTES`. Values are percent-encoded for you; keys may not contain `,` or `=`; quote numeric values in YAML (`"1234567"`) so they are not reformatted | `{}` |
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

By default the `ServiceMonitor` scrapes over HTTPS with TLS verification off
(`insecureSkipVerify: true`), because the metrics server presents a self-signed
certificate it generates at startup, and it sends the Prometheus
ServiceAccount's token as the bearer token. Anything that can answer on the
metrics endpoint can therefore capture that token. With the default, enable
`metrics.serviceMonitor` only where the pod network is trusted.

To verify the certificate instead, set `metrics.certManager.enabled=true`
(cert-manager must be installed). The chart then asks cert-manager, through its
self-signed Issuer, for a certificate for the metrics Service, mounts it in the
operator pods, and the `ServiceMonitor` checks the Service's name and trusts
the certificate's CA. The Prometheus Operator reads that CA from the Secret
`<fullname>-metrics-server-cert`, which it looks up in the `ServiceMonitor`'s
namespace, the operator's. The Issuer is shared with the webhook certificate.

## User roles

The chart ships an admin, an editor and a viewer ClusterRole for each kind,
12 in all, named `<fullname>-<kind>-<role>-role` with `<kind>` one of
`krakendgateway`, `krakendendpoint`, `krakendbackendpolicy` and
`krakendautoconfig`. Under the release name `krakend-operator` they are, for
example, `krakend-operator-krakendendpoint-editor-role`. The operator does not
use them and nothing binds them: they exist for cluster admins to grant to
people.

- `admin`: every verb on the kind and `get` on its status.
- `editor`: create, delete, get, list, patch, update and watch on the kind, and `get` on its status.
- `viewer`: get, list and watch on the kind, and `get` on its status.

```bash
kubectl create rolebinding gateway-team-endpoints -n <namespace> \
  --clusterrole=krakend-operator-krakendendpoint-editor-role \
  --group=<group>
```

## Network policy

With `networkPolicy.enabled=true` the chart creates a NetworkPolicy for the
operator pods that admits ingress only on:

- the metrics port, from namespaces matching `networkPolicy.metricsNamespaceSelector` (by default those labelled `metrics: enabled`);
- the webhook port, 9443, from any source: the API server calls the webhooks and cannot be selected by labels, and a policy without this rule would make every write to the four kinds fail while `webhooks.failurePolicy` is `Fail`.

It needs a CNI that enforces NetworkPolicy. It adds no rule for the health
port: kubelet probes are host traffic, which common CNIs admit. If yours does
not, the probes fail.

## Telemetry

The operator uses OpenTelemetry for its logs, traces and metrics.

- **Logs** are written to stdout as JSON, one record per line. Each record carries `TraceID` and `SpanID` when it was logged inside a reconcile or an admission request.
- **Metrics** are served on the metrics endpoint as before, with the same names and labels.
- **Traces and OTLP.** With `telemetry.otlp.endpoint` (or the node collector below) set, traces, metrics and logs are also exported over OTLP. Each reconcile and each admission request is one trace, with every krakend run and Kubernetes API call below it. With neither, nothing is exported.
- **Node-local collector.** If a collector runs on every node, set `telemetry.otlp.nodeCollector.enabled=true` instead of an endpoint. The operator then reaches the collector on its node's IP and labels its telemetry with the node and pod. For a gRPC collector also set the port and protocol:

  ```bash
  helm upgrade krakend-operator krakend-operator/krakend-operator -n krakend-operator-system --reuse-values \
    --set telemetry.otlp.nodeCollector.enabled=true \
    --set telemetry.otlp.nodeCollector.port=4317 \
    --set telemetry.otlp.protocol=grpc
  ```

  `status.hostIP` is an unbracketed address on IPv6-primary nodes, so the node collector does not apply there; set `telemetry.otlp.endpoint` instead.

- **Headers.** Put collector credentials in a Secret and name it in `telemetry.otlp.headersSecret`. A missing Secret or key leaves the export without headers, so the collector may reject it, and the operator still starts.
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
