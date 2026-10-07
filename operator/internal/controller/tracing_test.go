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
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
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
		spans.RequireChild(t, "gateway.config", "gateway.judge_endpoints")
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
	spans.RequireParent(t, "gateway.config", "configcheck.CheckRendered")
	for _, check := range spans.Named("configcheck.CheckEndpoint") {
		if purpose, ok := tracingtest.Attr(check, "configcheck.purpose"); ok {
			t.Errorf("an endpoint check carries the purpose of a render check: %v", purpose)
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

// Each child of an optional feature is applied inside the infrastructure
// stage: the autoscaler, and the Dragonfly, license ExternalSecret and Istio
// VirtualService whose CRDs are installed.
func TestGatewayReconcile_SpansEachOptionalChildUnderTheInfrastructureStage(t *testing.T) {
	gw := testGateway()
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MaxReplicas: 5}
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.License = &v1alpha1.LicenseConfig{ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{
		Enabled:        true,
		SecretStoreRef: v1alpha1.SecretStoreRef{Name: "vault", Kind: "ClusterSecretStore"},
		RemoteRef:      v1alpha1.ExternalRemoteRef{Key: "krakend/license"},
	}}
	gw.Spec.Istio = &v1alpha1.IstioSpec{
		Enabled: true, Hosts: []string{"api.example.com"}, Gateways: []string{"istio-system/gw"},
	}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
		WithObjects(gw).WithStatusSubresource(gw).Build()
	rec := tracingtest.New(t)
	if err := reconcileGateway(t, tracedGatewayReconciler(c, krakendValidator(rec), rec), gw); err != nil {
		t.Fatal(err)
	}
	spans := rec.Ended()

	for _, child := range []string{"apply hpa", "apply dragonfly", "apply externalsecret", "apply virtualservice"} {
		t.Run(child, func(t *testing.T) {
			spans.RequireChild(t, "gateway.infrastructure", child)
		})
	}
}

// A CRD lookup can send a discovery request, which client-go sends without a
// context, so no client span records it: each lookup is a k8s.discovery span
// of its own, under the stage that needs the kind.
func TestGatewayReconcile_EachCRDLookupIsADiscoverySpanOfItsStage(t *testing.T) {
	for _, tc := range []struct {
		kind, parent string
		enable       func(*v1alpha1.KrakenDGateway)
	}{
		{"Dragonfly.dragonflydb.io", "gateway.dragonfly", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
		}},
		{"VirtualService.networking.istio.io", "gateway.infrastructure", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Istio = &v1alpha1.IstioSpec{
				Enabled: true, Hosts: []string{"api.example.com"}, Gateways: []string{"istio-system/gw"},
			}
		}},
		{"ExternalSecret.external-secrets.io", "gateway.license", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Edition = v1alpha1.EditionEE
			gw.Spec.License = &v1alpha1.LicenseConfig{ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{
				Enabled:        true,
				SecretStoreRef: v1alpha1.SecretStoreRef{Name: "vault", Kind: "ClusterSecretStore"},
				RemoteRef:      v1alpha1.ExternalRemoteRef{Key: "krakend/license"},
			}}
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			gw := testGateway()
			tc.enable(gw)

			spans := reconcileTraced(t, gw, testEndpoint("e", "/e"))

			found := false
			for _, span := range spans.Named("k8s.discovery") {
				parent := spans.Parent(span)
				if parent == nil {
					t.Fatalf("a k8s.discovery span has no parent; spans: %s", spans)
				}
				kind := tracingtest.Spans{span}.With(attribute.String("k8s.discovery.kind", tc.kind))
				found = found || (len(kind) == 1 && parent.Name() == tc.parent)
			}
			if !found {
				t.Errorf("no k8s.discovery span of %s under %q; spans: %s", tc.kind, tc.parent, spans)
			}
		})
	}
}

