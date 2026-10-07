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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

const hookPath = "/validate-gateway-krakend-io-v1alpha1-krakendendpoint"

// serveOnce registers a hook that starts a child span, through a traced
// webhook server, and sends it one request with the given traceparent.
func serveOnce(t *testing.T, rec *tracingtest.Recorder, traceparent string) {
	t.Helper()
	srv := telemetry.TraceWebhookServer(webhook.NewServer(webhook.Options{}), rec.Provider())
	srv.Register(hookPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, span := tracing.Start(r.Context(), rec.Tracer(), "admission.validate")
		span.End()
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, hookPath, strings.NewReader("{}"))
	if traceparent != "" {
		req.Header.Set("Traceparent", traceparent)
	}
	srv.WebhookMux().ServeHTTP(httptest.NewRecorder(), req)
}

func TestTraceWebhookServer_RequestIsAServerSpanAboveTheValidator(t *testing.T) {
	rec := tracingtest.New(t)

	serveOnce(t, rec, "")

	spans := rec.Ended()
	spans.RequireChild(t, "admission "+hookPath, "admission.validate")
	if parent := spans.One(t, "admission "+hookPath).Parent(); parent.IsValid() {
		t.Errorf("server span has parent %v, want a new trace when none is propagated", parent)
	}
}

func TestTraceWebhookServer_ContinuesThePropagatedTrace(t *testing.T) {
	rec := tracingtest.New(t)
	const traceID, parentID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"

	serveOnce(t, rec, "00-"+traceID+"-"+parentID+"-01")

	parent := rec.Ended().One(t, "admission "+hookPath).Parent()
	if parent.TraceID().String() != traceID || parent.SpanID().String() != parentID {
		t.Errorf("server span parent = %s/%s, want the propagated %s/%s",
			parent.TraceID(), parent.SpanID(), traceID, parentID)
	}
}

func TestTraceWebhookServer_HandlerContextCarriesTheServerSpan(t *testing.T) {
	rec := tracingtest.New(t)
	var seen trace.Span
	srv := telemetry.TraceWebhookServer(webhook.NewServer(webhook.Options{}), rec.Provider())
	srv.Register(hookPath, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = trace.SpanFromContext(r.Context())
	}))

	srv.WebhookMux().ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, hookPath, strings.NewReader("{}")))

	server := rec.Ended().One(t, "admission "+hookPath).SpanContext()
	if got := seen.SpanContext(); got.TraceID() != server.TraceID() || got.SpanID() != server.SpanID() {
		t.Errorf("handler context span = %s/%s, want the server span %s/%s",
			got.TraceID(), got.SpanID(), server.TraceID(), server.SpanID())
	}
}
