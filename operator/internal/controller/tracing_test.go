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

	"go.opentelemetry.io/otel/attribute"
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
	withDragonfly := func(gw *v1alpha1.KrakenDGateway) { gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true} }
	for _, tc := range []struct {
		span, parent string
		tweak        func(*v1alpha1.KrakenDGateway)
	}{
		{"configcheck.Gather", "reconcile KrakenDGateway", nil},
		{"gateway.license", "reconcile KrakenDGateway", nil},
		{"gateway.plugins", "reconcile KrakenDGateway", nil},
		{"gateway.dragonfly", "reconcile KrakenDGateway", withDragonfly},
		{"gateway.render", "reconcile KrakenDGateway", nil},
		{"gateway.config", "reconcile KrakenDGateway", nil},
		{"configcheck.CheckRoot", "gateway.config", nil},
		{"configcheck.CheckRendered", "gateway.config", nil},
		{"gateway.judge_endpoints", "gateway.config", nil},
		{"gateway.publish_configmap", "gateway.config", nil},
		{"gateway.verify_configmap", "gateway.publish_configmap", nil},
		{"gateway.acceptance", "reconcile KrakenDGateway", nil},
		{"gateway.endpoint_status", "gateway.acceptance", nil},
		{"gateway.core_resources", "reconcile KrakenDGateway", nil},
		{"apply serviceaccount", "gateway.core_resources", nil},
		{"apply service", "gateway.core_resources", nil},
		{"apply pdb", "gateway.core_resources", nil},
		{"gateway.infrastructure", "reconcile KrakenDGateway", nil},
		{"apply deployment", "gateway.infrastructure", nil},
		{"gateway.collect_configmaps", "gateway.infrastructure", nil},
		{"apply job", "gateway.infrastructure", nil},
		{"gateway.delete_child", "gateway.infrastructure", nil},
		{"gateway.status", "reconcile KrakenDGateway", nil},
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

// The render without the endpoints that fail on their own is a render span of
// its own, inside the config stage, and is not counted as another render.
func TestGatewayReconcile_TheRenderAfterAnExclusionIsASpanOfTheConfigStage(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw, testEndpoint("good", "/a"), badHosted("bad", "/b"))
	rec := tracingtest.New(t)
	r := tracedGatewayReconciler(c, rejectsBadHosts(), rec)
	m, reg := testMetrics(t)
	r.Metrics = m

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	parents := map[string]int{}
	for _, render := range spans.Named("gateway.render") {
		if parent := spans.Parent(render); parent != nil {
			parents[parent.Name()]++
		}
	}
	if parents["reconcile KrakenDGateway"] != 1 || parents["gateway.config"] != 1 {
		t.Errorf("gateway.render spans by parent = %v, want the newest render under the reconcile and the one "+
			"without the excluded endpoint under gateway.config; spans: %s", parents, spans)
	}
	if got, _ := metricValue(t, reg, "krakend_operator_config_renders_total"); got != 1 {
		t.Errorf("config_renders_total = %v, want 1: the render after an exclusion is not another render attempt", got)
	}
}

// A krakend run is below the check that ran it, inside the config stage.
func TestGatewayReconcile_AKrakendRunIsBelowItsCheckInTheConfigStage(t *testing.T) {
	spans := reconcileTraced(t, testGateway(), testEndpoint("e", "/e"))

	spans.RequireAncestors(t, "krakend check", "configcheck.", "gateway.config", "reconcile KrakenDGateway")
}

// Each judging pass says which endpoints it judges: the first reconcile judges
// the suspects of the whole render's check; once that config is applied, the
// next judges only the endpoints the applied render leaves an entry of out.
func TestGatewayReconcile_EachJudgingPassNamesItsEndpoints(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw, testEndpoint("e", "/e"))
	first := tracingtest.New(t)
	r := tracedGatewayReconciler(c, krakendValidator(first), first)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	second := tracingtest.New(t)
	r.Tracer = second.Tracer()

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	for rec, pass := range map[*tracingtest.Recorder]string{first: "suspects", second: "masked"} {
		spans := rec.Ended()
		judged := spans.Named("gateway.judge_endpoints").With(attribute.String("gateway.judge.pass", pass))
		if n := len(judged); n != 1 {
			t.Errorf("%d judging passes of %q, want 1; spans: %s", n, pass, spans)
		}
	}
}

// An endpoint judged on its own is a check below the pass that judges it,
// inside the config stage.
func TestGatewayReconcile_AnEndpointCheckIsASpanOfItsJudgingPass(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw, testEndpoint("good", "/a"), badHosted("bad", "/b"))
	rec := tracingtest.New(t)
	r := tracedGatewayReconciler(c, rejectsBadHosts(), rec)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireParent(t, "gateway.judge_endpoints", "configcheck.CheckEndpoint")
	spans.RequireParent(t, "gateway.config", "gateway.judge_endpoints")
}

// decide checks a whole render twice when an endpoint fails on its own: the
// render with every endpoint, then the render without the excluded ones. Each
// check says which it is.
func TestGatewayReconcile_NamesWhyEachRenderIsChecked(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw, testEndpoint("good", "/a"), badHosted("bad", "/b"))
	rec := tracingtest.New(t)
	r := tracedGatewayReconciler(c, rejectsBadHosts(), rec)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	for _, purpose := range []string{"combined", "safety_net"} {
		checks := spans.Named("configcheck.CheckRendered").With(attribute.String("configcheck.purpose", purpose))
		if len(checks) != 1 {
			t.Errorf("%d render checks for %q, want 1; spans: %s", len(checks), purpose, spans)
		}
	}
}

// A pass whose render is rejected keeps the applied config, and verifies the
// ConfigMap that holds it inside the config stage.
func TestGatewayReconcile_KeepingTheAppliedConfigVerifiesItInTheConfigStage(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw)
	rend := renderOf(`{"version":3,"name":"applied"}`)
	val := &countingValidator{}
	r := newTestGatewayReconciler(c, rend, val)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	*rend = *renderOf(`{"version":3,"name":"rejected"}`)
	val.err = rejectedBy("- at '/endpoints/0/endpoint': bad")
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	rec.Ended().RequireChild(t, "gateway.config", "gateway.verify_configmap")
}
