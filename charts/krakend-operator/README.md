# KrakenD Operator Helm Chart

Installs the [KrakenD Operator](https://github.com/MyCarrier-DevOps/KrakenD-Operator) for managing KrakenD API Gateway instances on Kubernetes via Custom Resources.

## Prerequisites

- Kubernetes 1.33+
- Helm 3.18 or later (older releases default `helm template` and `helm lint` to Kubernetes capabilities below 1.33 and refuse the chart's `kubeVersion` unless given `--kube-version`)
- A deployer that can read, create and patch CustomResourceDefinitions: the chart renders the CRDs, so install and upgrade write them

## Install

```bash
helm repo add krakend-operator https://mycarrier-devops.github.io/KrakenD-Operator
helm repo update
helm install krakend-operator krakend-operator/krakend-operator -n krakend-operator-system --create-namespace
```

Migrating from `make deploy`: see the [upgrade guide](../../docs/upgrade-guide.md#unreleased--the-helm-chart-is-the-only-install-path).

## Upgrade

The chart renders the CRDs, so a plain `helm upgrade` moves them with the
operator:

```bash
helm repo update
helm upgrade krakend-operator krakend-operator/krakend-operator -n krakend-operator-system --version "$VERSION"
```

Do not apply the CRDs by hand. A release installed from chart 0.14.x or earlier,
which shipped the CRDs in `crds/`, needs a one-time migration first: see the
[upgrade guide](../../docs/upgrade-guide.md#unreleased--the-crds-are-part-of-the-chart).
Without it the upgrade refuses, naming the CRD it cannot import, and changes
nothing.

See the [upgrade guide](../../docs/upgrade-guide.md#general-upgrade-procedure)
for the checklist to work through first.

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
| `networkPolicy.metricsNamespaceSelector` | Namespaces allowed to scrape the metrics port. Keys merge with the default, so a selector you set is ANDed with `metrics: enabled`; `--set networkPolicy.metricsNamespaceSelector.matchLabels.metrics=null` removes it | `matchLabels: {metrics: enabled}` |
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
namespace, the operator's. When the webhook certificate also comes from cert-manager, both share the Issuer.

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

The operator follows Secret references with its own permissions, so admission
asks the API server whether the person who writes the object may do the same,
when the reference is added or changed:

- a gateway's `spec.license.secretRef` needs `get` on that Secret in the
  gateway's namespace: the gateway mounts it into the container that runs
  `spec.image`, or `spec.ceImage` under CE fallback;
- a gateway's `spec.license.externalSecret` needs `create` on
  `externalsecrets.external-secrets.io` in the namespace: the operator creates
  an ExternalSecret that reads the named key through the named SecretStore, or
  a ClusterSecretStore that admits the namespace;
- a gateway's `spec.dragonfly.authentication.passwordFromSecret` needs `get` on
  that Secret: the Dragonfly operator puts it into the environment of the
  container that runs `spec.dragonfly.image`;
- an AutoConfig's `spec.openapi.auth.bearerTokenSecret` or `basicAuthSecret`
  needs `get` on that Secret, checked again whenever `spec.openapi` changes:
  the operator sends it as an `Authorization` header to `spec.openapi.url`.

A write without the right is refused with `422` naming the field. So these
roles reach no Secret their holder could not read, with the exceptions below,
each limited to one value. Every Secret an object names is reviewed, whether or
not the CRD allows naming two, and so is the license source that takes over when
an edit changes which one the gateway uses.

- whoever may edit a gateway whose license or Dragonfly password is already set
  can read that value by changing `spec.image`, `spec.ceImage` or
  `spec.dragonfly.image`;
- a holder of `create` on ExternalSecrets who may not `get` Secrets can read the
  synced license through the gateway image;
- an existing Secret named `<gateway>-license` with a `LICENSE` key is mounted
  while the ExternalSecret has not synced, indefinitely if its remote key does
  not exist, so the same holder can name a gateway to match such a Secret and
  read it.

Keep AutoConfig to trusted operators or CI for the SSRF reasons in the
[architecture document](../../architecture/README.md#ssrf-mitigation). The
viewer roles and the `krakendendpoint` and `krakendbackendpolicy` roles reach no
Secret.

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

Each rule is present only while its server is on (`metrics.enabled`, `webhooks.enabled`); with both off the policy admits no ingress.

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

The operator puts the finalizer `gateway.krakend.io/policy-protection` on every
KrakenDBackendPolicy, and only the operator removes it. Once the operator is
gone, deleting a policy, or a namespace that holds one, stays `Terminating`.
Reinstalling the operator also releases policies that no endpoint references.
To uninstall, in this order:

1. Uninstall the release:

   ```bash
   helm uninstall krakend-operator -n krakend-operator-system
   ```

   The gateways keep serving: KrakenD does not depend on the operator once its
   Deployments are rendered.

2. Wait for the operator pods to exit. A pod that is still running re-adds the
   finalizer. If the command returns at once, or reports no matching
   resources, the pods are already gone and you can carry on. On a timeout,
   check `kubectl -n krakend-operator-system get pods`:

   ```bash
   kubectl -n krakend-operator-system wait --for=delete pod \
     -l app.kubernetes.io/instance=krakend-operator,control-plane=controller-manager \
     --timeout=60s
   ```

3. Strip the finalizer from every policy. The command is idempotent, and needs
   `get`, `list` and `patch` on `krakendbackendpolicies` in every namespace.
   Run it before you delete a policy or a namespace that holds one, and before
   you delete the CRDs:

   ```bash
   kubectl get krakendbackendpolicies -A -o json \
     | jq -r '.items[] | .metadata as $m
         | ($m.finalizers // [] | index("gateway.krakend.io/policy-protection")) as $i
         | select($i != null) | "\($m.namespace) \($m.name) \($i)"' \
     | while read -r ns name i; do
         kubectl patch krakendbackendpolicy "$name" -n "$ns" --type=json -p "[
           {\"op\":\"test\",\"path\":\"/metadata/finalizers/$i\",\"value\":\"gateway.krakend.io/policy-protection\"},
           {\"op\":\"remove\",\"path\":\"/metadata/finalizers/$i\"}]"
       done
   ```

   The `test` operation makes a patch fail, rather than remove another
   finalizer, if the list changed in the meantime. The patch is an UPDATE that
   the policy webhook would intercept, but the release's webhook configuration
   is gone after step 1. A policy that is already `Terminating` is deleted as
   soon as its finalizer is gone.

### Remove the CRDs

This is not part of an uninstall. `helm uninstall` leaves the CRDs in place,
because they carry `helm.sh/resource-policy: keep`. Deleting
them deletes every KrakenD object, and with each gateway its Deployment,
Service and ConfigMaps, so every gateway stops serving. Do it only to remove
KrakenD from the cluster entirely, after the steps above. It hangs on
`krakendbackendpolicies` if the finalizer was not stripped:

```bash
kubectl delete crd krakendgateways.gateway.krakend.io krakendendpoints.gateway.krakend.io krakendbackendpolicies.gateway.krakend.io krakendautoconfigs.gateway.krakend.io
```
