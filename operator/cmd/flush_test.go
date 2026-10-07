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
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
)

// flushTelemetry runs when the log pipeline is the thing being shut down, so
// a failed flush can only be reported on stderr.
func TestFlushTelemetry_AFailedFlushIsPrintedToStderr(t *testing.T) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER",
		"OTEL_LOGS_EXPORTER", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(name, "")
	}
	hung := make(chan struct{})
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hung }))
	defer collector.Close()
	defer close(hung)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		LogLevel: otellog.SeverityInfo, Stdout: io.Discard, Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, span := tel.TracerProvider.Tracer("t").Start(context.Background(), "pending")
	span.End()
	var stderr bytes.Buffer

	flushTelemetry(tel, 100*time.Millisecond, &stderr)

	if !strings.HasPrefix(stderr.String(), "flushing telemetry: ") {
		t.Errorf("stderr = %q, want the failed flush", stderr.String())
	}
}

func TestFlushTelemetry_ASuccessfulFlushPrintsNothing(t *testing.T) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER",
		"OTEL_LOGS_EXPORTER", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(name, "")
	}
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		LogLevel: otellog.SeverityInfo, Stdout: io.Discard, Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer

	flushTelemetry(tel, time.Second, &stderr)

	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

// The pod's terminationGracePeriodSeconds is 10: the manager's stop and the
// flush together must fit in it, or the kubelet kills the process mid-flush.
func TestTelemetryFlushTimeout_LeavesTheManagerRoomInTheGracePeriod(t *testing.T) {
	if telemetryFlushTimeout > 5*time.Second {
		t.Errorf("telemetryFlushTimeout = %v, want at most 5s of the pod's 10s grace period", telemetryFlushTimeout)
	}
}
