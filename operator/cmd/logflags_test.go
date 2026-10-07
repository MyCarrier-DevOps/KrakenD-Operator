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
	"slices"
	"testing"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
)

func parseLogFlags(t *testing.T, args ...string) (otellog.Severity, telemetry.LogFormat, []string, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var f logFlags
	f.bind(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f.resolve()
}

// A Deployment written for an earlier release passes --zap-* flags: they must
// parse, and the level ones keep their meaning.
func TestLogFlags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		level   otellog.Severity
		format  telemetry.LogFormat
		ignored []string
	}{
		{"defaults keep today's debug level, as JSON", nil, otellog.SeverityDebug4, telemetry.LogFormatJSON, nil},
		{"--zap-devel=false logs at info", []string{"--zap-devel=false"}, otellog.SeverityInfo, telemetry.LogFormatJSON, nil},
		{"--zap-log-level wins over --zap-devel", []string{"--zap-log-level=error"}, otellog.SeverityError,
			telemetry.LogFormatJSON, nil},
		{"an integer level keeps that verbosity", []string{"--zap-log-level=3"}, otellog.SeverityDebug2,
			telemetry.LogFormatJSON, nil},
		{"--zap-encoder=console is pretty", []string{"--zap-encoder=console"}, otellog.SeverityDebug4,
			telemetry.LogFormatPretty, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			level, format, ignored, err := parseLogFlags(t, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if level != tc.level || format != tc.format || !slices.Equal(ignored, tc.ignored) {
				t.Errorf("got %v %v %v, want %v %v %v", level, format, ignored, tc.level, tc.format, tc.ignored)
			}
		})
	}
}
