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

// Package telemetry wires the OpenTelemetry SDK: the tracer, meter and logger
// providers built from the standard OTEL_* environment, the Prometheus
// exporter behind /metrics, the log pipeline every component logs through,
// and the instrumentation of the Kubernetes client, the webhook server and
// the krakend executions. Only cmd uses it; the rest of the operator depends
// on the OpenTelemetry API alone.
package telemetry

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/types"
)

// NewPrometheusReader returns the metric reader that serves the operator's
// metrics on /metrics through reg. Instrument names are exported verbatim:
// no unit or _total suffix is added, no otel_scope_* label and no target_info
// series, so every name and label set stays what it was before the
// OpenTelemetry migration.
func NewPrometheusReader(reg prometheus.Registerer) (*otelprom.Exporter, error) {
	return otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes),
		otelprom.WithoutScopeInfo(),
		otelprom.WithoutTargetInfo(),
	)
}

// NewMeterProvider returns the meter provider every operator metric is
// recorded through, with opts and no cardinality limit. The SDK's default
// keeps 2,000 series per instrument and folds the rest into one
// otel_metric_overflow series; the reconcile-duration histogram keeps a
// deleted gateway's series until the operator restarts, so a long-lived
// operator would pass that limit and its newest gateways would lose their
// own series.
func NewMeterProvider(opts ...sdkmetric.Option) *sdkmetric.MeterProvider {
	return sdkmetric.NewMeterProvider(append(opts, sdkmetric.WithCardinalityLimit(0))...)
}

// OperatorMetrics records the operator's metrics on OpenTelemetry
// instruments. Counters and the reconcile histogram are synchronous. Every
// gauge is observable: it reports the values held here when the metrics are
// collected, so a forgotten gateway's or AutoConfig's series disappears.
type OperatorMetrics struct {
	renders    metric.Int64Counter
	rejections metric.Int64Counter
	restarts   metric.Int64Counter
	duration   metric.Float64Histogram

	mu            sync.Mutex
	licenseExpiry map[types.NamespacedName]float64
	endpoints     map[types.NamespacedName]float64
	dragonfly     map[types.NamespacedName]float64
	configValid   map[types.NamespacedName]float64
	info          map[types.NamespacedName]gatewayInfo
	excluded      map[types.NamespacedName]map[string]int
	synced        map[types.NamespacedName]float64
}

// gatewayInfo is the edition and version a gateway_info series carries.
type gatewayInfo struct{ edition, version string }

// NewOperatorMetrics creates the operator's instruments on meter. The
// unlabelled counters are added to once with zero, so they are exported from
// startup as they were before.
func NewOperatorMetrics(meter metric.Meter) (*OperatorMetrics, error) {
	m := &OperatorMetrics{
		licenseExpiry: map[types.NamespacedName]float64{},
		endpoints:     map[types.NamespacedName]float64{},
		dragonfly:     map[types.NamespacedName]float64{},
		configValid:   map[types.NamespacedName]float64{},
		info:          map[types.NamespacedName]gatewayInfo{},
		excluded:      map[types.NamespacedName]map[string]int{},
		synced:        map[types.NamespacedName]float64{},
	}
	var errs []error
	counter := func(name, help string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(help))
		errs = append(errs, err)
		return c
	}
	m.renders = counter("krakend_operator_config_renders_total", "Total config render attempts")
	m.rejections = counter("krakend_operator_config_validation_failures_total",
		"Fresh rejections of a gateway root, a backend policy or an endpoint checked on its own, "+
			"counted once per change of what the gateway controller checks, not once per reconcile; "+
			"content that comes back, the same policy on another gateway and an operator restart each count again")
	m.restarts = counter("krakend_operator_rolling_restarts_total",
		"Deployment writes that changed the pod template, rolling the pods (not creations)")
	var err error
	m.duration, err = meter.Float64Histogram("krakend_operator_reconcile_duration_seconds",
		metric.WithDescription("Reconciliation loop latency"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(prometheus.DefBuckets...))
	errs = append(errs, err, m.registerGauges(meter))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	ctx := context.Background()
	for _, c := range []metric.Int64Counter{m.renders, m.rejections, m.restarts} {
		c.Add(ctx, 0)
	}
	return m, nil
}

// ConfigRendered counts a config render.
func (m *OperatorMetrics) ConfigRendered(ctx context.Context) { m.renders.Add(ctx, 1) }

// ConfigRejected counts a rejected config.
func (m *OperatorMetrics) ConfigRejected(ctx context.Context) { m.rejections.Add(ctx, 1) }

// RollingRestart counts a Deployment write that rolled the pods.
func (m *OperatorMetrics) RollingRestart(ctx context.Context) { m.restarts.Add(ctx, 1) }

// GatewayReconciled records how long a gateway reconcile took. OpenTelemetry
// synchronous instruments cannot remove a series, so a deleted gateway's
// duration series stays until the operator restarts.
func (m *OperatorMetrics) GatewayReconciled(ctx context.Context, gateway types.NamespacedName, d time.Duration) {
	m.duration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("controller", "gateway"),
		attribute.String("namespace", gateway.Namespace),
		attribute.String("name", gateway.Name)))
}

// SetLicenseExpiry sets the time left until the gateway's license expires.
func (m *OperatorMetrics) SetLicenseExpiry(gateway types.NamespacedName, left time.Duration) {
	m.set(m.licenseExpiry, gateway, left.Seconds())
}

// ForgetLicenseExpiry removes the gateway's license expiry series.
func (m *OperatorMetrics) ForgetLicenseExpiry(gateway types.NamespacedName) {
	m.forget(m.licenseExpiry, gateway)
}

