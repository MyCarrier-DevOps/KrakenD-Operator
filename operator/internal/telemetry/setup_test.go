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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
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
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
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

func TestSetup_ResourceNamesTheOperatorItsVersionAndPod(t *testing.T) {
	cleanOTelEnv(t)
	var out bytes.Buffer
	tel := setup(t, &out)

	tel.Logger.Info("hello")

	got := out.String()
	for _, want := range []string{
		`{"Key":"service.name","Value":{"Type":"STRING","Value":"krakend-operator"}}`,
		`{"Key":"service.version","Value":{"Type":"STRING","Value":"1.2.3"}}`,
		`{"Key":"k8s.pod.name","Value":{"Type":"STRING","Value":"op-0"}}`,
		`{"Key":"k8s.namespace.name","Value":{"Type":"STRING","Value":"krakend-system"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("record lacks %s:\n%s", want, got)
		}
	}
}

func TestSetup_ServiceNameFromTheEnvironmentWins(t *testing.T) {
	cleanOTelEnv(t)
	t.Setenv("OTEL_SERVICE_NAME", "operator-staging")
	var out bytes.Buffer
	tel := setup(t, &out)

	tel.Logger.Info("hello")

	if want := `"Value":"operator-staging"`; !strings.Contains(out.String(), want) {
		t.Errorf("record lacks %s:\n%s", want, out.String())
	}
}

func TestSetup_RejectsAnUnsupportedProtocol(t *testing.T) {
	cleanOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")

	_, err := telemetry.Setup(context.Background(), telemetry.Config{Registerer: prometheus.NewRegistry()})

	if err == nil || !strings.Contains(err.Error(), "http/json") {
		t.Errorf("err = %v, want the unsupported protocol named", err)
	}
}

func TestSetup_WithAnEndpointTracesAreRecorded(t *testing.T) {
	cleanOTelEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")

	tel := setup(t, &bytes.Buffer{})

	_, span := tel.TracerProvider.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	if !span.IsRecording() {
		t.Error("span is not recording with an OTLP endpoint configured")
	}
}

// With an OTLP endpoint, every signal reaches the collector: a span, a
// metric and a log record are each posted to their http/protobuf path by the
// time Shutdown returns.
func TestSetup_ExportsEverySignalOverOTLP(t *testing.T) {
	cleanOTelEnv(t)
	var mu sync.Mutex
	posts := map[string]int{}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			posts[r.URL.Path]++
		}
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	var out bytes.Buffer
	tel := setup(t, &out)
	ctx := context.Background()

	_, span := tel.TracerProvider.Tracer("test").Start(ctx, "exported")
	span.End()
	counter, err := tel.MeterProvider.Meter("test").Int64Counter("exported_total")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 1)
	tel.Logger.Info("exported")
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		if posts[path] == 0 {
			t.Errorf("no POST to %s; the collector got %v", path, posts)
		}
	}
}

// A collector that accepts connections and never answers must not hold the
// operator's shutdown past its deadline.
func TestSetup_ShutdownWithAnUnreachableCollectorReturnsByTheDeadline(t *testing.T) {
	cleanOTelEnv(t)
	hung := make(chan struct{})
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hung }))
	defer collector.Close()
	defer close(hung)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		LogLevel: otellog.SeverityInfo, Stdout: &bytes.Buffer{}, Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, span := tel.TracerProvider.Tracer("t").Start(context.Background(), "pending")
	span.End()
	tel.Logger.Info("pending")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_ = tel.Shutdown(ctx)

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Shutdown took %v with a 2s deadline", elapsed)
	}
}

// A value the chart passes through can be malformed; the operator must still
// start, without that entry, and say so.
func TestSetup_AMalformedResourceAttributeIsAWarning(t *testing.T) {
	cleanOTelEnv(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "bad,team=platform")
	var out bytes.Buffer

	tel := setup(t, &out)
	tel.Logger.Info("hello")

	if tel.Warning == nil || !strings.Contains(tel.Warning.Error(), "bad") {
		t.Errorf("Warning = %v, want the malformed entry named", tel.Warning)
	}
	for _, want := range []string{`"Value":"krakend-operator"`, `{"Key":"team","Value":{"Type":"STRING","Value":"platform"}}`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("record lacks %s:\n%s", want, out.String())
		}
	}
}

// captureOTelDiagnostics installs an OpenTelemetry logger and error handler
// that write into the returned buffer, so what the SDK reports while Setup
// builds the exporters can be inspected. It restores quiet ones when t ends.
// Whatever the SDK would write to os.Stderr before this is installed is not
// captured: its default logger holds the original file from init.
func captureOTelDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	var diagnostics bytes.Buffer
	otel.SetLogger(funcr.New(func(prefix, args string) { diagnostics.WriteString(prefix + " " + args + "\n") },
		funcr.Options{Verbosity: 8}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { diagnostics.WriteString(err.Error() + "\n") }))
	t.Cleanup(func() {
		otel.SetLogger(logr.Discard())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	})
	return &diagnostics
}

// A collector credential written as "Authorization: Bearer <token>" instead
// of name=value must not be printed: the OTLP exporters log a header they
// cannot read with its value. The signal is not exported, and the warning
// names the variable, never its value.
func TestSetup_AMalformedHeaderNeverReachesTheOutput(t *testing.T) {
	cleanOTelEnv(t)
	const secret = "s3cr3t-token"
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization: Bearer "+secret)
	diagnostics := captureOTelDiagnostics(t)
	var out bytes.Buffer

	tel := setup(t, &out)
	tel.Logger.Info("started")

	for name, written := range map[string]string{
		"the OpenTelemetry diagnostics": diagnostics.String(), "stdout": out.String(),
	} {
		if strings.Contains(written, secret) {
			t.Errorf("%s carry the header's value:\n%s", name, written)
		}
	}
	if tel.Warning == nil || !strings.Contains(tel.Warning.Error(), "OTEL_EXPORTER_OTLP_HEADERS") ||
		strings.Contains(tel.Warning.Error(), secret) {
		t.Errorf("Warning = %v, want the variable named without its value", tel.Warning)
	}
	if _, ok := tel.TracerProvider.(noop.TracerProvider); !ok {
		t.Errorf("TracerProvider = %T, want the no-op provider: traces are not exported with headers it cannot read",
			tel.TracerProvider)
	}
}

// Setup reads the header and endpoint variables as the OTLP exporters do: a
// list they accept keeps the signal exported, and every form they reject
// (and would log with its value) keeps it from being exported.
func TestSetup_ReadsTheHeaderAndEndpointVariablesAsTheExportersDo(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value string
		exported              bool
	}{
		{"a name=value list with an encoded value", "OTEL_EXPORTER_OTLP_HEADERS", "api-key=abc, x-team=a%20b", true},
		{"an entry without =", "OTEL_EXPORTER_OTLP_HEADERS", "api-key=abc,Bearer xyz", false},
		{"a name that is not a token", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "api key=abc", false},
		{"a value that is not URL-encoded", "OTEL_EXPORTER_OTLP_HEADERS", "api-key=100%", false},
		{"an endpoint that is not a URL", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://collector:4318/%zz", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanOTelEnv(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			t.Setenv(tc.variable, tc.value)

			tel := setup(t, &bytes.Buffer{})

			_, noTraces := tel.TracerProvider.(noop.TracerProvider)
			if noTraces == tc.exported || (tel.Warning != nil) == tc.exported {
				t.Errorf("traces exported = %v, warning = %v; want exported %v", !noTraces, tel.Warning, tc.exported)
			}
		})
	}
}
