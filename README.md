# KrakenD Operator

Kubernetes operator for managing [KrakenD API Gateway](https://www.krakend.io) instances declaratively via Custom Resources.

## Features

- **KrakenDGateway** — Deploy and manage KrakenD API Gateway instances with full lifecycle management
- **KrakenDEndpoint** — Define API endpoints with backend routing, header forwarding, and query string configuration
- **KrakenDBackendPolicy** — Reusable policies for rate limiting, circuit breaking, and HTTP caching
- **KrakenDAutoConfig** — Automatically generate endpoints from OpenAPI/Swagger specifications
- **License Management** — Enterprise Edition license monitoring with expiry warnings and Community Edition fallback
- **Dragonfly Integration** — Optional DragonflyDB-based response caching
- **Istio Integration** — Optional VirtualService generation for mesh routing
- **External Secrets** — ExternalSecret integration for license management

## Quick Start

### Prerequisites

- Kubernetes 1.28+
- Helm 3.x

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
  endpoint: /api/users
  method: GET
  backends:
    - host: http://users-service.default.svc.cluster.local:8080
      urlPattern: /v1/users
      timeout: "10s"
```

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

See [LICENSE](LICENSE).
