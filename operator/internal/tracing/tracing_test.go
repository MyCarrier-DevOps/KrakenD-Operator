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
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

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
