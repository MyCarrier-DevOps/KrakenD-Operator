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
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// stdoutRecord is the part of a stdout JSON record the tests read.
type stdoutRecord struct {
	SeverityText string
	Timestamp    string
	Body         struct{ Value string }
	TraceID      string
	SpanID       string
	Scope        struct{ Name string }
	Attributes   []struct {
		Key   string
		Value struct{ Value any }
	}
}

// newStdoutLogger returns a logger writing JSON records at or above minimum
// to the returned buffer.
func newStdoutLogger(t *testing.T, minimum otellog.Severity) (logr.Logger, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return newStdoutLoggerTo(t, &out, minimum), &out
}

// newStdoutLoggerTo returns a logger writing JSON records at or above minimum
// to w.
func newStdoutLoggerTo(t *testing.T, w io.Writer, minimum otellog.Severity) logr.Logger {
	t.Helper()
	stdout, err := telemetry.NewStdoutProcessor(w, telemetry.LogFormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(telemetry.WithMinSeverity(stdout, minimum)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	return telemetry.NewLogger(lp, "test")
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

	ctx, span := tracing.Start(ctx, nil, "reconcile")
	defer span.End()

	logf.FromContext(ctx).Info("after the deadline")

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

func TestLogger_RecordInAChildSpanCarriesThatSpansIDs(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)
	rec := tracingtest.New(t)
	ctx := logf.IntoContext(context.Background(), logger)
	ctx, parent := tracing.Start(ctx, rec.Tracer(), "parent")
	ctx, child := tracing.Start(ctx, rec.Tracer(), "child")

	logf.FromContext(ctx).Info("inside the child")
	child.End()
	parent.End()

	got := records(t, out)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	sc := child.SpanContext()
	if got[0].TraceID != sc.TraceID().String() || got[0].SpanID != sc.SpanID().String() {
		t.Errorf("record carries %s/%s, want the child's %s/%s",
			got[0].TraceID, got[0].SpanID, sc.TraceID(), sc.SpanID())
	}
}

func TestParseLogLevel(t *testing.T) {
	for value, want := range map[string]otellog.Severity{
		"debug": otellog.SeverityDebug4, "info": otellog.SeverityInfo, "error": otellog.SeverityError,
		"panic": otellog.SeverityFatal, "3": otellog.SeverityDebug2, "9": otellog.SeverityTrace,
	} {
		if got, err := telemetry.ParseLogLevel(value); err != nil || got != want {
			t.Errorf("ParseLogLevel(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"0", "-1", "warn", "verbose"} {
		if _, err := telemetry.ParseLogLevel(bad); err == nil {
			t.Errorf("ParseLogLevel(%q) accepted it", bad)
		}
	}
}

// restoreGlobals undoes, when the test ends, what InstallLogging set, so the
// tests after it log nowhere stale. It cannot undo ctrl.SetLogger:
// controller-runtime fulfils its root logger once per process, and later calls
// do nothing, so the test of that call runs in a subprocess of its own. The
// saved OpenTelemetry error handler is not put back either: the default one
// delegates only once, so a no-op handler takes its place.
func restoreGlobals(t *testing.T) {
	t.Helper()
	writer, flags := log.Writer(), log.Flags()
	target := telemetry.SwapGRPCTarget(nil)
	t.Cleanup(func() {
		telemetry.SwapGRPCTarget(target)
		klog.ClearLogger()
		log.SetOutput(writer)
		log.SetFlags(flags)
		otel.SetLogger(logr.Discard())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {}))
	})
}

// client-go logs through klog, and net/http servers report TLS handshake
// errors through the standard library's log package: both must become records.
func TestInstallLogging_KlogAndStdlibLogReachTheOTelPipeline(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)
	restoreGlobals(t)
	telemetry.InstallLogging(logger, logger)

	klog.Info("from klog")
	klog.Flush()
	log.Print("from the standard library")

	scopes := map[string]string{}
	for _, r := range records(t, out) {
		scopes[r.Body.Value] = r.Scope.Name
	}
	if scopes["from klog"] != "test/klog" || scopes["from the standard library"] != "test/stdlib" {
		t.Errorf("records by message and scope = %v, want both, scoped test/klog and test/stdlib", scopes)
	}
}

// failingExporter counts its exports and fails each one.
type failingExporter struct{ exports *int }

func (e failingExporter) Export(context.Context, []sdklog.Record) error {
	*e.exports++
	return errors.New("export failed")
}
func (failingExporter) Shutdown(context.Context) error   { return nil }
func (failingExporter) ForceFlush(context.Context) error { return nil }

// An OTLP export failure is reported once; a failing diagnostics pipeline must
// not report its own failure, which would report a failure, forever. Once
// reported, the next error is reported too.
func TestInstallLogging_AFailingDiagnosticsPipelineDoesNotReportItselfInALoop(t *testing.T) {
	logger, _ := newStdoutLogger(t, otellog.SeverityInfo)
	exports := 0
	diag := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(failingExporter{&exports})))
	restoreGlobals(t)
	telemetry.InstallLogging(logger, telemetry.NewLogger(diag, "opentelemetry"))

	otel.Handle(errors.New("an exporter failed"))
	otel.Handle(errors.New("an exporter failed again"))

	if exports != 2 {
		t.Errorf("diagnostics exported %d times for two errors, want 2", exports)
	}
}

