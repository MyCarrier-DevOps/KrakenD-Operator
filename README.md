# KrakenD Operator

Kubernetes operator for managing [KrakenD API Gateway](https://www.krakend.io) instances declaratively via Custom Resources.

## Features

- **KrakenDGateway** — Deploy and manage KrakenD API Gateway instances with full lifecycle management
- **KrakenDEndpoint** — Define API endpoints with backend routing, header forwarding, and query string configuration
- **KrakenDBackendPolicy** — Reusable policies for rate limiting, circuit breaking, and HTTP caching
- **KrakenDAutoConfig** — Automatically generate endpoints from OpenAPI/Swagger specifications
- **License Management** — Enterprise Edition license tracking with expiry warnings and Community Edition fallback
- **Dragonfly Integration** — Optional DragonflyDB-based response caching
- **Istio Integration** — Optional VirtualService generation for mesh routing
- **External Secrets** — ExternalSecret integration for license management

## Quick Start

### Prerequisites

- Kubernetes 1.33+
- Helm 3.18 or later

### Install via Helm

```bash
helm repo add krakend-operator https://mycarrier-devops.github.io/KrakenD-Operator
helm repo update
helm install krakend-operator krakend-operator/krakend-operator \
  --namespace krakend-operator-system --create-namespace
```

### Install via Kustomize

```bash
cd operator
make deploy IMG=ghcr.io/mycarrier-devops/krakend-operator:latest
```

### Create a Gateway

```yaml
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDGateway
metadata:
  name: my-gateway
spec:
  edition: CE
  replicas: 2
  gateway:
    port: 8080
    timeout: "30s"
```

### Create an Endpoint

```yaml
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDEndpoint
metadata:
  name: users-list
spec:
  gatewayRef:
    name: my-gateway
  endpoints:
    - endpoint: /api/users
      method: GET
      backends:
        - host:
            - http://users-service.default.svc.cluster.local:8080
          urlPattern: /v1/users
      timeout: 10s
```

## Observability

The operator uses OpenTelemetry for its logs, traces and metrics.

- **Logs** go to stdout as JSON, one OpenTelemetry log record per line. A record logged during a reconcile or an admission request carries that trace's `TraceID` and `SpanID`. `--log-format=pretty` (Helm: `telemetry.logs.format`) indents each record for reading by hand.
- **Metrics** are served on the metrics endpoint (HTTPS, port 8443), under the same `krakend_operator_*` names and labels as earlier releases.
- **Traces.** Each reconcile and each admission request is one trace, with its stages, every `krakend check` run and every Kubernetes API call below it.
- **OTLP export.** Set `OTEL_EXPORTER_OTLP_ENDPOINT` (Helm: `telemetry.otlp.endpoint`, or `telemetry.otlp.nodeCollector.enabled` for a collector on every node) to export traces, metrics and logs over OTLP. Without it nothing is exported.

See the [runbook](docs/runbook.md#tracing) for finding a trace and correlating logs, and the [chart README](charts/krakend-operator/README.md#telemetry) for the values.

## Documentation

- [Operations Runbook](docs/runbook.md) — Day-2 operations, troubleshooting, and monitoring
- [Upgrade Guide](docs/upgrade-guide.md) — Version upgrade procedures
- [Architecture](architecture/README.md) — Operator design and architecture
- [Helm Chart](charts/krakend-operator/README.md) — Helm chart configuration reference

## Development

```bash
cd operator

# Build
make build

# Run tests
go test -race ./internal/... ./api/... ./cmd/...

# Lint
golangci-lint run -c ../.github/.golangci.yml

# Generate CRDs and deepcopy
make generate manifests

# Run locally against a cluster
make run
```

## License

Copyright 2026 The KrakenD Operator Authors.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
