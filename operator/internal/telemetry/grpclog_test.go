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
	"io"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/grpc/grpclog"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
)

// discardGRPCLogs puts back a grpc-go logger that writes nothing, so a test
// leaves no logger of its own behind.
func discardGRPCLogs(t *testing.T) {
	t.Cleanup(func() { grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard)) })
}

// grpc-go, which the OTLP gRPC exporters run on, logs through a global logger
// of its own that writes errors to stderr. Installed, it logs through the
// pipeline: a transport error is an ERROR record of the grpc logger.
func TestInstallGRPCLogging_AnErrorIsARecordOfThePipeline(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)
	telemetry.InstallGRPCLogging(logger.WithName("grpc"))
	discardGRPCLogs(t)

	grpclog.Errorf("transport: received GOAWAY with %s", "too_many_pings")

	got := records(t, out)
	if len(got) != 1 || got[0].Body.Value != "transport: received GOAWAY with too_many_pings" ||
		got[0].SeverityText != "ERROR" || got[0].Scope.Name != "test/grpc" {
		t.Errorf("records = %+v, want one ERROR record from test/grpc", got)
	}
}

// grpc-go logs a connection's every state change at info, which its own
// default logger drops. Through the pipeline they are verbosity 2: left out
// at the operator's default debug level (verbosity 1), shown from 2.
func TestInstallGRPCLogging_InfoIsVerbosityTwo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		minimum otellog.Severity
		want    int
	}{
		{"at the default debug level", telemetry.LevelSeverity(1), 0},
		{"at verbosity 2", telemetry.LevelSeverity(2), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, out := newStdoutLogger(t, tc.minimum)
			telemetry.InstallGRPCLogging(logger)
			discardGRPCLogs(t)

			grpclog.Info("[core] Channel switches to new LB policy")

			if got := len(records(t, out)); got != tc.want || grpclog.V(2) != (tc.want == 1) {
				t.Errorf("%d records, V(2) = %v; want %d", got, grpclog.V(2), tc.want)
			}
		})
	}
}

// grpc-go logs through grpclog.Component, which prefixes the component name:
// that is the path its own code takes, not the package-level functions.
func TestInstallGRPCLogging_AComponentsErrorIsARecordWithItsPrefix(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)
	telemetry.InstallGRPCLogging(logger)
	discardGRPCLogs(t)

	grpclog.Component("transport").Errorf("connection closed: %s", "EOF")

	got := records(t, out)
	if len(got) != 1 || got[0].Body.Value != "[transport] connection closed: EOF" || got[0].SeverityText != "ERROR" {
		t.Errorf("records = %+v, want one ERROR record, prefixed [transport]", got)
	}
}
