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
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// exitingExecutor runs `sh -c "exit <code>"` whatever it is asked to run.
type exitingExecutor struct{ code string }

func (e exitingExecutor) Execute(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "sh", "-c", "exit "+e.code).CombinedOutput()
}

func attr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	i := slices.IndexFunc(attrs, func(kv attribute.KeyValue) bool { return string(kv.Key) == key })
	if i < 0 {
		return attribute.Value{}, false
	}
	return attrs[i].Value, true
}

func TestTraceExecutor_KrakendCheckIsAChildSpanWithModeAndExitCode(t *testing.T) {
	rec := tracingtest.New(t)
	ctx, parent := rec.Tracer().Start(context.Background(), "configcheck.CheckRendered")
	executor := telemetry.TraceExecutor(exitingExecutor{code: "1"}, rec.Tracer())

	_, err := executor.Execute(ctx, "/usr/local/bin/krakend", "check", "-t", "-n", "-c", "/tmp/krakend-config-42.json")
	parent.End()

	if err == nil {
		t.Fatal("want the exit error back")
	}
	spans := rec.Ended()
	spans.RequireChild(t, "configcheck.CheckRendered", "krakend check")
	span := spans.One(t, "krakend check")
	attrs := span.Attributes()
	if v, _ := attr(attrs, "process.exit.code"); v.AsInt64() != 1 {
		t.Errorf("process.exit.code = %v, want 1", v.String())
	}
	if v, _ := attr(attrs, "krakend.check.mode"); v.AsString() != "validate" {
		t.Errorf("krakend.check.mode = %q, want validate", v.AsString())
	}
	if v, _ := attr(attrs, "process.command_args"); slices.Contains(v.AsStringSlice(), "/tmp/krakend-config-42.json") {
		t.Errorf("process.command_args = %v, want the config path reduced to its base name", v.AsStringSlice())
	}
	if span.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error for a failed check", span.Status())
	}
}

// stubExecutor returns a fixed output and error, as renderer's KrakenDExecutor
// does: the output carries krakend's findings, the error only its exit status.
type stubExecutor struct {
	out []byte
	err error
}

func (e stubExecutor) Execute(context.Context, string, ...string) ([]byte, error) {
	return e.out, e.err
}

// The output of krakend names the values of the config it rejected, which is
// tenant data: it reaches the span nowhere, whatever failed.
func TestTraceExecutor_TheCommandOutputIsRecordedNowhere(t *testing.T) {
	rec := tracingtest.New(t)
	rejection := exec.Command("sh", "-c", "exit 1").Run() // an *exec.ExitError, as a rejection returns
	executor := telemetry.TraceExecutor(stubExecutor{
		out: []byte("- at '/endpoints/0': tenant-secret"), err: rejection,
	}, rec.Tracer())

	_, _ = executor.Execute(context.Background(), "krakend", "check", "-t", "-c", "/tmp/krakend-config-42.json")

	span := rec.Ended().One(t, "krakend check")
	recorded := []string{span.Status().Description}
	for _, kv := range span.Attributes() {
		recorded = append(recorded, string(kv.Key), kv.Value.Emit())
	}
	for _, event := range span.Events() {
		recorded = append(recorded, event.Name)
		for _, kv := range event.Attributes {
			recorded = append(recorded, string(kv.Key), kv.Value.Emit())
		}
	}
	for _, text := range recorded {
		if strings.Contains(text, "tenant-secret") {
			t.Errorf("the span records the command's output: %q", text)
		}
	}
}

func TestTraceExecutor_RecordsHowEachRunEnded(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		err      error
		wantCode int64 // -1: no exit code recorded
		wantMode string
		wantErr  bool
	}{
		{
			name: "success is exit code 0 and leaves the status unset",
			args: []string{"check", "-t", "-c", "/tmp/c.json"}, wantCode: 0, wantMode: "validate",
		},
		{
			name: "lint has no -t",
			args: []string{"check", "-l", "-c", "/tmp/c.json"}, wantCode: 0, wantMode: "lint",
		},
		{
			name: "a failed start is an error with no exit code",
			args: []string{"check", "-t", "-c", "/tmp/c.json"}, err: errors.New("fork/exec krakend: no such file"),
			wantCode: -1, wantMode: "validate", wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := tracingtest.New(t)
			executor := telemetry.TraceExecutor(stubExecutor{err: tt.err}, rec.Tracer())

			_, _ = executor.Execute(context.Background(), "krakend", tt.args...)

			span := rec.Ended().One(t, "krakend check")
			code, hasCode := attr(span.Attributes(), "process.exit.code")
			if tt.wantCode < 0 && hasCode {
				t.Errorf("process.exit.code = %v, want none", code.AsInt64())
			}
			if tt.wantCode >= 0 && (!hasCode || code.AsInt64() != tt.wantCode) {
				t.Errorf("process.exit.code = %v (set: %t), want %d", code.AsInt64(), hasCode, tt.wantCode)
			}
			if mode, _ := attr(span.Attributes(), "krakend.check.mode"); mode.AsString() != tt.wantMode {
				t.Errorf("krakend.check.mode = %q, want %q", mode.AsString(), tt.wantMode)
			}
			if gotErr := span.Status().Code == codes.Error; gotErr != tt.wantErr {
				t.Errorf("status = %v, want error: %t", span.Status(), tt.wantErr)
			}
		})
	}
}
