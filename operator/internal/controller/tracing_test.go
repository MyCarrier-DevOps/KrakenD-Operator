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
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// acceptingExecutor stands in for a krakend binary that accepts every config.
type acceptingExecutor struct{}

func (acceptingExecutor) Execute(context.Context, string, ...string) ([]byte, error) {
	return []byte("Syntax OK!"), nil
}

// krakendValidator is the real validator over a krakend binary that accepts
// every config, each run a span of rec.
func krakendValidator(rec *tracingtest.Recorder) renderer.Validator {
	return renderer.NewValidator(renderer.ValidatorOptions{
		Executor: telemetry.TraceExecutor(acceptingExecutor{}, rec.Tracer()), BinaryPath: "krakend",
	})
}

// tracedGatewayReconciler returns a gateway reconciler with the real renderer,
// validating with v, whose spans and its checker's go to rec.
func tracedGatewayReconciler(c client.Client, v renderer.Validator, rec *tracingtest.Recorder) *KrakenDGatewayReconciler {
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), v)
	r.Checker = configcheck.New(c, renderer.New(renderer.Options{}), v, 1, rec.Tracer())
	r.Tracer = rec.Tracer()
	return r
}

// reconcileTraced reconciles gw, stored with objs, once, and returns its spans.
func reconcileTraced(t *testing.T, gw *v1alpha1.KrakenDGateway, objs ...client.Object) tracingtest.Spans {
	t.Helper()
	c, _ := gatewayStatusWrites(append([]client.Object{gw}, objs...)...)
	rec := tracingtest.New(t)
	if err := reconcileGateway(t, tracedGatewayReconciler(c, krakendValidator(rec), rec), gw); err != nil {
		t.Fatal(err)
	}
	return rec.Ended()
}

// A gateway reconcile is the root of a trace of its own, and names the
// generation it read.
func TestGatewayReconcile_IsTheRootOfATrace(t *testing.T) {
	gw := testGateway()
	gw.Generation = 4

	spans := reconcileTraced(t, gw)

	root := spans.One(t, "reconcile KrakenDGateway")
	if root.Parent().IsValid() {
		t.Errorf("the reconcile span has parent %v, want a root", root.Parent())
	}
	if len(tracingtest.Spans{root}.With(tracing.KeyGeneration.Int64(4))) != 1 {
		t.Errorf("the reconcile span lacks k8s.object.generation 4: %v", root.Attributes())
	}
}

// Each stage of a gateway reconcile is a span under the stage that runs it, so
// the trace reads as the reconcile's waterfall.
func TestGatewayReconcile_SpansEachStageUnderItsParent(t *testing.T) {
	for _, tc := range []struct {
		span, parent string
		tweak        func(*v1alpha1.KrakenDGateway)
	}{
		{"gateway.license", "reconcile KrakenDGateway", nil},
	} {
		t.Run(tc.span, func(t *testing.T) {
			gw := testGateway()
			if tc.tweak != nil {
				tc.tweak(gw)
			}

			spans := reconcileTraced(t, gw, testEndpoint("e", "/e"))

			spans.RequireChild(t, tc.parent, tc.span)
		})
	}
}