// A reconcile started inside another trace is still the root of one of its
// own: a trace per reconcile, whatever context the manager hands it.
func TestGatewayReconcile_StartsANewTraceWhateverTheContextCarries(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw, testEndpoint("e", "/e"))
	rec := tracingtest.New(t)
	ctx, outer := rec.Tracer().Start(context.Background(), "outer")
	defer outer.End()
	r := tracedGatewayReconciler(c, krakendValidator(rec), rec)

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)}); err != nil {
		t.Fatal(err)
	}

	root := rec.Ended().One(t, "reconcile KrakenDGateway")
	if root.Parent().IsValid() {
		t.Errorf("the reconcile span has parent %v, want a root", root.Parent())
	}
	if root.SpanContext().TraceID() == outer.SpanContext().TraceID() {
		t.Errorf("the reconcile span joined the trace %v of its context, want a trace of its own", root.SpanContext().TraceID())
	}
}

// A failure of the core resources is the core resources stage's error: the
// infrastructure stage that runs after it carries only its own, while the
// reconcile still returns both.
func TestGatewayReconcile_ACoreResourceFailureMarksOnlyItsOwnStage(t *testing.T) {
	gw := testGateway()
	c := fakeClientBuilder().WithObjects(gw, testEndpoint("e", "/e")).
		WithStatusSubresource(&v1alpha1.KrakenDGateway{}, &v1alpha1.KrakenDEndpoint{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Service); ok {
					return errors.New("service refused")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	rec := tracingtest.New(t)

	err := reconcileGateway(t, tracedGatewayReconciler(c, krakendValidator(rec), rec), gw)

	if err == nil || !strings.Contains(err.Error(), "service refused") {
		t.Fatalf("Reconcile error = %v, want the service failure", err)
	}
	spans := rec.Ended()
	for span, want := range map[string]codes.Code{
		"gateway.core_resources": codes.Error, "gateway.infrastructure": codes.Unset,
	} {
		if got := spans.One(t, span).Status().Code; got != want {
			t.Errorf("%s status = %v, want %v", span, got, want)
		}
	}
}

// The Job the post-restart stage applies names itself on its span.
func TestGatewayReconcile_TheJobSpanNamesTheJob(t *testing.T) {
	gw := testGateway()
	gw.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo done"}

	spans := reconcileTraced(t, gw, testEndpoint("e", "/e"))

	got, _ := tracingtest.Attr(spans.One(t, "apply job"), "k8s.object.name")
	if want := gw.Name + "-postrestart-"; !strings.HasPrefix(got.AsString(), want) {
		t.Errorf("apply job k8s.object.name = %q, want the Job's name, prefixed %q", got.AsString(), want)
	}
}

// The reconcile span names the object it reconciles, and carries the ID of the
// controller-runtime reconcile (empty when no manager ran it).
func TestGatewayReconcile_TheRootSpanNamesTheGateway(t *testing.T) {
	gw := testGateway()

	root := reconcileTraced(t, gw).One(t, "reconcile KrakenDGateway")

	for key, want := range map[string]string{
		"k8s.namespace.name": gw.Namespace, "k8s.object.name": gw.Name, "k8s.object.kind": "KrakenDGateway",
	} {
		if got, _ := tracingtest.Attr(root, key); got.Emit() != want {
			t.Errorf("the reconcile span's %s = %q, want %q", key, got.Emit(), want)
		}
	}
	if _, ok := tracingtest.Attr(root, "controller_runtime.reconcile_id"); !ok {
		t.Errorf("the reconcile span lacks controller_runtime.reconcile_id: %v", root.Attributes())
	}
}

// The status span says whether the stage wrote: the first reconcile writes the
// status, the next finds it unchanged.
func TestGatewayReconcile_TheStatusSpanSaysWhetherItWrote(t *testing.T) {
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

	for rec, want := range map[*tracingtest.Recorder]string{first: "true", second: "false"} {
		if got, _ := tracingtest.Attr(rec.Ended().One(t, "gateway.status"), "gateway.status.written"); got.Emit() != want {
			t.Errorf("gateway.status.written = %q, want %q", got.Emit(), want)
		}
	}
}

// The spans of a write name the object they write.
func TestGatewayReconcile_TheSpansOfAWriteNameItsObject(t *testing.T) {
	gw := testGateway()

	spans := reconcileTraced(t, gw, testEndpoint("e", "/e"))

	for _, span := range []string{"apply service", "apply deployment", "gateway.delete_child"} {
		if got, _ := tracingtest.Attr(spans.One(t, span), "k8s.object.name"); got.AsString() != gw.Name {
			t.Errorf("%s k8s.object.name = %q, want %q", span, got.AsString(), gw.Name)
		}
	}
}

// An endpoint reconcile is one trace: the resolution of its references and the
// status write are spans of it.
func TestEndpointReconcile_IsOneTraceWithItsRefsAndStatusWrite(t *testing.T) {
	gw := testGW1()
	ep := endpointOnGW1(1)
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(ep).Build()
	rec := tracingtest.New(t)
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder(), Tracer: rec.Tracer()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ep)}); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "reconcile KrakenDEndpoint", "endpoint.resolve_refs")
	spans.RequireChild(t, "reconcile KrakenDEndpoint", "endpoint.status")
}

