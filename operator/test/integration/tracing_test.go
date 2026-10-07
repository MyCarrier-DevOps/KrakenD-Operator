//go:build integration

/*
Copyright 2026 The KrakenD Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"fmt"
	"strings"
	"testing"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// A gateway reconcile is one trace on a real API server: its status write is a
// Kubernetes client span below the reconcile's status stage.
func TestTracing_GatewayStatusWriteIsAClientSpanOfItsReconcile(t *testing.T) {
	ns := testNamespace(t)
	key := createGateway(t, ns, "traced")
	waitForAppliedChecksum(t, key)

	eventually(t, func() error {
		spans := suiteTraces.Ended()
		for _, root := range spans.Named("reconcile KrakenDGateway").With(semconv.K8SNamespaceName(ns)) {
			if spans.Trace(root).Find("k8s update krakendgateways/status",
				"gateway.status", "reconcile KrakenDGateway") != nil {
				return nil
			}
		}
		return fmt.Errorf("no reconcile of %s whose status write is a client span below it", key)
	})
}

// Informer watches, lease renewals and event writes run outside any span:
// none may start a trace of its own.
func TestTracing_NoKubernetesClientSpanIsARoot(t *testing.T) {
	waitForAppliedChecksum(t, createGateway(t, testNamespace(t), "roots"))

	// A suite that records no Kubernetes client span would pass the loop below
	// without checking anything.
	eventually(t, func() error {
		for _, span := range suiteTraces.Ended() {
			if strings.HasPrefix(span.Name(), "k8s ") && span.Parent().IsValid() {
				return nil
			}
		}
		return fmt.Errorf("no Kubernetes client span was recorded")
	})

	for _, span := range suiteTraces.Ended() {
		if strings.HasPrefix(span.Name(), "k8s ") && !span.Parent().IsValid() {
			t.Errorf("Kubernetes client span %q has no parent: background traffic must not start traces", span.Name())
		}
	}
}
