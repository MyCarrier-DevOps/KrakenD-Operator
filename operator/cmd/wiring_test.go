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
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
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

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "")

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

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, operator)

	if got := w.Validators.Endpoint.OperatorUsername; got != operator {
		t.Errorf("endpoint validator's OperatorUsername = %q, want %q", got, operator)
	}
}

// The endpoint validator re-reads policies uncached, through the manager's
// API reader.
func TestWireValidation_EndpointValidatorReadsPoliciesUncached(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "")

	if w.Validators.Endpoint.APIReader != mgr.GetAPIReader() {
		t.Errorf("endpoint validator's APIReader = %v, want the manager's API reader", w.Validators.Endpoint.APIReader)
	}
}

// The AutoConfig controller's workers share the pod's checker with admission,
// so their prechecks may hold all but one of its slots.
func TestWireValidation_AutoConfigPrecheckLeavesAnAdmissionSlot(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil, "")

	if got, want := cap(w.AutoConfig.CheckSlots), configCheckSlots-1; got != want {
		t.Errorf("AutoConfig check slots = %d, want %d", got, want)
	}
}
