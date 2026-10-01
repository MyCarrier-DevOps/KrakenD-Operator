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
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// testNow is the instant the gateway tests' clock reads.
var testNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// newTestGatewayReconciler wires a gateway reconciler with the fakes the
// gateway tests need. A new collaborator is added here, once.
func newTestGatewayReconciler(
	c client.Client, rend renderer.Renderer, val renderer.Validator,
) *KrakenDGatewayReconciler {
	return &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  rend,
		Validator: val,
		Clock:     clocktesting.NewFakeClock(testNow),
	}
}

func TestGatewayReconcile_InfrastructureRunsWhateverTheConfigVerdict(t *testing.T) {
	cases := []struct {
		name      string
		verdict   error
		wantError bool
	}{
		{name: "rejected config", verdict: rejectedBy("- at '/endpoints/0/endpoint': bad")},
		{name: "validator unavailable", verdict: errors.New("fork/exec krakend: no such file or directory"), wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.Status.ConfigChecksum = "applied"
			c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOutput("new"), &countingValidator{err: tc.verdict})

			err := reconcileGateway(t, r, gw)
			if (err != nil) != tc.wantError {
				t.Fatalf("reconcile error = %v, want an error: %v", err, tc.wantError)
			}
			var dep appsv1.Deployment
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
				t.Fatalf("the Deployment must be reconciled whatever the config verdict: %v", err)
			}
			if got := dep.Spec.Template.Annotations[resources.PostRestartJobChecksumAnnotation]; got != "applied" {
				t.Errorf("the Deployment must carry the applied config %q, got %q", "applied", got)
			}
			var svc corev1.Service
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &svc); err != nil {
				t.Errorf("the Service must be reconciled whatever the config verdict: %v", err)
			}
		})
	}
}

func TestGatewayReconcile_NoDeploymentBeforeAnyConfigPasses(t *testing.T) {
	gw := reconciledGateway()
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("new"),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &svc); err != nil {
		t.Fatalf("the Service must be reconciled before any config passes: %v", err)
	}
	var dep appsv1.Deployment
	err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("no Deployment may exist before any config passes validation; Get returned %v", err)
	}
}
