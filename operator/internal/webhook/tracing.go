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

	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// tracedValidator wraps a validator so each admission decision is a span. It
// is a stub: it only calls next.
type tracedValidator struct {
	kind   string
	next   admission.CustomValidator
	tracer trace.Tracer
}

// ValidateCreate calls next.
func (v tracedValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.next.ValidateCreate(ctx, obj)
}

// ValidateUpdate calls next.
func (v tracedValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return v.next.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete calls next.
func (v tracedValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.next.ValidateDelete(ctx, obj)
}
