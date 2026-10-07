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
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/bridges/otellogr"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// LogFormat is how the stdout exporter writes records.
type LogFormat string

// The stdout formats: one JSON object per line, or indented JSON.
const (
	LogFormatJSON   LogFormat = "json"
	LogFormatPretty LogFormat = "pretty"
)

// LevelSeverity maps a logr verbosity to a log record severity: V(0) is INFO,
// and each level above it one severity lower (V(1) DEBUG4 to V(4) DEBUG, then
// TRACE4 to TRACE), so a --zap-log-level of N keeps exactly V(0) to V(N).
func LevelSeverity(level int) otellog.Severity {
	return max(otellog.SeverityInfo-otellog.Severity(level), otellog.SeverityTrace)
}

// ParseLogLevel reads a --zap-log-level value: debug (V(1)), info, error,
// panic (nothing logr emits), or an integer N > 0 (V(N)). It returns the
// lowest severity kept.
func ParseLogLevel(value string) (otellog.Severity, error) {
	switch strings.ToLower(value) {
	case "debug":
		return LevelSeverity(1), nil
	case "info":
		return otellog.SeverityInfo, nil
	case "error":
		return otellog.SeverityError, nil
	case "panic":
		return otellog.SeverityFatal, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid log level %q: want debug, info, error, panic or an integer above 0", value)
	}
	return LevelSeverity(n), nil
}

// NewStdoutProcessor returns the processor that writes every record to w
// synchronously, so a record is out before the process can exit.
func NewStdoutProcessor(w io.Writer, format LogFormat) (sdklog.Processor, error) {
	opts := []stdoutlog.Option{stdoutlog.WithWriter(w)}
	switch format {
	case LogFormatJSON:
	case LogFormatPretty:
		opts = append(opts, stdoutlog.WithPrettyPrint())
	default:
		return nil, fmt.Errorf("invalid log format %q: want json or pretty", format)
	}
	exporter, err := stdoutlog.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating the stdout log exporter: %w", err)
	}
	return sdklog.NewSimpleProcessor(exporter), nil
}

// WithMinSeverity wraps next so it drops records below minimum. Records it
// keeps are completed for readers that expect the fields the log bridge leaves
// unset (the timestamp and the severity text) and exported with a context that
// is never cancelled: an exporter that honours cancellation would otherwise
// drop the records a timed-out reconcile or admission request logs.
func WithMinSeverity(next sdklog.Processor, minimum otellog.Severity) sdklog.Processor {
	return &levelProcessor{Processor: next, minimum: minimum}
}

// NewLogger returns the logr.Logger every component logs through: an
// OpenTelemetry log bridge over provider, named name.
func NewLogger(provider otellog.LoggerProvider, name string) logr.Logger {
	return logr.New(otellogr.NewLogSink(name,
		otellogr.WithLoggerProvider(provider), otellogr.WithLevelSeverity(LevelSeverity)))
}

// NewErrorHandler returns an OpenTelemetry error handler that logs each
// error through logger. An error raised while one is being logged is dropped,
// so a failing log pipeline cannot report its own failures in a loop.
func NewErrorHandler(logger logr.Logger) func(error) {
	var busy atomic.Bool
	return func(err error) {
		if !busy.CompareAndSwap(false, true) {
			return
		}
		defer busy.Store(false)
		logger.Error(err, "OpenTelemetry pipeline error")
	}
}

// levelProcessor is the processor WithMinSeverity returns.
type levelProcessor struct {
	sdklog.Processor
	minimum otellog.Severity
}

// Enabled reports false below the minimum severity, so a disabled
// logger.V(n) call is not even built.
func (p *levelProcessor) Enabled(ctx context.Context, param sdklog.EnabledParameters) bool {
	return param.Severity >= p.minimum && p.Processor.Enabled(ctx, param)
}

// OnEmit passes records at or above the minimum severity on.
func (p *levelProcessor) OnEmit(ctx context.Context, r *sdklog.Record) error {
	if r.Severity() < p.minimum {
		return nil
	}
	if r.Timestamp().IsZero() {
		r.SetTimestamp(r.ObservedTimestamp())
	}
	if r.SeverityText() == "" {
		r.SetSeverityText(r.Severity().String())
	}
	return p.Processor.OnEmit(context.WithoutCancel(ctx), r)
}
