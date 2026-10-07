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
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
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
		Help: "Fresh rejections of a gateway root, a backend policy or an endpoint checked on its own: " +
			"each distinct config the gateway controller checked and krakend rejected counts once",
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

// recordGatewayMetrics sets the gateway's per-gateway series from its status.
// gateway_info is replaced, not added to, so a version or edition change
// leaves one series.
func recordGatewayMetrics(gw *v1alpha1.KrakenDGateway, endpoints int) {
	endpointsPerGateway.WithLabelValues(gw.Namespace, gw.Name).Set(float64(endpoints))
	gatewayInfo.DeletePartialMatch(prometheus.Labels{"namespace": gw.Namespace, "name": gw.Name})
	gatewayInfo.WithLabelValues(gw.Namespace, gw.Name, string(gw.Spec.Edition), gw.Spec.Version).Set(1)
	valid := 0.0
	if meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionConfigValid) {
		valid = 1
	}
	gatewayConfigValid.WithLabelValues(gw.Namespace, gw.Name).Set(valid)
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
