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

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	ctrl "sigs.k8s.io/controller-runtime"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// startReconcile starts the root span of one reconcile of the object req
// names, of the given kind: a new trace, with the object's namespace, name
// and kind, and controller-runtime's reconcile ID, which every log line of the
// reconcile carries too.
func startReconcile(
	ctx context.Context, tracer trace.Tracer, kind string, req ctrl.Request,
) (context.Context, trace.Span) {
	return tracing.Start(ctx, tracer, "reconcile "+kind, trace.WithNewRoot(), trace.WithAttributes(
		semconv.K8SNamespaceName(req.Namespace), tracing.KeyName.String(req.Name), tracing.KeyKind.String(kind),
		attribute.String("controller_runtime.reconcile_id", string(crcontroller.ReconcileIDFromContext(ctx)))))
}

// spanGeneration records, on the reconcile's span, the generation of the
// object it read.
func spanGeneration(ctx context.Context, generation int64) {
	trace.SpanFromContext(ctx).SetAttributes(tracing.KeyGeneration.Int64(generation))
}