// A policy reconcile is one trace: its protection and its status write are
// spans of it.
func TestPolicyReconcile_IsOneTraceWithItsProtectionAndStatusWrite(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	rec := tracingtest.New(t)
	r := &KrakenDBackendPolicyReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(), APIReader: c, Tracer: rec.Tracer(),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "reconcile KrakenDBackendPolicy", "policy.protection")
	spans.RequireChild(t, "reconcile KrakenDBackendPolicy", "policy.status")
}

// reconcileACTraced syncs, once, an AutoConfig with a filter, a stale endpoint
// of its own and a gateway its candidates are checked against, holding one of
// one check slot, and returns the sync's spans.
func reconcileACTraced(t *testing.T) tracingtest.Spans {
	t.Helper()
	ac := testAutoConfig()
	ac.Spec.Filter = &v1alpha1.FilterSpec{IncludeMethods: []string{"GET"}}
	stale := testEndpoint("test-ac-old-endpoint", "/api/old")
	stale.Labels = map[string]string{"gateway.krakend.io/autoconfig": ac.Name}
	c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM(), testGateway(), ownedCopy(t, ac, stale)).
		WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	r.CheckSlots = make(chan struct{}, 1)
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()
	r.Checker = configcheck.New(c, renderer.New(renderer.Options{}), krakendValidator(rec), 1, rec.Tracer())

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}
	return rec.Ended()
}

// An AutoConfig sync is one trace: the spec fetch with its $ref resolution,
// CUE evaluation, the filter, generation, the endpoint pass with its precheck,
// check-slot waits, writes and deletions, and the status write, each under the
// stage that runs it.
func TestAutoConfigReconcile_SpansEachStageUnderItsParent(t *testing.T) {
	for _, tc := range []struct{ span, parent string }{
		{"autoconfig.fetch_spec", "reconcile KrakenDAutoConfig"},
		{"autoconfig.resolve_refs", "autoconfig.fetch_spec"},
		{"autoconfig.cue_definitions", "reconcile KrakenDAutoConfig"},
		{"autoconfig.evaluate", "reconcile KrakenDAutoConfig"},
		{"autoconfig.filter", "reconcile KrakenDAutoConfig"},
		{"autoconfig.generate", "reconcile KrakenDAutoConfig"},
		{"autoconfig.endpoints", "reconcile KrakenDAutoConfig"},
		{"autoconfig.precheck", "autoconfig.endpoints"},
		{"configcheck.Conflicts", "autoconfig.precheck"},
		{"autoconfig.judge_candidates", "autoconfig.precheck"},
		{"autoconfig.slot", "autoconfig.judge_candidates"},
		{"configcheck.CheckRoot", "autoconfig.judge_candidates"},
		{"configcheck.CheckGroup", "autoconfig.judge_candidates"},
		{"autoconfig.write_endpoint", "autoconfig.endpoints"},
		{"autoconfig.delete_endpoint", "autoconfig.endpoints"},
		{"autoconfig.status", "reconcile KrakenDAutoConfig"},
	} {
		t.Run(tc.span, func(t *testing.T) {
			spans := reconcileACTraced(t)

			spans.RequireParent(t, tc.parent, tc.span)
		})
	}
}

// A sync that fails writes its failure status in a span of its own too. A
// failed fetch is the fetch stage's error and the reconcile's: the status write
// that records the failure carries only its own.
func TestAutoConfigReconcile_AFailedSyncWritesItsStatusInASpan(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM()).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	f.err = errors.New("the spec host is unreachable")
	r := newACReconciler(c, f, ce, fi, g)
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("a failed fetch must fail the sync")
	}

	rec.Ended().RequireParent(t, "reconcile KrakenDAutoConfig", "autoconfig.status")
	requireCodes(t, rec.Ended(), map[string]codes.Code{
		"reconcile KrakenDAutoConfig": codes.Error, "autoconfig.fetch_spec": codes.Error, "autoconfig.status": codes.Unset,
	})
}

