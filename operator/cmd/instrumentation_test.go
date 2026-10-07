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
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/types"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
)

// goldenShape is one family of internal/controller/testdata/metrics.golden:
// the /metrics shape dashboards and alerts depend on.
type goldenShape struct {
	help, kind string
	labels     []string
}

// readGolden parses the golden file's families by name. A family's label
// names are those of its plain (or histogram _count) sample.
func readGolden(t *testing.T) map[string]*goldenShape {
	t.Helper()
	data, err := os.ReadFile("../internal/controller/testdata/metrics.golden")
	if err != nil {
		t.Fatal(err)
	}
	families := map[string]*goldenShape{}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			families[name] = &goldenShape{help: help}
		case strings.HasPrefix(line, "# TYPE "):
			name, kind, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			families[name].kind = kind
		default:
			name, labels, _ := strings.Cut(line, "{")
			if shape, ok := families[strings.TrimSuffix(name, "_count")]; ok && labels != "" && shape.labels == nil {
				shape.labels = strings.Split(strings.TrimSuffix(labels, "}"), ",")
			}
		}
	}
	return families
}

// Production /metrics is controller-runtime's registry, and the operator's
// families are on it only through the recorder the pod's instrumentation
// builds: a recorder that is missing serves none of them.
func TestNewInstrumentation_ServesEveryOperatorFamilyOnTheRegistry(t *testing.T) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER",
		"OTEL_LOGS_EXPORTER", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(name, "")
	}
	cfg := telemetryConfig(otellog.SeverityInfo, telemetry.LogFormatJSON)
	cfg.Stdout = io.Discard
	tel, err := telemetry.Setup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	inst, err := newInstrumentation(tel)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Tracer == nil || inst.Metrics == nil {
		t.Fatalf("instrumentation = %+v, want a tracer and a metrics recorder", inst)
	}
	key := types.NamespacedName{Namespace: "ns", Name: "gw"}
	inst.Metrics.SetLicenseExpiry(key, time.Second)
	inst.Metrics.SetEndpoints(key, 1)
	inst.Metrics.GatewayReconciled(context.Background(), key, time.Second)
	inst.Metrics.SetDragonflyReady(key, true)
	inst.Metrics.SetGatewayInfo(key, "EE", "2.13")
	inst.Metrics.SetConfigValid(key, true)
	inst.Metrics.SetExcludedEndpoints(key, map[string]int{"reason": 1})
	inst.Metrics.SetAutoConfigSynced(key, true)

	got := map[string]*dto.MetricFamily{}
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "krakend_operator_") {
			got[family.GetName()] = family
		}
	}
	want := readGolden(t)
	for name, shape := range want {
		family := got[name]
		if family == nil {
			t.Errorf("%s is not on controller-runtime's registry", name)
			continue
		}
		var labels []string
		for _, l := range family.GetMetric()[0].GetLabel() {
			labels = append(labels, l.GetName())
		}
		slices.Sort(labels)
		wantLabels := slices.Clone(shape.labels)
		slices.Sort(wantLabels)
		if family.GetHelp() != shape.help || !strings.EqualFold(family.GetType().String(), shape.kind) ||
			!slices.Equal(labels, wantLabels) {
			t.Errorf("%s = help %q, type %v, labels %v; want help %q, type %s, labels %v",
				name, family.GetHelp(), family.GetType(), labels, shape.help, shape.kind, wantLabels)
		}
	}
	for name := range got {
		if want[name] == nil {
			t.Errorf("%s is served but is not in the golden shape", name)
		}
	}
}
