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
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// tracedValidator wraps a validator so each admission decision is a span
// named "admission.validate <kind>", a child of the request's server span,
// with the object's identity and the operation. A denial is the validator's
// answer, not a failure: it ends the span without an error status.
type tracedValidator struct {
	kind   string
	next   admission.CustomValidator
	tracer trace.Tracer
}

// ValidateCreate validates obj inside a span.
func (v tracedValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (_ admission.Warnings, retErr error) {
	ctx, span := v.start(ctx, "CREATE", obj)
	defer func() { v.finish(span, retErr) }()
	return v.next.ValidateCreate(ctx, obj)
}

// ValidateUpdate validates newObj inside a span.
func (v tracedValidator) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (_ admission.Warnings, retErr error) {
	ctx, span := v.start(ctx, "UPDATE", newObj)
	defer func() { v.finish(span, retErr) }()
	return v.next.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete validates obj inside a span.
func (v tracedValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (_ admission.Warnings, retErr error) {
	ctx, span := v.start(ctx, "DELETE", obj)
	defer func() { v.finish(span, retErr) }()
	return v.next.ValidateDelete(ctx, obj)
}

// finish says on span whether the request was allowed and the code the API
// server gets, as controller-runtime derives it from err, and ends it. Never
// why: the denial's text can quote the tenant's object.
func (v tracedValidator) finish(span trace.Span, err error) {
	code := http.StatusOK
	if err != nil {
		code = http.StatusForbidden // a plain error is a denial with no status
		var status apierrors.APIStatus
		if errors.As(err, &status) {
			code = int(status.Status().Code)
		}
	}
	span.SetAttributes(attribute.Bool("admission.allowed", err == nil), attribute.Int("admission.code", code))
	endDecision(span, err)
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

// decisionFailed is the description of a span whose decision could not be
// reached. The spans that record only this leave the failure's text to the
// config check's span and, for a plain error, the rules' span; a failure
// raised anywhere else (a 500 that wraps a lookup) leaves no text in the trace.
const decisionFailed = "the admission could not be decided"

// rulesFailed is the description of a rules span whose failure was a status
// error: its text can carry the tenant's values.
const rulesFailed = "the rules could not be evaluated"

// endDecision ends span, the span of a decision that returned err. The span
// records neither a denial (see isDenial), which is an answer and can quote the
// tenant's object, nor the text of a failure, which can too: a failure to
// decide only marks the span an error.
func endDecision(span trace.Span, err error) {
	if isDenial(err) {
		err = nil
	}
	tracing.EndFailed(span, err, decisionFailed)
}

// endRules ends span, the span of rules that return err. A denial is an
// answer and is not recorded. Another status error is only marked, as its text
// can carry the tenant's values; any other error is the rules' own failure.
func endRules(span trace.Span, err error) {
	var status apierrors.APIStatus
	switch {
	case isDenial(err):
		span.End()
	case errors.As(err, &status):
		tracing.EndFailed(span, err, rulesFailed)
	default:
		tracing.End(span, err)
	}
}

// isDenial reports whether err is a validator's refusal of the request: a
// status error, as controller-runtime reads it to set the response code, with
// a 4xx code. A 500 (a check that could not run) is not, even when it reports
// a denial.
func isDenial(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	code := status.Status().Code
	return code >= http.StatusBadRequest && code < http.StatusInternalServerError
}