// A failure status write that conflicts did not happen, though the reconcile
// returns the sync's own failure: its span says so.
func TestAutoConfigReconcile_AConflictingFailureStatusWriteMarksItsSpan(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM()).WithStatusSubresource(ac).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption,
			) error {
				return apierrors.NewConflict(schema.GroupResource{Resource: "krakendautoconfigs"}, ac.Name, errors.New("stale"))
			},
		}).Build()
	f, ce, fi, g := defaultMocks()
	f.err = errors.New("the spec host is unreachable")
	r := newACReconciler(c, f, ce, fi, g)
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()

	if _, err := reconcileAC(r, ac); err == nil || !strings.Contains(err.Error(), "the spec host is unreachable") {
		t.Fatalf("Reconcile error = %v, want the sync's own failure", err)
	}

	if got := rec.Ended().One(t, "autoconfig.status").Status().Code; got != codes.Error {
		t.Errorf("autoconfig.status status = %v, want %v", got, codes.Error)
	}
}

// Each of the endpoint, policy and AutoConfig reconciles is the root of a trace
// of its own, whatever context the manager hands it, and names the object and
// the generation it read.
func TestReconcilers_EachStartsATraceNamingItsObject(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		setup func(t *testing.T, tracer trace.Tracer) (client.Object, func(context.Context) error)
	}{
		{"KrakenDEndpoint", func(_ *testing.T, tracer trace.Tracer) (client.Object, func(context.Context) error) {
			ep := endpointOnGW1(3)
			c := fakeClientBuilder().WithObjects(testGW1(), ep).WithStatusSubresource(ep).Build()
			r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder(), Tracer: tracer}
			return ep, func(ctx context.Context) error {
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ep)})
				return err
			}
		}},
		{"KrakenDBackendPolicy", func(_ *testing.T, tracer trace.Tracer) (client.Object, func(context.Context) error) {
			policy := &v1alpha1.KrakenDBackendPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default", Generation: 3},
			}
			c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
			r := &KrakenDBackendPolicyReconciler{
				Client: c, Scheme: testScheme(), Recorder: fakeRecorder(), APIReader: c, Tracer: tracer,
			}
			return policy, func(ctx context.Context) error {
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
				return err
			}
		}},
		{"KrakenDAutoConfig", func(_ *testing.T, tracer trace.Tracer) (client.Object, func(context.Context) error) {
			ac := testAutoConfig()
			ac.Generation = 3
			c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM()).WithStatusSubresource(ac).Build()
			f, ce, fi, g := defaultMocks()
			r := newACReconciler(c, f, ce, fi, g)
			r.Tracer = tracer
			return ac, func(ctx context.Context) error {
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
				return err
			}
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			rec := tracingtest.New(t)
			obj, reconcile := tc.setup(t, rec.Tracer())
			ctx, outer := rec.Tracer().Start(context.Background(), "outer")
			defer outer.End()

			if err := reconcile(ctx); err != nil {
				t.Fatal(err)
			}

			root := rec.Ended().One(t, "reconcile "+tc.kind)
			if root.Parent().IsValid() || root.SpanContext().TraceID() == outer.SpanContext().TraceID() {
				t.Errorf("the reconcile span has parent %v in trace %v, want a root of a trace of its own",
					root.Parent(), root.SpanContext().TraceID())
			}
			for key, want := range map[string]string{
				"k8s.namespace.name": obj.GetNamespace(), "k8s.object.name": obj.GetName(),
				"k8s.object.kind": tc.kind, "k8s.object.generation": "3",
			} {
				if got, _ := tracingtest.Attr(root, key); got.Emit() != want {
					t.Errorf("the reconcile span's %s = %q, want %q", key, got.Emit(), want)
				}
			}
			if _, ok := tracingtest.Attr(root, "controller_runtime.reconcile_id"); !ok {
				t.Errorf("the reconcile span lacks controller_runtime.reconcile_id: %v", root.Attributes())
			}
		})
	}
}

