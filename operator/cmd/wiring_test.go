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
// controller and every webhook validator must share the one Checker that
// holds it.
func TestWireValidation_SharesOneCheckerBetweenControllerAndWebhooks(t *testing.T) {
	mgr := stubManager{client: fake.NewClientBuilder().Build()}

	w := wireValidation(mgr, renderer.New(renderer.Options{}), nil)

	if w.Checker == nil {
		t.Fatal("wireValidation built no checker")
	}
	if w.Gateway.Checker != controller.ConfigChecker(w.Checker) {
		t.Errorf("the gateway reconciler's checker = %v, want the pod's checker %p", w.Gateway.Checker, w.Checker)
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
