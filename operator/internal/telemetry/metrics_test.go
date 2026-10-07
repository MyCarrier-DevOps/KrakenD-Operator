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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/types"

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
	mp := telemetry.NewMeterProvider(sdkmetric.WithReader(reader))
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

func TestOperatorMetrics_ForgetGatewayRemovesItsGaugeSeries(t *testing.T) {
	m, scrape := newScraped(t)
	gw := types.NamespacedName{Namespace: "ns", Name: "gw"}
	m.SetEndpoints(gw, 3)
	m.SetGatewayInfo(gw, "EE", "2.13")
	m.SetConfigValid(gw, true)
	m.SetDragonflyReady(gw, true)
	m.SetLicenseExpiry(gw, time.Hour)
	m.SetExcludedEndpoints(gw, map[string]int{"FailsValidation": 2})
	if got := strings.Count(scrape(), `namespace="ns"`); got != 6 {
		t.Fatalf("%d series for the gateway before ForgetGateway, want 6", got)
	}

	m.ForgetGateway(gw)

	if got := scrape(); strings.Contains(got, `namespace="ns"`) {
		t.Errorf("series left for a forgotten gateway:\n%s", got)
	}
}

func TestOperatorMetrics_ExcludedEndpointsReplacesEveryReason(t *testing.T) {
	m, scrape := newScraped(t)
	gw := types.NamespacedName{Namespace: "ns", Name: "gw"}
	m.SetExcludedEndpoints(gw, map[string]int{"A": 1, "B": 2})

	m.SetExcludedEndpoints(gw, map[string]int{"B": 1, "C": 0})

	got := scrape()
	if want := `krakend_operator_gateway_excluded_endpoints{gateway="gw",namespace="ns",reason="B"} 1`; !strings.Contains(got, want) {
		t.Errorf("exposition lacks %q:\n%s", want, got)
	}
	if strings.Contains(got, `reason="A"`) || strings.Contains(got, `reason="C"`) {
		t.Errorf("a dropped or zero reason still has a series:\n%s", got)
	}
}

func TestPrometheusReader_AddsNoFamilyOrLabelOfItsOwn(t *testing.T) {
	m, scrape := newScraped(t)
	m.ConfigRendered(context.Background())

	got := scrape()
	for _, unwanted := range []string{"target_info", "otel_scope_"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("exposition has %q:\n%s", unwanted, got)
		}
	}
}

// The SDK keeps at most 2,000 series per instrument unless told otherwise and
// folds the rest into one otel_metric_overflow series. The reconcile-duration
// histogram keeps a deleted gateway's series until the operator restarts, so a
// long-lived operator passes that limit: every gateway must keep its own series.
func TestOperatorMetrics_EveryGatewayKeepsItsOwnSeriesPastTheSDKDefaultLimit(t *testing.T) {
	m, scrape := newScraped(t)
	const gateways = 2001
	for i := range gateways {
		gw := types.NamespacedName{Namespace: "ns", Name: fmt.Sprintf("gw-%d", i)}
		m.GatewayReconciled(context.Background(), gw, time.Millisecond)
		m.SetEndpoints(gw, 1)
	}

	got := scrape()
	if strings.Contains(got, "otel_metric_overflow") {
		t.Error("the exposition has an otel_metric_overflow series: gateways past the limit lost their own")
	}
	for _, series := range []string{"krakend_operator_reconcile_duration_seconds_count{", "krakend_operator_endpoints{"} {
		if n := strings.Count(got, series); n != gateways {
			t.Errorf("%d %s…} series, want one per gateway (%d)", n, series, gateways)
		}
	}
}