// The spans of an endpoint write and deletion name the endpoint.
func TestAutoConfigReconcile_TheSpansOfAWriteNameTheEndpoint(t *testing.T) {
	spans := reconcileACTraced(t)

	for span, want := range map[string]string{
		"autoconfig.write_endpoint": "test-ac-listusers", "autoconfig.delete_endpoint": "test-ac-old-endpoint",
	} {
		if got, _ := tracingtest.Attr(spans.One(t, span), "k8s.object.name"); got.AsString() != want {
			t.Errorf("%s k8s.object.name = %q, want %q", span, got.AsString(), want)
		}
	}
}

// requireCodes fails the test unless each of the named spans has the status
// code want gives for it.
func requireCodes(t *testing.T, spans tracingtest.Spans, want map[string]codes.Code) {
	t.Helper()
	for name, code := range want {
		if got := spans.One(t, name).Status().Code; got != code {
			t.Errorf("%s status = %v, want %v", name, got, code)
		}
	}
}

// A refused endpoint write is the write's error and the reconcile's. The
// endpoint pass, which only collects it, and the status write that records the
// failure carry none: it is neither's own.
func TestAutoConfigReconcile_AFailedWriteMarksOnlyItsOwnStage(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM(), testGateway()).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
					return errors.New("endpoint refused")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()

	if _, err := reconcileAC(r, ac); err == nil || !strings.Contains(err.Error(), "endpoint refused") {
		t.Fatalf("Reconcile error = %v, want the refused write", err)
	}

	want := map[string]codes.Code{
		"reconcile KrakenDAutoConfig": codes.Error, "autoconfig.write_endpoint": codes.Error,
		"autoconfig.endpoints": codes.Unset, "autoconfig.status": codes.Unset,
	}
	requireCodes(t, rec.Ended(), want)
	rec.Ended().RequireParent(t, "reconcile KrakenDAutoConfig", "autoconfig.status")
}

// A refused status patch is the status stage's error and the reconcile's; the
// stage that resolved the references before it carries none.
func TestEndpointReconcile_AFailedStatusWriteMarksOnlyItsOwnStage(t *testing.T) {
	ep := endpointOnGW1(1)
	c := fakeClientBuilder().WithObjects(testGW1(), ep).WithStatusSubresource(ep).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption,
			) error {
				return errors.New("status refused")
			},
		}).Build()
	rec := tracingtest.New(t)
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder(), Tracer: rec.Tracer()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ep)}); err == nil {
		t.Fatal("a refused status patch must fail the reconcile")
	}

	want := map[string]codes.Code{
		"reconcile KrakenDEndpoint": codes.Error, "endpoint.status": codes.Error, "endpoint.resolve_refs": codes.Unset,
	}
	requireCodes(t, rec.Ended(), want)
}

// A refused status update is the status stage's error and the reconcile's; the
// protection stage before it carries none.
func TestPolicyReconcile_AFailedStatusWriteMarksOnlyItsOwnStage(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption,
			) error {
				return errors.New("status refused")
			},
		}).Build()
	rec := tracingtest.New(t)
	r := &KrakenDBackendPolicyReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(), APIReader: c, Tracer: rec.Tracer(),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err == nil {
		t.Fatal("a refused status update must fail the reconcile")
	}

	want := map[string]codes.Code{
		"reconcile KrakenDBackendPolicy": codes.Error, "policy.status": codes.Error, "policy.protection": codes.Unset,
	}
	requireCodes(t, rec.Ended(), want)
}

// rejectingExecutor stands in for a krakend binary that refuses every config
// that mentions its marker, and accepts the rest.
type rejectingExecutor struct{ marker string }

func (e rejectingExecutor) Execute(_ context.Context, _ string, args ...string) ([]byte, error) {
	config, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		return nil, err
	}
	if !strings.Contains(string(config), e.marker) {
		return []byte("Syntax OK!"), nil
	}
	return []byte("refused"), exec.Command("sh", "-c", "exit 1").Run()
}

