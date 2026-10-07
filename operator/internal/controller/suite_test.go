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

package controller

import (
	"context"
	"maps"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestControllers(t *testing.T) {
	// Placeholder to ensure the package is testable.
	// Individual controller tests use the fake client pattern below.
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func fakeClientBuilder() *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointGateway, fieldindex.EndpointGatewayKeys).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointPolicy, fieldindex.EndpointPolicyKeys).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointController, fieldindex.EndpointControllerKeys)
}

// newTestChecker returns a config checker reading through c and validating
// with v, as main wires it.
func newTestChecker(c client.Client, v renderer.Validator) *configcheck.Checker {
	return configcheck.New(c, renderer.New(renderer.Options{}), v, 1, nil)
}

func fakeRecorder() *record.FakeRecorder {
	return record.NewFakeRecorder(100)
}

// countStatusWrites returns interceptor funcs that count every status update
// and status patch of an object of type T in n, then pass it through.
func countStatusWrites[T client.Object](n *int) interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			if _, ok := obj.(T); ok {
				*n++
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(
			ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			if _, ok := obj.(T); ok {
				*n++
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
}

// testMetrics returns the operator's metrics recording into a registry of
// their own, exported as /metrics exports them.
func testMetrics(t *testing.T) (*telemetry.OperatorMetrics, *prometheus.Registry) {
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
	return m, reg
}

// metricValue returns the value (a histogram's sample count) of family's
// series whose labels are exactly labels, given as name, value pairs, and
// whether that series exists.
func metricValue(t *testing.T, g prometheus.Gatherer, family string, labels ...string) (float64, bool) {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if !maps.Equal(got, want) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount()), true
			default:
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// seriesCount counts family's series in g.
func seriesCount(t *testing.T, g prometheus.Gatherer, family string) int {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() == family {
			return len(f.GetMetric())
		}
	}
	return 0
}
