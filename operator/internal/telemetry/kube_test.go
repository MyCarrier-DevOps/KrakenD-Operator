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

package telemetry_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// fakeAPIServer answers every request with the same ConfigMap and keeps the
// traceparent header of each request it got.
type fakeAPIServer struct {
	mu           sync.Mutex
	traceparents []string
}

func (s *fakeAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.traceparents = append(s.traceparents, r.Header.Get("Traceparent"))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"ns","name":"cm"}}`)
}

// tracedClient returns a client for a fake API server whose config went
// through TraceKubeAPI, and the server.
func tracedClient(t *testing.T, rec *tracingtest.Recorder) (client.Client, *fakeAPIServer) {
	t.Helper()
	return tracedClientAt(t, rec, "")
}

// tracedClientAt is tracedClient for a host with a path prefix.
func tracedClientAt(t *testing.T, rec *tracingtest.Recorder, prefix string) (client.Client, *fakeAPIServer) {
	t.Helper()
	return tracedClientWith(t, rec.Provider(), prefix)
}

// tracedClientWith is tracedClientAt for any tracer provider.
func tracedClientWith(t *testing.T, tp trace.TracerProvider, prefix string) (client.Client, *fakeAPIServer) {
	t.Helper()
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL + prefix}
	telemetry.TraceKubeAPI(cfg, tp)
	// A static mapper: the fake server answers no discovery request.
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
	c, err := client.New(cfg, client.Options{Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	return c, api
}

func updateConfigMap(ctx context.Context, c client.Client) error {
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm"},
	}
	return c.Update(ctx, cm)
}

func TestTraceKubeAPI_RequestUnderASpanIsItsChildAndCarriesTheTrace(t *testing.T) {
	rec := tracingtest.New(t)
	c, api := tracedClient(t, rec)
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	if err := updateConfigMap(ctx, c); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := rec.Ended()
	spans.RequireChild(t, "reconcile", "k8s update configmaps")
	sc := spans.One(t, "k8s update configmaps").SpanContext()
	want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"
	if len(api.traceparents) != 1 || api.traceparents[0] != want {
		t.Errorf("API server got traceparent headers %q, want [%s] (the client span)", api.traceparents, want)
	}
}

func TestTraceKubeAPI_RequestWithoutASpanStartsNoTrace(t *testing.T) {
	rec := tracingtest.New(t)
	c, api := tracedClient(t, rec)

	if err := updateConfigMap(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	if got := rec.Ended(); len(got) != 0 {
		t.Errorf("spans = %s, want none for a request with no span", got)
	}
	if len(api.traceparents) != 1 || api.traceparents[0] != "" {
		t.Errorf("API server got traceparent headers %q, want none", api.traceparents)
	}
}

func TestReadEvents_AReadAddsAnEventToTheActiveSpan(t *testing.T) {
	rec := tracingtest.New(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm"}}
	c := telemetry.ReadEvents(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).Build())
	ctx, span := rec.Tracer().Start(context.Background(), "reconcile")

	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); err != nil {
		t.Fatal(err)
	}
	span.End()

	events := rec.Ended().One(t, "reconcile").Events()
	if len(events) != 1 || events[0].Name != "k8s.client.get" {
		t.Fatalf("events = %+v, want one k8s.client.get", events)
	}
	attrs := map[string]string{}
	for _, kv := range events[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["k8s.object.kind"] != "ConfigMap" || attrs["k8s.namespace.name"] != "ns" ||
		attrs["k8s.object.name"] != "cm" || attrs["k8s.client.found"] != "true" {
		t.Errorf("event attributes = %v, want ConfigMap ns/cm found", attrs)
	}
}

func TestTraceKubeAPI_NoAttributeCarriesAQueryValue(t *testing.T) {
	rec := tracingtest.New(t)
	c, _ := tracedClient(t, rec)
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	// The fake answers a ConfigMap, so decoding the list may fail; the
	// request is what matters.
	_ = c.List(ctx, &corev1.ConfigMapList{}, client.InNamespace("ns"),
		client.MatchingLabels{"secret-key": "secret-value"})
	parent.End()

	for _, kv := range rec.Ended().One(t, "k8s list configmaps").Attributes() {
		if value := kv.Value.Emit(); strings.Contains(value, "secret-value") {
			t.Errorf("attribute %s = %q carries a query value", kv.Key, value)
		}
	}
}

// A bare query key is a token as much as a value is, and a semicolon separates
// pairs: the URL on the span follows the same rules as every other recorded URL.
func TestTraceKubeAPI_URLFullRedactsBareKeysAndSemicolonPairs(t *testing.T) {
	rec := tracingtest.New(t)
	srv := httptest.NewServer(&fakeAPIServer{})
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	telemetry.TraceKubeAPI(cfg, rec.Provider())
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/v1/namespaces/ns/configmaps?SECRETKEY&a=1;b=2", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	parent.End()

	got, _ := tracingtest.Attr(rec.Ended().One(t, "k8s list configmaps"), "url.full")
	if !strings.HasSuffix(got.AsString(), "/configmaps?REDACTED&a=REDACTED&b=REDACTED") {
		t.Errorf("url.full = %q, want the bare key and each value REDACTED", got)
	}
}

func TestTraceKubeAPI_PrefixedHostStillNamesTheRequestByItsResource(t *testing.T) {
	rec := tracingtest.New(t)
	c, _ := tracedClientAt(t, rec, "/k8s/clusters/c-1")
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	if err := updateConfigMap(ctx, c); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := rec.Ended()
	for _, span := range spans {
		if strings.Contains(span.Name(), "ns") || strings.Contains(span.Name(), "cm") {
			t.Errorf("span name %q carries the namespace or object name", span.Name())
		}
	}
	span := spans.One(t, "k8s update configmaps")
	resource, _ := tracingtest.Attr(span, "k8s.resource")
	verb, _ := tracingtest.Attr(span, "k8s.verb")
	if resource.AsString() != "configmaps" || verb.AsString() != "update" {
		t.Errorf("k8s.resource, k8s.verb = %v, %v, want the configmaps update", resource, verb)
	}
}

func TestTraceKubeAPI_ClientSpanRecordsTheRequestAndTheParentDoesNot(t *testing.T) {
	rec := tracingtest.New(t)
	c, _ := tracedClient(t, rec)
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	if err := updateConfigMap(ctx, c); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := rec.Ended()
	clientSpan := spans.One(t, "k8s update configmaps")
	for key, want := range map[string]string{
		"k8s.namespace.name": "ns", "k8s.object.name": "cm", "k8s.resource": "configmaps", "k8s.verb": "update",
	} {
		if got, _ := tracingtest.Attr(clientSpan, key); got.AsString() != want {
			t.Errorf("client span %s = %q, want %q", key, got.AsString(), want)
		}
	}
	for _, kv := range spans.One(t, "reconcile").Attributes() {
		if strings.HasPrefix(string(kv.Key), "k8s.") {
			t.Errorf("parent span carries %s", kv.Key)
		}
	}
}

func TestTraceKubeAPI_StatusUpdateIsNamedWithItsSubresource(t *testing.T) {
	rec := tracingtest.New(t)
	c, _ := tracedClient(t, rec)
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm"}}
	if err := c.Status().Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	parent.End()

	rec.Ended().RequireChild(t, "reconcile", "k8s update configmaps/status")
}

func TestTraceKubeAPI_ClusterScopedRequestIsNamedByItsResource(t *testing.T) {
	rec := tracingtest.New(t)
	c, _ := tracedClient(t, rec)
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	// The fake answers a ConfigMap, so decoding may fail; the request is
	// what matters.
	_ = c.Update(ctx, &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
	})
	parent.End()

	rec.Ended().RequireChild(t, "reconcile", "k8s update namespaces")
}

func TestTraceKubeAPI_NonResourceRequestIsNamedByMethodAndPath(t *testing.T) {
	rec := tracingtest.New(t)
	srv := httptest.NewServer(&fakeAPIServer{})
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	telemetry.TraceKubeAPI(cfg, rec.Provider())
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/apis", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	parent.End()

	rec.Ended().RequireChild(t, "reconcile", "k8s GET /apis")
}

// eventAttrs returns the attributes of the only event of the span named name.
func eventAttrs(t *testing.T, rec *tracingtest.Recorder, name, event string) map[string]string {
	t.Helper()
	events := rec.Ended().One(t, name).Events()
	if len(events) != 1 || events[0].Name != event {
		t.Fatalf("events = %+v, want one %s", events, event)
	}
	attrs := map[string]string{}
	for _, kv := range events[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	return attrs
}

func TestReadEvents_AListAddsAnEventToTheActiveSpan(t *testing.T) {
	rec := tracingtest.New(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm"}}
	c := telemetry.ReadEvents(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(cm).Build())
	ctx, span := rec.Tracer().Start(context.Background(), "reconcile")

	if err := c.List(ctx, &corev1.ConfigMapList{}, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	span.End()

	attrs := eventAttrs(t, rec, "reconcile", "k8s.client.list")
	if attrs["k8s.object.kind"] != "ConfigMap" || attrs["k8s.namespace.name"] != "ns" || attrs["k8s.client.succeeded"] != "true" {
		t.Errorf("event attributes = %v, want kind ConfigMap, namespace ns, succeeded", attrs)
	}
}

func TestTraceKubeAPI_UnsampledParentRecordsNoSpanAndPassesTheDecisionOn(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())),
		sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	c, api := tracedClientWith(t, tp, "")
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("1112131415161718")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, Remote: true, // no TraceFlags: not sampled
	}))

	if err := updateConfigMap(ctx, c); err != nil {
		t.Fatal(err)
	}

	if got := exporter.GetSpans(); len(got) != 0 {
		t.Errorf("spans = %v, want none under an unsampled parent", got)
	}
	if len(api.traceparents) != 1 || !strings.HasPrefix(api.traceparents[0], "00-"+traceID.String()+"-") ||
		!strings.HasSuffix(api.traceparents[0], "-00") {
		t.Errorf("API server got traceparent headers %q, want one for the parent's trace with flags 00", api.traceparents)
	}
}

func TestTraceKubeAPI_ListRecordsNoEmptyObjectName(t *testing.T) {
	rec := tracingtest.New(t)
	c, _ := tracedClient(t, rec)
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	// The fake answers a ConfigMap, so decoding the list may fail; the
	// request is what matters.
	_ = c.List(ctx, &corev1.ConfigMapList{}, client.InNamespace("ns"))
	parent.End()

	if name, ok := tracingtest.Attr(rec.Ended().One(t, "k8s list configmaps"), "k8s.object.name"); ok {
		t.Errorf("k8s.object.name = %q on a list, want the attribute omitted", name.AsString())
	}
}

func TestReadEvents_AGetThatFailsForAnotherReasonRecordsItsErrorType(t *testing.T) {
	rec := tracingtest.New(t)
	c := telemetry.ReadEvents(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return errors.New("connection refused")
			},
		}).Build())
	ctx, span := rec.Tracer().Start(context.Background(), "reconcile")

	err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "cm"}, &corev1.ConfigMap{})
	span.End()

	if err == nil {
		t.Fatal("Get succeeded, want the interceptor's error")
	}
	attrs := eventAttrs(t, rec, "reconcile", "k8s.client.get")
	if _, ok := attrs["k8s.client.found"]; ok || attrs["error.type"] == "" {
		t.Errorf("event attributes = %v, want error.type and no found for an error that is not NotFound", attrs)
	}
}

func TestReadEvents_AGetOfAMissingObjectRecordsFoundFalse(t *testing.T) {
	rec := tracingtest.New(t)
	c := telemetry.ReadEvents(fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build())
	ctx, span := rec.Tracer().Start(context.Background(), "reconcile")

	err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "cm"}, &corev1.ConfigMap{})
	span.End()

	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get = %v, want NotFound", err)
	}
	attrs := eventAttrs(t, rec, "reconcile", "k8s.client.get")
	if _, ok := attrs["error.type"]; ok || attrs["k8s.client.found"] != "false" {
		t.Errorf("event attributes = %v, want found=false and no error.type", attrs)
	}
}
