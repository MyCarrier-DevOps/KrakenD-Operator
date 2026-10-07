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

package main

import (
	"context"
	"testing"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gatewayv1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
	webhooksetup "github.com/mycarrier-devops/krakend-operator/internal/webhook"
)

// stubManager answers the four Manager methods wireValidation reads and
// panics on any other, so the test also notices a new dependency.
type stubManager struct {
	ctrl.Manager
	client client.Client
}

func (m stubManager) GetClient() client.Client { return m.client }

func (m stubManager) GetScheme() *runtime.Scheme { return m.client.Scheme() }

func (m stubManager) GetAPIReader() client.Reader { return m.client }

func (m stubManager) GetEventRecorderFor(string) record.EventRecorder {
	return record.NewFakeRecorder(1)
}

// The 3-slot limit on concurrent krakend executions is per pod: the gateway
// controller, the AutoConfig controller and every webhook validator must share
// the one Checker that holds it.
func TestWireValidation_SharesOneCheckerBetweenControllerAndWebhooks(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "", instrumentation{})

	if w.Checker == nil {
		t.Fatal("wireValidation built no checker")
	}
	if w.Gateway.Checker != controller.ConfigChecker(w.Checker) {
		t.Errorf("the gateway reconciler's checker = %v, want the pod's checker %p", w.Gateway.Checker, w.Checker)
	}
	if w.AutoConfig == nil {
		t.Fatal("wireValidation built no AutoConfig reconciler")
	}
	if w.AutoConfig.Checker != controller.AutoConfigChecker(w.Checker) {
		t.Errorf("the AutoConfig reconciler's checker = %v, want the pod's checker %p", w.AutoConfig.Checker, w.Checker)
	}
	if w.Validators.Gateway.Checker != webhooksetup.ConfigChecker(w.Checker) {
		t.Errorf("the gateway validator's checker = %v, want the pod's checker %p",
			w.Validators.Gateway.Checker, w.Checker)
	}
	if w.Validators.Endpoint.Checker != webhooksetup.ConfigChecker(w.Checker) {
		t.Errorf("the endpoint validator's checker = %v, want the pod's checker %p",
			w.Validators.Endpoint.Checker, w.Checker)
	}
}

func TestConfigCheckSlots(t *testing.T) {
	if configCheckSlots != 3 {
		t.Errorf("configCheckSlots = %d, want 3", configCheckSlots)
	}
}

// The endpoint validator trusts the username wireValidation is given, and
// only that one.
func TestWireValidation_EndpointValidatorTrustsTheGivenOperatorUsername(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}
	const operator = "system:serviceaccount:krakend-system:krakend-operator"

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, operator, instrumentation{})

	if got := w.Validators.Endpoint.OperatorUsername; got != operator {
		t.Errorf("endpoint validator's OperatorUsername = %q, want %q", got, operator)
	}
}

// The endpoint validator re-reads policies uncached, through the manager's
// API reader.
func TestWireValidation_EndpointValidatorReadsPoliciesUncached(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "", instrumentation{})

	if w.Validators.Endpoint.APIReader != mgr.GetAPIReader() {
		t.Errorf("endpoint validator's APIReader = %v, want the manager's API reader", w.Validators.Endpoint.APIReader)
	}
}

// The pod's checker is shared by the gateway controller, the AutoConfig
// controller and admission. The AutoConfig prechecks and the gateway checks
// together never hold more than all but one of its slots (2 of the 3);
// concurrent admission requests can still take the rest.
func TestWireValidation_AutoConfigAndGatewayChecksLeaveAnAdmissionSlot(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "", instrumentation{})

	gatewayWorkers := w.Gateway.MaxConcurrentReconciles
	if held := cap(w.AutoConfig.CheckSlots) + gatewayWorkers; held > configCheckSlots-1 {
		t.Errorf("AutoConfig prechecks (%d slots) and gateway checks (%d) can hold %d of %d checker slots, "+
			"leaving admission none", cap(w.AutoConfig.CheckSlots), gatewayWorkers, held, configCheckSlots)
	}
}

// The gateway controller reconciles with as many workers as the checker slots
// the AutoConfig bound leaves it.
func TestWireValidation_GatewayWorkersMatchTheSlotsReservedForThem(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "", instrumentation{})

	if got := w.Gateway.MaxConcurrentReconciles; got != gatewayCheckWorkers {
		t.Errorf("gateway MaxConcurrentReconciles = %d, want gatewayCheckWorkers (%d)", got, gatewayCheckWorkers)
	}
}

// testInstrumentation records spans to rec and metrics to a no-op meter.
func testInstrumentation(t *testing.T, rec *tracingtest.Recorder) instrumentation {
	t.Helper()
	m, err := telemetry.NewOperatorMetrics(metricnoop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return instrumentation{Tracer: rec.Tracer(), Metrics: m}
}

// Every component gets the pod's tracer and recorder: a nil one would record
// nothing, silently.
func TestWireValidation_InstrumentsEveryPart(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}
	rec := tracingtest.New(t)
	inst := testInstrumentation(t, rec)

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "", inst)

	if w.Gateway.Tracer != inst.Tracer || w.Gateway.Metrics != controller.GatewayMetrics(inst.Metrics) {
		t.Error("the gateway reconciler is not given the pod's tracer and metrics")
	}
	if w.AutoConfig.Tracer != inst.Tracer || w.AutoConfig.Metrics != controller.AutoConfigMetrics(inst.Metrics) {
		t.Error("the AutoConfig reconciler is not given the pod's tracer and metrics")
	}
	if w.Validators.Tracer != inst.Tracer {
		t.Error("the validators are not given the pod's tracer")
	}
	ctx := context.Background()
	_, _ = w.Checker.Gather(ctx, &gatewayv1alpha1.KrakenDGateway{}, nil)
	_, _ = w.AutoConfig.Fetcher.Fetch(ctx, autoconfig.FetchSource{ConfigMapRef: &gatewayv1alpha1.ConfigMapKeyRef{Name: "absent"}})
	for _, name := range []string{"configcheck.Gather", "autoconfig.fetch"} {
		if len(rec.Ended().Named(name)) != 1 {
			t.Errorf("no %s span: that component is not given the pod's tracer", name)
		}
	}
}

// A recorder the pod has none of is a nil port, which the reconcilers turn
// into a no-op: a nil *OperatorMetrics inside the interface would not be nil
// and would panic on its first use.
func TestWireValidation_NoRecorderLeavesTheMetricsPortsNil(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "", instrumentation{})

	if w.Gateway.Metrics != nil {
		t.Errorf("gateway Metrics = %#v, want a nil port", w.Gateway.Metrics)
	}
	if w.AutoConfig.Metrics != nil {
		t.Errorf("AutoConfig Metrics = %#v, want a nil port", w.AutoConfig.Metrics)
	}
}
