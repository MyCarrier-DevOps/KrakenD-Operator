/*
Copyright 2026.

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
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	configRenders = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "krakend_operator_config_renders_total",
		Help: "Total config render attempts",
	})

	configValidationFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "krakend_operator_config_validation_failures_total",
		Help: "Validation failures (broken configs blocked)",
	})

	rollingRestarts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "krakend_operator_rolling_restarts_total",
		Help: "Rolling deployments triggered",
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
)

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
	)
}