// SetEndpoints sets how many KrakenDEndpoints reference the gateway.
func (m *OperatorMetrics) SetEndpoints(gateway types.NamespacedName, n int) {
	m.set(m.endpoints, gateway, float64(n))
}

// SetDragonflyReady sets whether the gateway's Dragonfly is ready.
func (m *OperatorMetrics) SetDragonflyReady(gateway types.NamespacedName, ready bool) {
	m.set(m.dragonfly, gateway, oneIf(ready))
}

// ForgetDragonflyReady removes the gateway's Dragonfly series.
func (m *OperatorMetrics) ForgetDragonflyReady(gateway types.NamespacedName) {
	m.forget(m.dragonfly, gateway)
}

// SetConfigValid sets whether the gateway's newest config passed validation.
func (m *OperatorMetrics) SetConfigValid(gateway types.NamespacedName, valid bool) {
	m.set(m.configValid, gateway, oneIf(valid))
}

// SetGatewayInfo replaces the gateway's gateway_info series.
func (m *OperatorMetrics) SetGatewayInfo(gateway types.NamespacedName, edition, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.info[gateway] = gatewayInfo{edition: edition, version: version}
}

// SetExcludedEndpoints replaces the gateway's excluded-endpoint counts, by
// reason. A reason left out, or a zero count, has no series.
func (m *OperatorMetrics) SetExcludedEndpoints(gateway types.NamespacedName, byReason map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := map[string]int{}
	for reason, n := range byReason {
		if n > 0 {
			counts[reason] = n
		}
	}
	if len(counts) == 0 {
		delete(m.excluded, gateway)
		return
	}
	m.excluded[gateway] = counts
}

// ForgetGateway removes every gauge series of the gateway.
func (m *OperatorMetrics) ForgetGateway(gateway types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, values := range []map[types.NamespacedName]float64{m.licenseExpiry, m.endpoints, m.dragonfly, m.configValid} {
		delete(values, gateway)
	}
	delete(m.info, gateway)
	delete(m.excluded, gateway)
}

// SetAutoConfigSynced sets whether the AutoConfig's last reconcile synced.
func (m *OperatorMetrics) SetAutoConfigSynced(autoConfig types.NamespacedName, synced bool) {
	m.set(m.synced, autoConfig, oneIf(synced))
}

// ForgetAutoConfig removes the AutoConfig's series.
func (m *OperatorMetrics) ForgetAutoConfig(autoConfig types.NamespacedName) {
	m.forget(m.synced, autoConfig)
}

func (m *OperatorMetrics) registerGauges(meter metric.Meter) error {
	gauge := func(name, help string) (metric.Float64ObservableGauge, error) {
		return meter.Float64ObservableGauge(name, metric.WithDescription(help))
	}
	license, err1 := gauge("krakend_operator_license_expiry_seconds", "Seconds until EE license expiry")
	endpoints, err2 := gauge("krakend_operator_endpoints", "Number of KrakenDEndpoints per gateway")
	dragonfly, err3 := gauge("krakend_operator_dragonfly_ready", "1 if Dragonfly is ready, 0 otherwise")
	info, err4 := gauge("krakend_operator_gateway_info", "Gateway metadata labels")
	valid, err5 := gauge("krakend_operator_gateway_config_valid",
		"1 while the gateway's newest rendered config passed validation (ConfigValid=True), 0 otherwise")
	synced, err6 := gauge("krakend_operator_autoconfig_synced",
		"1 if the KrakenDAutoConfig's last reconcile synced successfully, 0 if it is failing")
	excluded, err7 := gauge("krakend_operator_gateway_excluded_endpoints",
		"KrakenDEndpoints a gateway leaves out of its config because they fail validation on their own, "+
			"by Accepted reason")
	if err := errors.Join(err1, err2, err3, err4, err5, err6, err7); err != nil {
		return err
	}
	_, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		observeByName(o, license, m.licenseExpiry)
		observeByName(o, endpoints, m.endpoints)
		observeByName(o, dragonfly, m.dragonfly)
		observeByName(o, valid, m.configValid)
		observeByName(o, synced, m.synced)
		for key, i := range m.info {
			o.ObserveFloat64(info, 1, metric.WithAttributes(nameAttrs(key,
				attribute.String("edition", i.edition), attribute.String("version", i.version))...))
		}
		for key, counts := range m.excluded {
			for reason, n := range counts {
				o.ObserveFloat64(excluded, float64(n), metric.WithAttributes(
					attribute.String("namespace", key.Namespace), attribute.String("gateway", key.Name),
					attribute.String("reason", reason)))
			}
		}
		return nil
	}, license, endpoints, dragonfly, info, valid, synced, excluded)
	return err
}

func (m *OperatorMetrics) set(values map[types.NamespacedName]float64, key types.NamespacedName, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	values[key] = v
}

func (m *OperatorMetrics) forget(values map[types.NamespacedName]float64, key types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(values, key)
}

// observeByName reports each value with namespace and name labels.
func observeByName(o metric.Observer, g metric.Float64ObservableGauge, values map[types.NamespacedName]float64) {
	for key, v := range values {
		o.ObserveFloat64(g, v, metric.WithAttributes(nameAttrs(key)...))
	}
}

func nameAttrs(key types.NamespacedName, extra ...attribute.KeyValue) []attribute.KeyValue {
	return append([]attribute.KeyValue{
		attribute.String("namespace", key.Namespace), attribute.String("name", key.Name),
	}, extra...)
}

func oneIf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
