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

package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

var (
	configRenders = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "krakend_operator_config_renders_total",
		Help: "Total config render attempts",
	})

	configValidationFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "krakend_operator_config_validation_failures_total",
		Help: "Fresh rejections of a gateway root, a backend policy or an endpoint checked on its own, " +
			"counted once per change of what the gateway controller checks, not once per reconcile; " +
			"content that comes back, the same policy on another gateway and an operator restart each count again",
	})

	rollingRestarts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "krakend_operator_rolling_restarts_total",
		Help: "Deployment writes that changed the pod template, rolling the pods (not creations)",
	})

	licenseExpirySeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "krakend_operator_license_expiry_seconds",
		Help: "Seconds until EE license expiry",
	}, []string{"namespace", "name"})

	endpointsPerGateway = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "krakend_operator_endpoints",
		Help: "Number of KrakenDEndpoints per gateway",
	}, []string{"namespace", "name"})

	reconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "krakend_operator_reconcile_duration_seconds",
		Help:    "Reconciliation loop latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"controller", "namespace", "name"})

	dragonflyReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "krakend_operator_dragonfly_ready",
		Help: "1 if Dragonfly is ready, 0 otherwise",
	}, []string{"namespace", "name"})

	gatewayInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "krakend_operator_gateway_info",
		Help: "Gateway metadata labels",
	}, []string{"namespace", "name", "edition", "version"})

	gatewayConfigValid = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "krakend_operator_gateway_config_valid",
		Help: "1 while the gateway's newest rendered config passed validation (ConfigValid=True), 0 otherwise",
	}, []string{"namespace", "name"})

	gatewayExcludedEndpoints = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "krakend_operator_gateway_excluded_endpoints",
		Help: "KrakenDEndpoints a gateway leaves out of its config because they fail validation on their own, by Accepted reason",
	}, []string{"namespace", "gateway", "reason"})

	autoConfigSynced = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "krakend_operator_autoconfig_synced",
		Help: "1 if the KrakenDAutoConfig's last reconcile synced successfully, 0 if it is failing",
	}, []string{"namespace", "name"})
)

// GatewayMetrics records the gateway controller's metrics. The controller
// owns this port; telemetry.OperatorMetrics implements it on OpenTelemetry
// instruments, and a reconciler given none records nothing.
type GatewayMetrics interface {
	ConfigRendered(ctx context.Context)
	ConfigRejected(ctx context.Context)
	RollingRestart(ctx context.Context)
	GatewayReconciled(ctx context.Context, gateway types.NamespacedName, d time.Duration)
	SetLicenseExpiry(gateway types.NamespacedName, left time.Duration)
	ForgetLicenseExpiry(gateway types.NamespacedName)
	SetEndpoints(gateway types.NamespacedName, n int)
	SetDragonflyReady(gateway types.NamespacedName, ready bool)
	ForgetDragonflyReady(gateway types.NamespacedName)
	SetConfigValid(gateway types.NamespacedName, valid bool)
	SetGatewayInfo(gateway types.NamespacedName, edition, version string)
	SetExcludedEndpoints(gateway types.NamespacedName, byReason map[string]int)
	ForgetGateway(gateway types.NamespacedName)
}

// AutoConfigMetrics records the AutoConfig controller's metric.
type AutoConfigMetrics interface {
	SetAutoConfigSynced(autoConfig types.NamespacedName, synced bool)
	ForgetAutoConfig(autoConfig types.NamespacedName)
}

// deleteGatewayMetrics removes every series labelled with the gateway, so a
// deleted gateway stops reporting, and alerting, until it is recreated.
func deleteGatewayMetrics(namespace, name string) {
	gateway := prometheus.Labels{"namespace": namespace, "name": name}
	endpointsPerGateway.DeletePartialMatch(gateway)
	gatewayInfo.DeletePartialMatch(gateway)
	gatewayConfigValid.DeletePartialMatch(gateway)
	dragonflyReady.DeletePartialMatch(gateway)
	licenseExpirySeconds.DeletePartialMatch(gateway)
	gatewayExcludedEndpoints.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "gateway": name})
	reconcileDuration.DeletePartialMatch(prometheus.Labels{
		"controller": "gateway", "namespace": namespace, "name": name,
	})
}

// recordExcludedEndpoints replaces the gateway's excluded-endpoint series
// with one per reason that has any, so a reason with none has no series.
func recordExcludedEndpoints(gw *v1alpha1.KrakenDGateway, counts map[string]int) {
	gatewayExcludedEndpoints.DeletePartialMatch(prometheus.Labels{"namespace": gw.Namespace, "gateway": gw.Name})
	for reason, n := range counts {
		gatewayExcludedEndpoints.WithLabelValues(gw.Namespace, gw.Name, reason).Set(float64(n))
	}
}

