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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
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

// noopMetrics records nothing: the recorder of a reconciler given none.
type noopMetrics struct{}

func (noopMetrics) ConfigRendered(context.Context)                                         {}
func (noopMetrics) ConfigRejected(context.Context)                                         {}
func (noopMetrics) RollingRestart(context.Context)                                         {}
func (noopMetrics) GatewayReconciled(context.Context, types.NamespacedName, time.Duration) {}
func (noopMetrics) SetLicenseExpiry(types.NamespacedName, time.Duration)                   {}
func (noopMetrics) ForgetLicenseExpiry(types.NamespacedName)                               {}
func (noopMetrics) SetEndpoints(types.NamespacedName, int)                                 {}
func (noopMetrics) SetDragonflyReady(types.NamespacedName, bool)                           {}
func (noopMetrics) ForgetDragonflyReady(types.NamespacedName)                              {}
func (noopMetrics) SetConfigValid(types.NamespacedName, bool)                              {}
func (noopMetrics) SetGatewayInfo(types.NamespacedName, string, string)                    {}
func (noopMetrics) SetExcludedEndpoints(types.NamespacedName, map[string]int)              {}
func (noopMetrics) ForgetGateway(types.NamespacedName)                                     {}
func (noopMetrics) SetAutoConfigSynced(types.NamespacedName, bool)                         {}
func (noopMetrics) ForgetAutoConfig(types.NamespacedName)                                  {}

// metrics returns r's recorder, or one that records nothing.
func (r *KrakenDGatewayReconciler) metrics() GatewayMetrics {
	if r.Metrics == nil {
		return noopMetrics{}
	}
	return r.Metrics
}

// metrics returns r's recorder, or one that records nothing.
func (r *KrakenDAutoConfigReconciler) metrics() AutoConfigMetrics {
	if r.Metrics == nil {
		return noopMetrics{}
	}
	return r.Metrics
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
