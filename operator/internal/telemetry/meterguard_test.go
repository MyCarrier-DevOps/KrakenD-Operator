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

package telemetry_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// This test sets the OTel global meter provider, which no other test or
// production code does, and so it never runs in parallel. The guard it pins
// (a no-op meter provider passed to otelhttp) can only be observed through
// the global fallback: with the option missing, otelhttp records its HTTP
// metrics on whatever global provider is set. The previous provider is
// restored when the test ends.
func TestTraceKubeAPIAndWebhookServer_RecordNoHTTPMetricOnTheGlobalProvider(t *testing.T) {
	reg := prometheus.NewRegistry()
	reader, err := telemetry.NewPrometheusReader(reg)
	if err != nil {
		t.Fatal(err)
	}
	provider := telemetry.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	rec := tracingtest.New(t)

	serveOnce(t, rec, "")
	ctx, span := rec.Tracer().Start(context.Background(), "reconcile")
	c, _ := tracedClient(t, rec)
	if err := updateConfigMap(ctx, c); err != nil {
		t.Fatal(err)
	}
	span.End()

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "http_") {
			t.Errorf("metric family %q is exposed, want no http_ family", family.GetName())
		}
	}
}
