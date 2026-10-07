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

package telemetry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceName is the service.name the operator reports unless
// OTEL_SERVICE_NAME or OTEL_RESOURCE_ATTRIBUTES set another.
const ServiceName = "krakend-operator"

// The OTLP protocols the exporters speak.
const (
	protocolGRPC = "grpc"
	protocolHTTP = "http/protobuf"
)

// Config is what Setup takes besides the OTEL_* environment.
type Config struct {
	// ServiceVersion is the operator's version, reported as service.version.
	ServiceVersion string
	// PodName and PodNamespace, when set, are reported as k8s.pod.name and
	// k8s.namespace.name.
	PodName, PodNamespace string
	// LogLevel is the lowest severity logged; LogFormat is how stdout gets it.
	LogLevel  otellog.Severity
	LogFormat LogFormat
	// Stdout receives every log record. Nil means os.Stdout.
	Stdout io.Writer
	// Registerer is where the /metrics exporter registers: controller-runtime's
	// metrics.Registry in the operator.
	Registerer prometheus.Registerer
}

// Telemetry holds the operator's providers.
type Telemetry struct {
	// TracerProvider is a no-op provider unless traces are exported.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	// Logger logs through every log exporter; Diagnostics through stdout only.
	Logger      logr.Logger
	Diagnostics logr.Logger
	// Warning is a configuration problem Setup worked around, to report once
	// logging is installed: a malformed OTEL_RESOURCE_ATTRIBUTES entry.
	Warning  error
	shutdown []func(context.Context) error
}

// Setup builds the providers. Each signal is exported over OTLP only when an
// endpoint is configured for it (OTEL_EXPORTER_OTLP_ENDPOINT or the signal's
// own OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT) and OTEL_<SIGNAL>_EXPORTER is not
// none. Metrics are always served on /metrics and logs always written to
// stdout. Without OTLP export, traces use a no-op provider.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	res, err := newResource(ctx, cfg)
	var warning error
	if errors.Is(err, resource.ErrPartialResource) {
		// A malformed entry is left out; the rest still describes the operator.
		warning, err = err, nil
	}
	if err != nil {
		return nil, err
	}
	t := &Telemetry{Warning: warning}
	if err := t.setupTraces(ctx, res); err != nil {
		return nil, err
	}
	if err := t.setupMetrics(ctx, res, cfg.Registerer); err != nil {
		return nil, errors.Join(err, t.Shutdown(ctx))
	}
	if err := t.setupLogs(ctx, res, cfg); err != nil {
		return nil, errors.Join(err, t.Shutdown(ctx))
	}
	return t, nil
}

// Shutdown flushes and stops the providers: traces, then metrics, then logs,
// so the last records logged by the others still go out.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, stop := range t.shutdown {
		errs = append(errs, stop(ctx))
	}
	t.shutdown = nil
	return errors.Join(errs...)
}

func (t *Telemetry) setupTraces(ctx context.Context, res *resource.Resource) error {
	protocol, err := otlpProtocol("TRACES")
	if err != nil || protocol == "" {
		t.TracerProvider = noop.NewTracerProvider()
		return err
	}
	var exporter sdktrace.SpanExporter
	if protocol == protocolGRPC {
		exporter, err = otlptracegrpc.New(ctx)
	} else {
		exporter, err = otlptracehttp.New(ctx)
	}
	if err != nil {
		return fmt.Errorf("creating the OTLP trace exporter: %w", err)
	}
	// The sampler comes from OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG,
	// parent-based always-on by default.
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
	t.TracerProvider = tp
	t.shutdown = append(t.shutdown, tp.Shutdown)
	return nil
}

func (t *Telemetry) setupMetrics(ctx context.Context, res *resource.Resource, reg prometheus.Registerer) error {
	prom, err := NewPrometheusReader(reg)
	if err != nil {
		return fmt.Errorf("creating the Prometheus exporter: %w", err)
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithReader(prom)}
	protocol, err := otlpProtocol("METRICS")
	if err != nil {
		return err
	}
	if protocol != "" {
		var exporter sdkmetric.Exporter
		if protocol == protocolGRPC {
			exporter, err = otlpmetricgrpc.New(ctx)
		} else {
			exporter, err = otlpmetrichttp.New(ctx)
		}
		if err != nil {
			return fmt.Errorf("creating the OTLP metric exporter: %w", err)
		}
		// The interval comes from OTEL_METRIC_EXPORT_INTERVAL, 60s by default.
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	}
	mp := NewMeterProvider(opts...)
	t.MeterProvider = mp
	t.shutdown = append(t.shutdown, mp.Shutdown)
	return nil
}

func (t *Telemetry) setupLogs(ctx context.Context, res *resource.Resource, cfg Config) error {
	stdout, err := NewStdoutProcessor(cmp.Or[io.Writer](cfg.Stdout, os.Stdout), cmp.Or(cfg.LogFormat, LogFormatJSON))
	if err != nil {
		return err
	}
	stdout = WithMinSeverity(stdout, cfg.LogLevel)
	opts := []sdklog.LoggerProviderOption{sdklog.WithResource(res), sdklog.WithProcessor(stdout)}
	protocol, err := otlpProtocol("LOGS")
	if err != nil {
		return err
	}
	if protocol != "" {
		var exporter sdklog.Exporter
		if protocol == protocolGRPC {
			exporter, err = otlploggrpc.New(ctx)
		} else {
			exporter, err = otlploghttp.New(ctx)
		}
		if err != nil {
			return fmt.Errorf("creating the OTLP log exporter: %w", err)
		}
		opts = append(opts, sdklog.WithProcessor(WithMinSeverity(sdklog.NewBatchProcessor(exporter), cfg.LogLevel)))
	}
	lp := sdklog.NewLoggerProvider(opts...)
	t.Logger = NewLogger(lp, ServiceName)
	// The OpenTelemetry SDK reports its own failures, an OTLP export that
	// failed among them, through Diagnostics: logging them through Logger would
	// queue a record for the exporter that just failed.
	diagnostics := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(stdout))
	t.Diagnostics = NewLogger(diagnostics, "opentelemetry")
	t.shutdown = append(t.shutdown, lp.Shutdown)
	return nil
}

// otlpProtocol returns the OTLP protocol signal (TRACES, METRICS or LOGS) is
// exported with, or "" when it is not exported.
func otlpProtocol(signal string) (string, error) {
	switch exporter := os.Getenv("OTEL_" + signal + "_EXPORTER"); exporter {
	case "none":
		return "", nil
	case "", "otlp":
	default:
		return "", fmt.Errorf("OTEL_%s_EXPORTER=%q is not supported: want otlp or none", signal, exporter)
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") == "" {
		return "", nil
	}
	protocol := cmp.Or(os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_PROTOCOL"),
		os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"), protocolHTTP)
	if protocol != protocolGRPC && protocol != protocolHTTP {
		return "", fmt.Errorf("OTLP protocol %q is not supported for %s: want %s or %s",
			protocol, strings.ToLower(signal), protocolGRPC, protocolHTTP)
	}
	return protocol, nil
}

// newResource describes the operator process. OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES override the defaults set here.
func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.PodName != "" {
		attrs = append(attrs, semconv.K8SPodName(cfg.PodName))
	}
	if cfg.PodNamespace != "" {
		attrs = append(attrs, semconv.K8SNamespaceName(cfg.PodNamespace))
	}
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(),
	)
	if err != nil {
		// A partial resource comes back with its error; Setup decides.
		return res, fmt.Errorf("building the telemetry resource: %w", err)
	}
	return res, nil
}