// The SDK reports its own warnings through otel's logger, at V(1): they reach
// the diagnostics logger, not the main one.
func TestInstallLogging_TheSDKsOwnWarningsReachTheDiagnosticsLogger(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)
	diagnostics, diagOut := newStdoutLogger(t, telemetry.LevelSeverity(1))
	restoreGlobals(t)
	telemetry.InstallLogging(logger, diagnostics)

	_ = sdklog.NewLoggerProvider().Logger("")

	got := records(t, diagOut)
	if len(got) != 1 || got[0].Body.Value != "Invalid Logger name." || len(records(t, out)) != 0 {
		t.Errorf("diagnostics records = %+v, want the SDK's one warning, and none in the main logger", got)
	}
}

// childEnv marks the process TestInstallLogging_ControllerRuntimeLogsThroughThePipeline
// starts to run the logging half of that test.
const childEnv = "TELEMETRY_TEST_INSTALL_LOGGING_CHILD"

// controller-runtime fulfils its root logger once per process, so only the
// first SetLogger counts and a test in this process cannot pin it. The test
// runs InstallLogging in a process of its own and reads what that one writes.
func TestInstallLogging_ControllerRuntimeLogsThroughThePipeline(t *testing.T) {
	if os.Getenv(childEnv) == "1" {
		logger := newStdoutLoggerTo(t, os.Stdout, otellog.SeverityInfo)
		telemetry.InstallLogging(logger, logger)
		ctrl.Log.WithName("x").Info("from controller-runtime")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestInstallLogging_ControllerRuntimeLogsThroughThePipeline$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("child process: %v\n%s", err, output)
	}

	var got []stdoutRecord
	for _, line := range bytes.Split(output, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var r stdoutRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 1 || got[0].Body.Value != "from controller-runtime" || got[0].Scope.Name != "test/x" {
		t.Errorf("records = %+v\nchild output:\n%s\nwant one record, scoped test/x", got, output)
	}
}

// An error passed to logger.Error is recorded as the exception attributes of
// the OpenTelemetry semantic conventions.
func TestLogger_ErrorRecordsTheExceptionAttributes(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)

	logger.Error(errors.New("boom"), "it failed")

	got := records(t, out)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	attrs := map[string]any{}
	for _, a := range got[0].Attributes {
		attrs[a.Key] = a.Value.Value
	}
	if attrs["exception.message"] != "boom" || attrs["exception.type"] == nil {
		t.Errorf("attributes = %v, want exception.message=boom and an exception.type", attrs)
	}
}
