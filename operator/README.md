# KrakenD Operator — Development Guide

This directory contains the operator source code, built with [Operator SDK](https://sdk.operatorframework.io/) and [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime).

## Prerequisites

- Go 1.26+
- Docker or Podman
- kubectl configured for a Kubernetes 1.33+ cluster
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

## Flags

The manager binary (`cmd/main.go`) takes these flags. The chart and the
kustomize manifests set the ones they need.

| Flag | Default | Purpose |
|------|---------|---------|
| `--metrics-bind-address` | `0` (off) | Address the metrics endpoint binds to (`:8443` for HTTPS, `:8080` for HTTP). |
| `--health-probe-bind-address` | `:8081` | Address the health probes bind to. |
| `--leader-elect` | `false` | Enable leader election. |
| `--metrics-secure` | `true` | Serve metrics over HTTPS. |
| `--webhook-cert-path`, `--webhook-cert-name`, `--webhook-cert-key` | `""`, `tls.crt`, `tls.key` | Directory and file names of the webhook serving certificate. |
| `--metrics-cert-path`, `--metrics-cert-name`, `--metrics-cert-key` | `""`, `tls.crt`, `tls.key` | Directory and file names of the metrics serving certificate. |
| `--enable-http2` | `false` | Allow HTTP/2 on the metrics and webhook servers. |
| `--enable-webhooks` | `true` | Serve the validating admission webhooks. |
| `--operator-username` | `system:serviceaccount:$POD_NAMESPACE:$POD_SERVICE_ACCOUNT`, or empty when either variable is unset | Username of the operator's own API requests. See below. |
| `--autoconfig-max-concurrent-reconciles` | `4` | How many KrakenDAutoConfigs reconcile at once. Each reconcile fetches its OpenAPI spec over the network, so a slow upstream delays only its own AutoConfig. Values below 1 mean 1. The AutoConfig config checks hold at most 2 of the 3 validation slots, so admission always finds one free. |

`--operator-username` is matched exactly, as a whole string, against the
username on each admission request. A KrakenDEndpoint write from that username
skips the admission render check when a KrakenDAutoConfig is the endpoint's
controller owner (`metadata.ownerReferences[].controller: true`). The check is
skipped because the AutoConfig controller validates the endpoints it is about
to write before it writes them. Every other check still runs: schema,
references, the audience rule, the entry rules and duplicate routes. Writes from any other user, writes to endpoints
without an AutoConfig controller, and every other kind get the full check.

The Helm chart and `config/manager/manager.yaml` set `POD_NAMESPACE`
(`fieldRef: metadata.namespace`) and `POD_SERVICE_ACCOUNT`
(`fieldRef: spec.serviceAccountName`) from the downward API, so the default is
the pod's own ServiceAccount, whatever the release or name prefix. A custom
deployment that does not set both variables must pass the flag, or every write
gets the render check. An empty value, from either route, disables the
exemption: nothing is trusted. The startup log line `admission skips the
render check for AutoConfig endpoint writes from` shows the username in use.

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

Copyright 2026 The KrakenD Operator Authors.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