// promMetrics records into the package's Prometheus collectors. It is the
// recorder of a reconciler given none while the collectors still exist.
type promMetrics struct{}

func (promMetrics) ConfigRendered(context.Context) { configRenders.Inc() }
func (promMetrics) ConfigRejected(context.Context) { configValidationFailures.Inc() }
func (promMetrics) RollingRestart(context.Context) { rollingRestarts.Inc() }

func (promMetrics) GatewayReconciled(_ context.Context, gw types.NamespacedName, d time.Duration) {
	reconcileDuration.WithLabelValues("gateway", gw.Namespace, gw.Name).Observe(d.Seconds())
}

func (promMetrics) SetLicenseExpiry(gw types.NamespacedName, left time.Duration) {
	licenseExpirySeconds.WithLabelValues(gw.Namespace, gw.Name).Set(left.Seconds())
}

func (promMetrics) ForgetLicenseExpiry(gw types.NamespacedName) {
	licenseExpirySeconds.DeleteLabelValues(gw.Namespace, gw.Name)
}

func (promMetrics) SetEndpoints(gw types.NamespacedName, n int) {
	endpointsPerGateway.WithLabelValues(gw.Namespace, gw.Name).Set(float64(n))
}

func (promMetrics) SetDragonflyReady(gw types.NamespacedName, ready bool) {
	dragonflyReady.WithLabelValues(gw.Namespace, gw.Name).Set(gaugeOf(ready))
}

func (promMetrics) ForgetDragonflyReady(gw types.NamespacedName) {
	dragonflyReady.DeleteLabelValues(gw.Namespace, gw.Name)
}

func (promMetrics) SetConfigValid(gw types.NamespacedName, valid bool) {
	gatewayConfigValid.WithLabelValues(gw.Namespace, gw.Name).Set(gaugeOf(valid))
}

func (promMetrics) SetGatewayInfo(gw types.NamespacedName, edition, version string) {
	gatewayInfo.DeletePartialMatch(prometheus.Labels{"namespace": gw.Namespace, "name": gw.Name})
	gatewayInfo.WithLabelValues(gw.Namespace, gw.Name, edition, version).Set(1)
}

func (promMetrics) SetExcludedEndpoints(gw types.NamespacedName, byReason map[string]int) {
	gatewayExcludedEndpoints.DeletePartialMatch(prometheus.Labels{"namespace": gw.Namespace, "gateway": gw.Name})
	for reason, n := range byReason {
		if n > 0 {
			gatewayExcludedEndpoints.WithLabelValues(gw.Namespace, gw.Name, reason).Set(float64(n))
		}
	}
}

func (promMetrics) ForgetGateway(gw types.NamespacedName) {
	deleteGatewayMetrics(gw.Namespace, gw.Name)
}

func (promMetrics) SetAutoConfigSynced(ac types.NamespacedName, synced bool) {
	autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name).Set(gaugeOf(synced))
}

func (promMetrics) ForgetAutoConfig(ac types.NamespacedName) {
	autoConfigSynced.DeleteLabelValues(ac.Namespace, ac.Name)
}

// metrics returns r's recorder, or the Prometheus collectors when it has none.
func (r *KrakenDGatewayReconciler) metrics() GatewayMetrics {
	if r.Metrics == nil {
		return promMetrics{}
	}
	return r.Metrics
}

// metrics returns r's recorder, or the Prometheus collectors when it has none.
func (r *KrakenDAutoConfigReconciler) metrics() AutoConfigMetrics {
	if r.Metrics == nil {
		return promMetrics{}
	}
	return r.Metrics
}

// gaugeOf is 1 for true and 0 for false.
func gaugeOf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// recordGatewayMetrics sets the gateway's per-gateway series from its status.
// gateway_info is replaced, not added to, so a version or edition change
// leaves one series.
func (r *KrakenDGatewayReconciler) recordGatewayMetrics(gw *v1alpha1.KrakenDGateway, endpoints int) {
	key := client.ObjectKeyFromObject(gw)
	m := r.metrics()
	m.SetEndpoints(key, endpoints)
	m.SetGatewayInfo(key, string(gw.Spec.Edition), gw.Spec.Version)
	m.SetConfigValid(key, meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionConfigValid))
}

func init() { //nolint:gochecknoinits // required by prometheus metric registration
	metrics.Registry.MustRegister(
		configRenders,
		configValidationFailures,
		rollingRestarts,
		licenseExpirySeconds,
		endpointsPerGateway,
		reconcileDuration,
		dragonflyReady,
		gatewayInfo,
		gatewayConfigValid,
		gatewayExcludedEndpoints,
		autoConfigSynced,
	)
}
