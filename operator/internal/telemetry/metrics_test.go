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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
)

// newScraped returns OperatorMetrics recording into a fresh registry, and a
// function that returns the registry's text exposition.
func newScraped(t *testing.T) (*telemetry.OperatorMetrics, func() string) {
	t.Helper()
	reg := prometheus.NewRegistry()
	reader, err := telemetry.NewPrometheusReader(reg)
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := telemetry.NewOperatorMetrics(mp.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	return m, func() string {
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
		for _, f := range families {
			if err := enc.Encode(f); err != nil {
				t.Fatal(err)
			}
		}
		return b.String()
	}
}

// The unlabelled counters were exported at 0 from startup by client_golang;
// an OpenTelemetry counter has no series until something is added to it.
func TestOperatorMetrics_UnlabelledCountersAreExportedFromStartup(t *testing.T) {
	_, scrape := newScraped(t)

	got := scrape()
	for _, line := range []string{
		"krakend_operator_config_renders_total 0",
		"krakend_operator_config_validation_failures_total 0",
		"krakend_operator_rolling_restarts_total 0",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("exposition lacks %q:\n%s", line, got)
		}
	}
}
