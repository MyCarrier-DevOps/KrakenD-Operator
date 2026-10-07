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
	"maps"
	"net"
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
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

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

// collector records the calls an OTLP client makes to a test server: the URL
// path for http/protobuf, the full method name for gRPC.
type collector struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *collector) record(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[key]++
}

func (c *collector) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[key]
}

func (c *collector) all() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.calls)
}

// newHTTPCollector serves OTLP over http/protobuf and returns its URL.
func newHTTPCollector(t *testing.T) (*collector, string) {
	t.Helper()
	c := &collector{}
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			c.record(r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return c, server.URL
}

// newGRPCCollector serves any gRPC method, answering with an empty message,
// and returns its URL. An http:// scheme makes the OTLP exporter insecure.
func newGRPCCollector(t *testing.T) (*collector, string) {
	t.Helper()
	c := &collector{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		_ = stream.RecvMsg(&emptypb.Empty{})
		c.record(method)
		return stream.SendMsg(&emptypb.Empty{})
	}))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return c, "http://" + listener.Addr().String()
}

// otlpSignal is one of the three signals Setup exports, with where an OTLP
// client delivers it and a way to produce one.
type otlpSignal struct {
	name, httpPath, grpcMethod string
	emit                       func(t *testing.T, tel *telemetry.Telemetry)
}

var otlpSignals = []otlpSignal{
	{"TRACES", "/v1/traces", "/opentelemetry.proto.collector.trace.v1.TraceService/Export",
		func(_ *testing.T, tel *telemetry.Telemetry) {
			_, span := tel.TracerProvider.Tracer("test").Start(context.Background(), "exported")
			span.End()
		}},
	{"METRICS", "/v1/metrics", "/opentelemetry.proto.collector.metrics.v1.MetricsService/Export",
		func(t *testing.T, tel *telemetry.Telemetry) {
			counter, err := tel.MeterProvider.Meter("test").Int64Counter("exported_total")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(context.Background(), 1)
		}},
	{"LOGS", "/v1/logs", "/opentelemetry.proto.collector.logs.v1.LogsService/Export",
		func(_ *testing.T, tel *telemetry.Telemetry) { tel.Logger.Info("exported") }},
}