// A sync that holds a candidate checks the rest again for router clashes, and
// judges the candidates on their own: both run under the precheck and its
// judging stage, as the first pass does.
func TestAutoConfigReconcile_AHeldCandidateIsRecheckedUnderThePrecheck(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM(), testGateway()).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	orders := g.output.Endpoints[0].DeepCopy()
	orders.Name = "test-ac-listorders"
	orders.Spec.Endpoints[0].Endpoint = "/api/orders"
	orders.Spec.Endpoints[0].Backends[0].URLPattern = "/api/orders"
	g.output.Endpoints = append(g.output.Endpoints, orders)
	r := newACReconciler(c, f, ce, fi, g)
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()
	v := renderer.NewValidator(renderer.ValidatorOptions{
		Executor: telemetry.TraceExecutor(rejectingExecutor{marker: "/api/users"}, rec.Tracer()), BinaryPath: "krakend",
	})
	r.Checker = configcheck.New(c, renderer.New(renderer.Options{}), v, 1, rec.Tracer())

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	if got := len(spans.Named("configcheck.Conflicts")); got != 4 {
		t.Errorf("%d configcheck.Conflicts spans, want 4: the clash check, then once more without the held candidate", got)
	}
	spans.RequireParent(t, "autoconfig.precheck", "configcheck.Conflicts")
	spans.RequireParent(t, "autoconfig.judge_candidates", "configcheck.CheckEndpoint")
	spans.RequireParent(t, "reconcile KrakenDAutoConfig", "autoconfig.status")
}

// The wait for a check slot is a span under the caller's, and it has ended
// once the slot is taken: the check is not part of it.
func TestWithCheckSlot_TheWaitEndsBeforeTheCheckRuns(t *testing.T) {
	rec := tracingtest.New(t)
	ctx, outer := rec.Tracer().Start(context.Background(), "outer")
	waitEnded := false

	_, err := withCheckSlot(ctx, rec.Tracer(), make(chan struct{}, 1), func() (struct{}, error) {
		waitEnded = len(rec.Ended().Named("autoconfig.slot")) == 1
		return struct{}{}, nil
	})
	outer.End()

	if err != nil {
		t.Fatal(err)
	}
	if !waitEnded {
		t.Error("the autoconfig.slot span was still open while the check ran")
	}
	spans := rec.Ended()
	spans.RequireParent(t, "outer", "autoconfig.slot")
	if got := spans.One(t, "autoconfig.slot").Status().Code; got != codes.Unset {
		t.Errorf("autoconfig.slot status = %v, want Unset", got)
	}
}

// A wait that gives up when its context ends records why on its span.
func TestWithCheckSlot_AGivenUpWaitIsAnErrorOnItsSpan(t *testing.T) {
	rec := tracingtest.New(t)
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := withCheckSlot(ctx, rec.Tracer(), slots, func() (struct{}, error) { return struct{}{}, nil })

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if got := rec.Ended().One(t, "autoconfig.slot").Status().Code; got != codes.Error {
		t.Errorf("autoconfig.slot status = %v, want Error", got)
	}
}

// The fetch of an AutoConfig's spec is a span of the fetch stage: here a spec
// in a ConfigMap, read by the real fetcher.
func TestAutoConfigReconcile_TheSpecFetchIsASpanOfTheFetchStage(t *testing.T) {
	ac := testAutoConfig()
	ac.Spec.OpenAPI = v1alpha1.OpenAPISource{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "spec", Key: "openapi.json"}}
	spec := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "spec", Namespace: ac.Namespace},
		Data:       map[string]string{"openapi.json": `{"openapi":"3.0.0","paths":{}}`},
	}
	c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM(), testGateway(), spec).
		WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()
	r.Fetcher = autoconfig.NewFetcher(c, rec.Tracer())

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}

	rec.Ended().RequireChild(t, "autoconfig.fetch_spec", "autoconfig.fetch")
}

// tracedFetcher fetches through next, each fetch a span of tracer started from
// the context it is given, named as the real fetcher's is.
type tracedFetcher struct {
	next   autoconfig.Fetcher
	tracer trace.Tracer
}

func (f tracedFetcher) Fetch(ctx context.Context, source autoconfig.FetchSource) (*autoconfig.FetchResult, error) {
	ctx, span := f.tracer.Start(ctx, "autoconfig.fetch")
	defer span.End()
	return f.next.Fetch(ctx, source)
}

