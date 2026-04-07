# KrakenD Operator — Development Guide

This directory contains the operator source code, built with [Operator SDK](https://sdk.operatorframework.io/) and [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime).

## Prerequisites

- Go 1.24+
- Docker or Podman
- kubectl configured for a Kubernetes 1.28+ cluster
- operator-sdk v1.42+

## Project Layout

```
cmd/            Main entrypoint
api/v1alpha1/   CRD type definitions and deepcopy
internal/
  controller/   Reconciliation controllers (Gateway, Endpoint, BackendPolicy, AutoConfig)
  autoconfig/   OpenAPI fetcher, parser, endpoint generator
  renderer/     KrakenD JSON configuration renderer
  resources/    Kubernetes resource builders (Deployment, ConfigMap, Service, etc.)
  webhook/      Validating and mutating webhooks
  util/         Shared utilities (conditions, labels)
config/         Kustomize manifests (CRDs, RBAC, manager, samples)
bundle/         OLM operator bundle
```

## Building

```bash
make build                  # Build the manager binary
make generate               # Generate deepcopy methods
make manifests              # Generate CRD and RBAC manifests
make bundle                 # Generate OLM bundle
```

## Testing

```bash
go test -race ./internal/... ./api/... ./cmd/...   # Unit tests
make test-e2e                                       # End-to-end tests (requires Kind)
```

## Linting

```bash
golangci-lint run -c ../.github/.golangci.yml
```

## Running Locally

```bash
make install    # Install CRDs into the current cluster
make run        # Run the operator outside the cluster
```

## Deploying

```bash
make deploy IMG=ghcr.io/mycarrier-devops/krakend-operator:latest
make undeploy   # Remove the operator
```

## License

Copyright 2026. Licensed under the Apache License, Version 2.0.
