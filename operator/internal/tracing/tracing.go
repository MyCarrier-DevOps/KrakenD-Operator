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

// Package tracing holds the span helpers the controllers, the admission
// webhooks and the config checker share. It depends only on the OpenTelemetry
// API: the SDK is wired in cmd.
package tracing

import (
	"context"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Attribute keys for the Kubernetes object a reconcile or an admission
// request is about. The semantic conventions name the namespace
// (k8s.namespace.name) but have no generic object keys, so the rest are the
// operator's own.
const (
	KeyKind       = attribute.Key("k8s.object.kind")
	KeyName       = attribute.Key("k8s.object.name")
	KeyGeneration = attribute.Key("k8s.object.generation")
)

// logContextKey is the logger value the OpenTelemetry log bridge reads a
// record's context from.
const logContextKey = "ctx"

// Start starts a span named name as a child of the span in ctx and returns a
// context that carries it. A nil tracer starts a no-op span. The logger in
// ctx, if any, is rebound to the new context, so a record logged through
// logr.FromContext (or controller-runtime's log.FromContext) carries this
// span's trace and span IDs.
func Start(
	ctx context.Context, tracer trace.Tracer, name string, opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}
	// The span is returned: the caller ends it.
	ctx, span := tracer.Start(ctx, name, opts...) //nolint:spancheck // see above
	return withLogContext(ctx), span              //nolint:spancheck // see above
}

// withLogContext rebinds the logger in ctx, if any, to ctx, so records it
// logs carry the span ctx holds.
func withLogContext(ctx context.Context) context.Context {
	logger, err := logr.FromContext(ctx)
	if err != nil {
		return ctx
	}
	return logr.NewContext(ctx, logger.WithValues(logContextKey, logContext{ctx}))
}

// End records err on span, when it is not nil, and ends span.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// EndFailed ends span, marking it an error with the fixed description when err
// is not nil. It records neither err's text nor an exception event: use it for
// a failure whose text can quote the tenant's values.
func EndFailed(span trace.Span, err error, description string) {
	if err != nil {
		span.SetStatus(codes.Error, description)
	}
	span.End()
}

// Object returns the attributes naming obj, of the given kind: its namespace,
// name, kind and generation.
func Object(kind string, obj client.Object) []attribute.KeyValue {
	return []attribute.KeyValue{
		semconv.K8SNamespaceName(obj.GetNamespace()),
		KeyName.String(obj.GetName()),
		KeyKind.String(kind),
		KeyGeneration.Int64(obj.GetGeneration()),
	}
}

// logContext carries a context to the OpenTelemetry log bridge, which emits a
// record with the context.Context it finds among a logger's values. Any other
// log sink prints it as the trace and span IDs.
type logContext struct{ context.Context }

// String returns the trace and span IDs the context carries.
func (c logContext) String() string {
	sc := trace.SpanContextFromContext(c.Context)
	return sc.TraceID().String() + "/" + sc.SpanID().String()
}
