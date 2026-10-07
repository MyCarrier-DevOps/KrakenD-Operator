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
	"bytes"
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
)

// cleanOTelEnv empties every OTEL_* variable Setup reads, so the developer's
// or the CI runner's environment cannot change a test's outcome.
func cleanOTelEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
		"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER",
		"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
	} {
		t.Setenv(name, "")
	}
}

func setup(t *testing.T, out *bytes.Buffer) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		ServiceVersion: "1.2.3", PodName: "op-0", PodNamespace: "krakend-system",
		LogLevel: otellog.SeverityInfo, LogFormat: telemetry.LogFormatJSON,
		Stdout: out, Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

func TestSetup_WithoutAnEndpointNothingIsExported(t *testing.T) {
	cleanOTelEnv(t)

	tel := setup(t, &bytes.Buffer{})

	if _, ok := tel.TracerProvider.(noop.TracerProvider); !ok {
		t.Errorf("TracerProvider = %T, want the no-op provider", tel.TracerProvider)
	}
}
