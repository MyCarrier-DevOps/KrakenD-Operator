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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
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

// OperatorMetrics records the operator's metrics on OpenTelemetry
// instruments.
type OperatorMetrics struct {
	renders    metric.Int64Counter
	rejections metric.Int64Counter
	restarts   metric.Int64Counter
}

// NewOperatorMetrics creates the operator's instruments on meter.
func NewOperatorMetrics(meter metric.Meter) (*OperatorMetrics, error) {
	m := &OperatorMetrics{}
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
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	ctx := context.Background()
	for _, c := range []metric.Int64Counter{m.renders, m.rejections, m.restarts} {
		c.Add(ctx, 0)
	}
	return m, nil
}

// SetEndpoints is a stub.
func (m *OperatorMetrics) SetEndpoints(types.NamespacedName, int) {}

// SetGatewayInfo is a stub.
func (m *OperatorMetrics) SetGatewayInfo(types.NamespacedName, string, string) {}

// SetConfigValid is a stub.
func (m *OperatorMetrics) SetConfigValid(types.NamespacedName, bool) {}

// SetDragonflyReady is a stub.
func (m *OperatorMetrics) SetDragonflyReady(types.NamespacedName, bool) {}

// SetLicenseExpiry is a stub.
func (m *OperatorMetrics) SetLicenseExpiry(types.NamespacedName, time.Duration) {}

// SetExcludedEndpoints is a stub.
func (m *OperatorMetrics) SetExcludedEndpoints(types.NamespacedName, map[string]int) {}

// ForgetGateway is a stub.
func (m *OperatorMetrics) ForgetGateway(types.NamespacedName) {}
