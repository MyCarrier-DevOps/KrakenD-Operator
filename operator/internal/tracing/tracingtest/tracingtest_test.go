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

package tracingtest_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

func TestAttr_FindsAnAttributeByKeyAndSaysWhenThereIsNone(t *testing.T) {
	rec := tracingtest.New(t)
	_, span := rec.Tracer().Start(context.Background(), "s", trace.WithAttributes(
		attribute.Int64("a.count", 3), attribute.Bool("a.flag", false)))
	span.End()
	ended := rec.Ended().One(t, "s")

	if v, ok := tracingtest.Attr(ended, "a.count"); !ok || v.AsInt64() != 3 {
		t.Errorf("Attr(a.count) = %v, %v; want 3, true", v, ok)
	}
	if v, ok := tracingtest.Attr(ended, "a.flag"); !ok || v.AsBool() {
		t.Errorf("Attr(a.flag) = %v, %v; want false, true: a false value is still present", v, ok)
	}
	if v, ok := tracingtest.Attr(ended, "a.missing"); ok {
		t.Errorf("Attr(a.missing) = %v, true; want it absent", v)
	}
}
