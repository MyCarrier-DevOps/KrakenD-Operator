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

// Package tracingtest records spans in memory and asserts on their
// parent/child structure.
package tracingtest

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Recorder is a tracer provider that samples every span and keeps each one
// that ends.
type Recorder struct {
	provider *sdktrace.TracerProvider
	spans    *tracetest.SpanRecorder
}

// NewRecorder returns a Recorder. The caller shuts it down with Shutdown.
func NewRecorder() *Recorder {
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(spans), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	return &Recorder{provider: provider, spans: spans}
}

// New returns a Recorder that is shut down when t ends.
func New(t testing.TB) *Recorder {
	t.Helper()
	r := NewRecorder()
	t.Cleanup(func() {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return r
}

// Provider returns the recorder's tracer provider.
func (r *Recorder) Provider() trace.TracerProvider { return r.provider }

// Tracer returns a tracer of the recorder.
func (r *Recorder) Tracer() trace.Tracer { return r.provider.Tracer("test") }

// Ended returns the spans ended so far.
func (r *Recorder) Ended() Spans { return Spans(r.spans.Ended()) }

// Shutdown stops the recorder.
func (r *Recorder) Shutdown(ctx context.Context) error { return r.provider.Shutdown(ctx) }

// Spans are ended spans.
type Spans []sdktrace.ReadOnlySpan

// Named returns the spans named name, in the order they ended.
func (s Spans) Named(name string) Spans {
	var out Spans
	for _, span := range s {
		if span.Name() == name {
			out = append(out, span)
		}
	}
	return out
}

// With returns the spans that carry kv among their attributes.
func (s Spans) With(kv attribute.KeyValue) Spans {
	var out Spans
	for _, span := range s {
		for _, attr := range span.Attributes() {
			if attr == kv {
				out = append(out, span)
				break
			}
		}
	}
	return out
}

// Trace returns the spans of root's trace.
func (s Spans) Trace(root sdktrace.ReadOnlySpan) Spans {
	var out Spans
	for _, span := range s {
		if span.SpanContext().TraceID() == root.SpanContext().TraceID() {
			out = append(out, span)
		}
	}
	return out
}

// One returns the only span named name, failing t unless there is exactly one.
func (s Spans) One(t testing.TB, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	named := s.Named(name)
	if len(named) != 1 {
		t.Fatalf("%d spans named %q, want 1; spans: %s", len(named), name, s)
	}
	return named[0]
}

// Parent returns the ended span that is span's parent, or nil.
func (s Spans) Parent(span sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	parent := span.Parent()
	if !parent.IsValid() {
		return nil
	}
	for _, candidate := range s {
		if candidate.SpanContext().SpanID() == parent.SpanID() &&
			candidate.SpanContext().TraceID() == parent.TraceID() {
			return candidate
		}
	}
	return nil
}

// RequireChild fails t unless the only span named child has the only span
// named parent as its parent.
func (s Spans) RequireChild(t testing.TB, parent, child string) {
	t.Helper()
	p, c := s.One(t, parent), s.One(t, child)
	if got := s.Parent(c); got == nil || got.SpanContext().SpanID() != p.SpanContext().SpanID() {
		t.Errorf("span %q has parent %s, want %q; spans: %s", child, nameOf(got), parent, s)
	}
}

// RequireParent fails t unless there is a span named child and every span
// named child has a span named parent as its parent.
func (s Spans) RequireParent(t testing.TB, parent, child string) {
	t.Helper()
	named := s.Named(child)
	if len(named) == 0 {
		t.Fatalf("0 spans named %q, want at least 1; spans: %s", child, s)
	}
	for _, c := range named {
		if got := s.Parent(c); got == nil || got.Name() != parent {
			t.Errorf("span %q has parent %s, want %q; spans: %s", child, nameOf(got), parent, s)
		}
	}
}

// Find returns a span named descendant whose ancestors, walking up from it,
// include spans whose names start with each of prefixes in order (each an
// ancestor of the one before), or nil.
func (s Spans) Find(descendant string, prefixes ...string) sdktrace.ReadOnlySpan {
	for _, span := range s.Named(descendant) {
		if s.hasAncestors(span, prefixes) {
			return span
		}
	}
	return nil
}

// RequireAncestors fails t unless Find finds a span, and returns it.
func (s Spans) RequireAncestors(t testing.TB, descendant string, prefixes ...string) sdktrace.ReadOnlySpan {
	t.Helper()
	span := s.Find(descendant, prefixes...)
	if span == nil {
		t.Fatalf("no span %q under %q; spans: %s", descendant, strings.Join(prefixes, " < "), s)
	}
	return span
}

// String lists the spans as name<-parent pairs, for failure messages.
func (s Spans) String() string {
	names := make([]string, 0, len(s))
	for _, span := range s {
		names = append(names, span.Name()+"<-"+nameOf(s.Parent(span)))
	}
	return "[" + strings.Join(names, ", ") + "]"
}

func (s Spans) hasAncestors(span sdktrace.ReadOnlySpan, prefixes []string) bool {
	for _, prefix := range prefixes {
		span = s.Parent(span)
		for span != nil && !strings.HasPrefix(span.Name(), prefix) {
			span = s.Parent(span)
		}
		if span == nil {
			return false
		}
	}
	return true
}

func nameOf(span sdktrace.ReadOnlySpan) string {
	if span == nil {
		return "<root>"
	}
	return span.Name()
}

// Attr returns the value of span's attribute key, and whether it has one.
func Attr(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	return attribute.Value{}, false
}
