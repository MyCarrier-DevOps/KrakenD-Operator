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

package telemetry

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// Executor runs a command and returns its combined output, as
// renderer.CommandExecutor does.
type Executor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
}

// TraceExecutor wraps next so each command it runs is a span named after the
// command and its first argument ("krakend check"), with the arguments, the
// check mode and the exit code. The value of a -c flag (the config file, a
// random temporary name) is replaced by its base name; neither the config nor
// the command's output is recorded.
func TraceExecutor(next Executor, tracer trace.Tracer) Executor {
	return tracedExecutor{next: next, tracer: tracer}
}

// tracedExecutor is the executor TraceExecutor returns.
type tracedExecutor struct {
	next   Executor
	tracer trace.Tracer
}

// Execute runs the command inside its span.
func (e tracedExecutor) Execute(ctx context.Context, name string, args ...string) (out []byte, err error) {
	ctx, span := tracing.Start(ctx, e.tracer, spanName(name, args),
		trace.WithAttributes(commandAttributes(name, args)...))
	defer func() {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			span.SetAttributes(semconv.ProcessExitCode(exitErr.ExitCode()))
		} else if err == nil {
			span.SetAttributes(semconv.ProcessExitCode(0))
		}
		tracing.End(span, err)
	}()
	return e.next.Execute(ctx, name, args...)
}

func spanName(name string, args []string) string {
	if len(args) == 0 {
		return filepath.Base(name)
	}
	return filepath.Base(name) + " " + args[0]
}

// commandAttributes describes the command without the temporary file's path.
func commandAttributes(name string, args []string) []attribute.KeyValue {
	shown := slices.Clone(args)
	for i := range shown[:max(len(shown)-1, 0)] {
		if shown[i] == "-c" {
			shown[i+1] = filepath.Base(shown[i+1])
		}
	}
	mode := "lint"
	if slices.Contains(args, "-t") {
		mode = "validate"
	}
	return []attribute.KeyValue{
		semconv.ProcessExecutableName(filepath.Base(name)),
		semconv.ProcessCommandArgs(shown...),
		attribute.String("krakend.check.mode", mode),
	}
}