// A document an external $ref names is fetched below the $ref resolution; the
// spec itself, below the fetch stage.
func TestAutoConfigReconcile_ARefFetchIsASpanOfTheRefResolution(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM(), testGateway()).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	f.result.Data = []byte(
		`{"paths":{"/a":{"get":{"responses":{"200":{"$ref":"https://schemas.example.com/frag.json#/A"}}}}}}`,
	)
	f.byURL = map[string]mockFetchOutcome{"https://schemas.example.com/frag.json": {
		result: &autoconfig.FetchResult{Data: []byte(`{"A":{"description":"ok"}}`)},
	}}
	r := newACReconciler(c, f, ce, fi, g)
	rec := tracingtest.New(t)
	r.Tracer = rec.Tracer()
	r.Fetcher = tracedFetcher{next: f, tracer: rec.Tracer()}

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	parents := map[string]int{}
	for _, fetch := range spans.Named("autoconfig.fetch") {
		if parent := spans.Parent(fetch); parent != nil {
			parents[parent.Name()]++
		}
	}
	if parents["autoconfig.fetch_spec"] != 1 || parents["autoconfig.resolve_refs"] != 1 {
		t.Errorf("fetches by parent = %v, want the spec's below autoconfig.fetch_spec and the $ref's below "+
			"autoconfig.resolve_refs; spans: %s", parents, spans)
	}
}

// A CUE evaluation or a generation failure can quote the AutoConfig's spec or
// the fetched document. The span of the stage that failed says only that it
// did: it carries no tenant value in its status and records no exception.
func TestAutoConfigReconcile_AFailedStageSpanCarriesNoTenantText(t *testing.T) {
	for _, tc := range []struct {
		span  string
		setup func(ce *mockCUEEvaluator, g *mockGenerator)
	}{
		{"autoconfig.evaluate", func(ce *mockCUEEvaluator, _ *mockGenerator) { ce.err = errors.New("SPECSECRET") }},
		{"autoconfig.generate", func(_ *mockCUEEvaluator, g *mockGenerator) { g.err = errors.New("SPECSECRET") }},
	} {
		t.Run(tc.span, func(t *testing.T) {
			ac := testAutoConfig()
			c := fakeClientBuilder().WithObjects(ac, testCUEDefinitionsCM()).WithStatusSubresource(ac).Build()
			f, ce, fi, g := defaultMocks()
			tc.setup(ce, g)
			r := newACReconciler(c, f, ce, fi, g)
			rec := tracingtest.New(t)
			r.Tracer = rec.Tracer()

			if _, err := reconcileAC(r, ac); err == nil || !strings.Contains(err.Error(), "SPECSECRET") {
				t.Fatalf("Reconcile error = %v, want the failure itself, unchanged", err)
			}

			span := rec.Ended().One(t, tc.span)
			if got := span.Status().Code; got != codes.Error {
				t.Errorf("%s status = %v, want %v", tc.span, got, codes.Error)
			}
			if desc := span.Status().Description; desc == "" || strings.Contains(desc, "SPECSECRET") {
				t.Errorf("%s status description = %q, want fixed text without the tenant's text", tc.span, desc)
			}
			if events := span.Events(); len(events) != 0 {
				t.Errorf("%s has events %v, want none", tc.span, events)
			}
			requireNoText(t, rec.Ended(), "SPECSECRET")
		})
	}
}

// requireNoText fails t when any ended span's status description, attributes
// or events, with their attributes, contain text.
func requireNoText(t *testing.T, spans tracingtest.Spans, text string) {
	t.Helper()
	for _, span := range spans {
		if strings.Contains(span.Status().Description, text) {
			t.Errorf("span %q status description %q contains %q", span.Name(), span.Status().Description, text)
		}
		for _, kv := range span.Attributes() {
			if strings.Contains(kv.Value.Emit(), text) {
				t.Errorf("span %q attribute %s contains %q", span.Name(), kv.Key, text)
			}
		}
		for _, ev := range span.Events() {
			for _, kv := range ev.Attributes {
				if strings.Contains(kv.Value.Emit(), text) {
					t.Errorf("span %q event %q attribute %s contains %q", span.Name(), ev.Name, kv.Key, text)
				}
			}
		}
	}
}
