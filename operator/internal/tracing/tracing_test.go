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

package tracing_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"go.opentelemetry.io/otel/codes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// A record logged through the context's logger inside a span names that
// span: the OpenTelemetry log bridge reads its context from the logger's
// values, and other sinks print the trace and span IDs.
func TestStart_RebindsTheContextLoggerToTheNewSpan(t *testing.T) {
	rec := tracingtest.New(t)
	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	ctx := logr.NewContext(context.Background(), logger)

	ctx, span := tracing.Start(ctx, rec.Tracer(), "child")
	logr.FromContextOrDiscard(ctx).Info("inside")
	span.End()

	if len(lines) != 1 || !strings.Contains(lines[0], span.SpanContext().SpanID().String()) {
		t.Errorf("log lines = %q, want one carrying span ID %s", lines, span.SpanContext().SpanID())
	}
}

func TestStart_NilTracerStartsANoOpSpan(t *testing.T) {
	_, span := tracing.Start(context.Background(), nil, "anything")
	defer span.End()

	if span.IsRecording() {
		t.Error("a nil tracer started a recording span")
	}
}

func TestEnd_RecordsTheErrorAsTheSpanStatus(t *testing.T) {
	rec := tracingtest.New(t)
	_, span := tracing.Start(context.Background(), rec.Tracer(), "failing")

	tracing.End(span, errors.New("boom"))

	got := rec.Ended().One(t, "failing")
	if got.Status().Code != codes.Error || got.Status().Description != "boom" || len(got.Events()) != 1 {
		t.Errorf("status = %+v, events = %d; want Error \"boom\" and one exception event", got.Status(), len(got.Events()))
	}
}

func TestObject_NamesTheObject(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "gw", Generation: 3}}

	attrs := tracing.Object("KrakenDGateway", gw)

	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.String()
	}
	want := map[string]string{"k8s.namespace.name": "ns", "k8s.object.name": "gw",
		"k8s.object.kind": "KrakenDGateway", "k8s.object.generation": "3"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestEnd_LeavesASuccessfulSpanUnset(t *testing.T) {
	rec := tracingtest.New(t)
	_, span := tracing.Start(context.Background(), rec.Tracer(), "ok")
	_, noop := tracing.Start(context.Background(), nil, "ok")

	tracing.End(span, nil)
	tracing.End(noop, nil)

	got := rec.Ended().One(t, "ok")
	if got.Status().Code != codes.Unset || len(got.Events()) != 0 {
		t.Errorf("status = %+v, events = %d; want Unset and no events", got.Status(), len(got.Events()))
	}
}

func TestStart_NestedKeepsTheLoggerNameAndValues(t *testing.T) {
	rec := tracingtest.New(t)
	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{}).
		WithName("controller").WithValues("gateway", "gw")
	ctx := logr.NewContext(context.Background(), logger)

	ctx, outer := tracing.Start(ctx, rec.Tracer(), "outer")
	ctx, inner := tracing.Start(ctx, rec.Tracer(), "inner")
	logr.FromContextOrDiscard(ctx).Info("inside")
	inner.End()
	outer.End()

	if len(lines) != 1 {
		t.Fatalf("log lines = %q, want one", lines)
	}
	for _, want := range []string{"controller", `"gateway"="gw"`, inner.SpanContext().SpanID().String()} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %q lacks %q", lines[0], want)
		}
	}
}
