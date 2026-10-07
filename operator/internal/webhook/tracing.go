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

package webhook

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// tracedValidator wraps a validator so each admission decision is a span
// named "admission.validate <kind>", a child of the request's server span,
// with the object's identity and the operation. A denial ends the span with
// an error status.
type tracedValidator struct {
	kind   string
	next   admission.CustomValidator
	tracer trace.Tracer
}

// ValidateCreate validates obj inside a span.
func (v tracedValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (_ admission.Warnings, retErr error) {
	ctx, span := v.start(ctx, "CREATE", obj)
	defer func() { tracing.End(span, retErr) }()
	return v.next.ValidateCreate(ctx, obj)
}

// ValidateUpdate validates newObj inside a span.
func (v tracedValidator) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (_ admission.Warnings, retErr error) {
	ctx, span := v.start(ctx, "UPDATE", newObj)
	defer func() { tracing.End(span, retErr) }()
	return v.next.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete validates obj inside a span.
func (v tracedValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (_ admission.Warnings, retErr error) {
	ctx, span := v.start(ctx, "DELETE", obj)
	defer func() { tracing.End(span, retErr) }()
	return v.next.ValidateDelete(ctx, obj)
}

func (v tracedValidator) start(
	ctx context.Context, operation string, obj runtime.Object,
) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{attribute.String("k8s.admission.operation", operation)}
	if o, ok := obj.(client.Object); ok {
		attrs = append(attrs, tracing.Object(v.kind, o)...)
	}
	if req, err := admission.RequestFromContext(ctx); err == nil {
		attrs = append(attrs, attribute.Bool("k8s.admission.dry_run", req.DryRun != nil && *req.DryRun),
			attribute.String("k8s.admission.uid", string(req.UID)))
	}
	return tracing.Start(ctx, v.tracer, "admission.validate "+v.kind, trace.WithAttributes(attrs...))
}