func TestSetup_WithAnEndpointTracesAreRecorded(t *testing.T) {
	cleanOTelEnv(t)
	_, url := newHTTPCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
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
	collected, url := newHTTPCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	tel := setup(t, &bytes.Buffer{})

	for _, signal := range otlpSignals {
		signal.emit(t, tel)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, signal := range otlpSignals {
		if collected.count(signal.httpPath) == 0 {
			t.Errorf("no POST to %s; the collector got %v", signal.httpPath, collected.all())
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

// An endpoint with a password in it that the exporters cannot read must not be
// printed either: they log the value they failed to parse. The signal is not
// exported, and the warning names the variable, never its value.
func TestSetup_AMalformedEndpointNeverReachesTheOutput(t *testing.T) {
	const secret = "pw-s3cret"
	for _, tc := range []struct{ name, value string }{
		{"an invalid escape", "http://user:" + secret + "@127.0.0.1:1/%zz"},
		{"a leading space", " http://user:" + secret + "@127.0.0.1:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanOTelEnv(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.value)
			diagnostics := captureOTelDiagnostics(t)
			var out bytes.Buffer

			tel := setup(t, &out)
			tel.Logger.Info("started")

			for name, written := range map[string]string{
				"the OpenTelemetry diagnostics": diagnostics.String(), "stdout": out.String(),
			} {
				if strings.Contains(written, secret) {
					t.Errorf("%s carry the endpoint's password:\n%s", name, written)
				}
			}
			if tel.Warning == nil || !strings.Contains(tel.Warning.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT") ||
				strings.Contains(tel.Warning.Error(), secret) {
				t.Errorf("Warning = %v, want the variable named without its value", tel.Warning)
			}
			if _, ok := tel.TracerProvider.(noop.TracerProvider); !ok {
				t.Errorf("TracerProvider = %T, want the no-op provider", tel.TracerProvider)
			}
		})
	}
}

// OTEL_<SIGNAL>_EXPORTER=none turns that signal's OTLP export off even with an
// endpoint configured, and leaves the other signals exported.
func TestSetup_ExporterNoneDisablesOnlyThatSignal(t *testing.T) {
	for _, off := range otlpSignals {
		t.Run(off.name, func(t *testing.T) {
			cleanOTelEnv(t)
			collected, url := newHTTPCollector(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
			t.Setenv("OTEL_"+off.name+"_EXPORTER", "none")
			tel := setup(t, &bytes.Buffer{})

			for _, signal := range otlpSignals {
				signal.emit(t, tel)
			}
			if err := tel.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}

			for _, signal := range otlpSignals {
				if got := collected.count(signal.httpPath); (got > 0) == (signal.name == off.name) {
					t.Errorf("%d POSTs to %s with %s off; the collector got %v",
						got, signal.httpPath, off.name, collected.all())
				}
			}
		})
	}
}

// An exporter other than otlp or none is a configuration the operator cannot
// honour: Setup fails and names the variable rather than ignore it.
func TestSetup_RejectsAnUnsupportedExporter(t *testing.T) {
	for _, signal := range otlpSignals {
		t.Run(signal.name, func(t *testing.T) {
			cleanOTelEnv(t)
			variable := "OTEL_" + signal.name + "_EXPORTER"
			t.Setenv(variable, "jaeger")

			_, err := telemetry.Setup(context.Background(), telemetry.Config{Registerer: prometheus.NewRegistry()})

			if err == nil || !strings.Contains(err.Error(), variable) {
				t.Errorf("err = %v, want %s named", err, variable)
			}
		})
	}
}

// The protocol a signal is exported with is its own OTEL_EXPORTER_OTLP_<SIGNAL>_PROTOCOL,
// else OTEL_EXPORTER_OTLP_PROTOCOL, else http/protobuf. Each signal is pointed
// at the collector that speaks the protocol the variables select, and only that
// collector receives it.
func TestSetup_ProtocolIsTheSignalsThenTheGenericThenHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, generic, own string
		grpc               bool
	}{
		{"neither set", "", "", false},
		{"generic grpc", "grpc", "", true},
		{"signal http over generic grpc", "grpc", "http/protobuf", false},
		{"signal grpc over generic http", "http/protobuf", "grpc", true},
	} {
		for _, signal := range otlpSignals {
			t.Run(signal.name+"/"+tc.name, func(t *testing.T) {
				cleanOTelEnv(t)
				httpCollected, httpURL := newHTTPCollector(t)
				grpcCollected, grpcURL := newGRPCCollector(t)
				for _, other := range otlpSignals {
					if other.name != signal.name {
						t.Setenv("OTEL_"+other.name+"_EXPORTER", "none")
					}
				}
				t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tc.generic)
				t.Setenv("OTEL_EXPORTER_OTLP_"+signal.name+"_PROTOCOL", tc.own)
				want, wantKey, other, otherKey := httpCollected, signal.httpPath, grpcCollected, signal.grpcMethod
				// A signal's own endpoint is used as given, path included.
				endpoint := httpURL + signal.httpPath
				if tc.grpc {
					want, wantKey, other, otherKey = grpcCollected, signal.grpcMethod, httpCollected, signal.httpPath
					endpoint = grpcURL
				}
				t.Setenv("OTEL_EXPORTER_OTLP_"+signal.name+"_ENDPOINT", endpoint)
				tel := setup(t, &bytes.Buffer{})

				signal.emit(t, tel)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = tel.Shutdown(ctx)

				if want.count(wantKey) == 0 {
					t.Errorf("%s never reached %s; got %v", signal.name, wantKey, want.all())
				}
				if other.count(otherKey) != 0 {
					t.Errorf("%s reached %s, the other protocol", signal.name, otherKey)
				}
			})
		}
	}
}

// A signal's own endpoint, with no generic one, turns that signal's export on
// and no other.
func TestSetup_ASignalEndpointAloneEnablesThatSignal(t *testing.T) {
	for _, on := range otlpSignals {
		t.Run(on.name, func(t *testing.T) {
			cleanOTelEnv(t)
			collected, url := newHTTPCollector(t)
			t.Setenv("OTEL_EXPORTER_OTLP_"+on.name+"_ENDPOINT", url+on.httpPath)
			tel := setup(t, &bytes.Buffer{})

			for _, signal := range otlpSignals {
				signal.emit(t, tel)
			}
			if err := tel.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}

			if got := collected.all(); len(got) != 1 || got[on.httpPath] == 0 {
				t.Errorf("the collector got %v, want only %s", got, on.httpPath)
			}
		})
	}
}

// The SDK's own diagnostics go to stdout only. Sent through the OTLP log
// exporter, a failing export would report itself into the exporter that failed.
func TestSetup_DiagnosticsAreNeverExportedOverOTLP(t *testing.T) {
	cleanOTelEnv(t)
	collected, url := newHTTPCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", url)
	var out bytes.Buffer
	tel := setup(t, &out)

	tel.Diagnostics.Info("export failed")
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "export failed") {
		t.Errorf("stdout lacks the diagnostic:\n%s", out.String())
	}
	if n := collected.count("/v1/logs"); n != 0 {
		t.Errorf("%d log POSTs carried the diagnostic; the collector got %v", n, collected.all())
	}
}
