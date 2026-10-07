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
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// stdoutRecord is the part of a stdout JSON record the tests read.
type stdoutRecord struct {
	SeverityText string
	Timestamp    string
	Body         struct{ Value string }
	TraceID      string
	SpanID       string
	Scope        struct{ Name string }
}

// newStdoutLogger returns a logger writing JSON records at or above minimum
// to the returned buffer.
func newStdoutLogger(t *testing.T, minimum otellog.Severity) (logr.Logger, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	stdout, err := telemetry.NewStdoutProcessor(&out, telemetry.LogFormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(telemetry.WithMinSeverity(stdout, minimum)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	return telemetry.NewLogger(lp, "test"), &out
}

// records decodes the JSON records written to out.
func records(t *testing.T, out *bytes.Buffer) []stdoutRecord {
	t.Helper()
	var got []stdoutRecord
	dec := json.NewDecoder(out)
	for dec.More() {
		var r stdoutRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	return got
}

// A reconcile or admission request that runs out of time logs why; the
// stdout exporter drops a record whose context is cancelled, so the pipeline
// must not hand it one.
func TestLogger_RecordWithACancelledContextStillReachesStdout(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)
	ctx, cancel := context.WithCancel(logf.IntoContext(context.Background(), logger))
	cancel()

	logf.FromContext(tracing.WithLogContext(ctx)).Info("after the deadline")

	if got := records(t, out); len(got) != 1 || got[0].Body.Value != "after the deadline" {
		t.Errorf("records = %+v, want the one logged after cancellation", got)
	}
}

// --zap-log-level=debug keeps V(1) and drops V(2), as zap did; a kept record
// carries a severity name and the time it was logged.
func TestLogger_KeepsVerbosityUpToTheLevel(t *testing.T) {
	minimum, err := telemetry.ParseLogLevel("debug")
	if err != nil {
		t.Fatal(err)
	}
	logger, out := newStdoutLogger(t, minimum)

	logger.V(1).Info("kept")
	logger.V(2).Info("dropped")

	got := records(t, out)
	if len(got) != 1 || got[0].Body.Value != "kept" || got[0].SeverityText != "DEBUG4" {
		t.Errorf("records = %+v, want only the V(1) one, as DEBUG4", got)
	}
	if len(got) == 1 && (got[0].Timestamp == "" || strings.HasPrefix(got[0].Timestamp, "0001-")) {
		t.Errorf("timestamp = %q, want the time it was logged", got[0].Timestamp)
	}
}
