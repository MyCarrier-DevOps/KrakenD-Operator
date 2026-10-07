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

package main

import (
	"flag"
	"fmt"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
)

// logFlags are the logging flags. The --zap-* flags keep their names, so an
// existing Deployment's arguments still parse: --zap-devel and --zap-log-level
// keep their meaning, --zap-encoder picks the stdout format unless
// --log-format does, and --zap-stacktrace-level and --zap-time-encoding are
// accepted and ignored.
type logFlags struct {
	devel          bool
	level          string
	format         string
	encoder        string
	stacktrace     string
	timeEncoding   string
	formatExplicit bool
}

// bind registers the flags on fs.
func (f *logFlags) bind(fs *flag.FlagSet) {
	fs.BoolVar(&f.devel, "zap-devel", true,
		"Log at debug level unless --zap-log-level is set; false logs at info level.")
	fs.StringVar(&f.level, "zap-log-level", "",
		"Lowest level logged: debug, info, error, panic, or an integer N > 0 for verbosity N.")
	fs.Func("log-format", "How log records are written to stdout: json (one object per line, the default) or pretty.",
		func(v string) error { f.format, f.formatExplicit = v, true; return nil })
	fs.StringVar(&f.encoder, "zap-encoder", "",
		"Deprecated: use --log-format. json selects json and console selects pretty.")
	fs.StringVar(&f.stacktrace, "zap-stacktrace-level", "", "Deprecated and ignored.")
	fs.StringVar(&f.timeEncoding, "zap-time-encoding", "", "Deprecated and ignored: records carry their timestamp.")
}

// resolve returns the lowest severity logged and the stdout format, and the
// ignored flags that were set, for a startup warning.
func (f *logFlags) resolve() (otellog.Severity, telemetry.LogFormat, []string, error) {
	level := f.level
	if level == "" {
		level = "info"
		if f.devel {
			level = "debug"
		}
	}
	minimum, err := telemetry.ParseLogLevel(level)
	if err != nil {
		return 0, "", nil, err
	}
	format := telemetry.LogFormatJSON
	switch {
	case f.formatExplicit:
		format = telemetry.LogFormat(f.format)
	case f.encoder == "console":
		format = telemetry.LogFormatPretty
	case f.encoder != "" && f.encoder != "json":
		return 0, "", nil, fmt.Errorf("invalid --zap-encoder %q: want json or console", f.encoder)
	}
	if format != telemetry.LogFormatJSON && format != telemetry.LogFormatPretty {
		return 0, "", nil, fmt.Errorf("invalid --log-format %q: want json or pretty", format)
	}
	var ignored []string
	if f.stacktrace != "" {
		ignored = append(ignored, "--zap-stacktrace-level")
	}
	if f.timeEncoding != "" {
		ignored = append(ignored, "--zap-time-encoding")
	}
	return minimum, format, ignored, nil
}
