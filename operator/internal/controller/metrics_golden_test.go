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
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// goldenMetrics is the shape of every operator metric on /metrics: each
// family's HELP and TYPE lines and, per sample, its name and label names (le
// with its value). Dashboards and alerts depend on exactly this, so it must
// not change.
const goldenMetrics = "testdata/metrics.golden"

// foreignPrefixes are the families other libraries register in the same
// registry. Any other family is the operator's and must be in the golden file.
var foreignPrefixes = []string{
	"certwatcher_", "controller_runtime_", "go_", "leader_election_", "process_", "rest_client_", "workqueue_",
}

// labelValue matches one label="value" pair of an exposition line.
var labelValue = regexp.MustCompile(`(\w+)="(?:[^"\\]|\\.)*"`)

func TestMetricsExposition_MatchesGolden(t *testing.T) {
	gatherer := recordEveryMetric(t)

	got := metricsShape(t, gatherer)

	want, err := os.ReadFile(goldenMetrics)
	if err != nil {
		t.Fatalf("reading %s: %v\nthe exposition shape is:\n%s", goldenMetrics, err, got)
	}
	if got != string(want) {
		t.Errorf("the exposition shape changed\n--- want (%s)\n%s\n--- got\n%s", goldenMetrics, want, got)
	}
}

// recordEveryMetric gives every operator metric at least one series and
// returns the registry /metrics serves.
func recordEveryMetric(t *testing.T) prometheus.Gatherer {
	t.Helper()
	const ns, name = "golden", "golden"
	t.Cleanup(func() {
		deleteGatewayMetrics(ns, name)
		autoConfigSynced.DeleteLabelValues(ns, name)
	})
	configRenders.Add(0)
	configValidationFailures.Add(0)
	rollingRestarts.Add(0)
	licenseExpirySeconds.WithLabelValues(ns, name).Set(1)
	endpointsPerGateway.WithLabelValues(ns, name).Set(1)
	reconcileDuration.WithLabelValues("gateway", ns, name).Observe(0.1)
	dragonflyReady.WithLabelValues(ns, name).Set(1)
	gatewayInfo.WithLabelValues(ns, name, "EE", "2.13").Set(1)
	gatewayConfigValid.WithLabelValues(ns, name).Set(1)
	gatewayExcludedEndpoints.WithLabelValues(ns, name, "golden").Set(1)
	autoConfigSynced.WithLabelValues(ns, name).Set(1)
	return ctrlmetrics.Registry
}

// metricsShape renders the operator's families in gatherer as the golden
// file holds them.
func metricsShape(t *testing.T, gatherer prometheus.Gatherer) string {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	var b bytes.Buffer
	enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, family := range families {
		if foreign(family.GetName()) {
			continue
		}
		if err := enc.Encode(family); err != nil {
			t.Fatalf("encoding %s: %v", family.GetName(), err)
		}
	}
	var lines []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if !strings.HasPrefix(line, "#") {
			line = labelValue.ReplaceAllStringFunc(line[:strings.LastIndexByte(line, ' ')], func(pair string) string {
				if strings.HasPrefix(pair, `le="`) {
					return pair
				}
				return pair[:strings.IndexByte(pair, '=')]
			})
		}
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func foreign(family string) bool {
	for _, prefix := range foreignPrefixes {
		if strings.HasPrefix(family, prefix) {
			return true
		}
	}
	return false
}
