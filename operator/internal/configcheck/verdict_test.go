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

package configcheck

import (
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

func TestVerdictSummary_BoundsAtAFindingBoundary(t *testing.T) {
	var v Verdict
	for i := range 10 {
		v.Findings = append(v.Findings, Finding{
			Endpoint: types.NamespacedName{Namespace: "ns", Name: "ep"}, Index: 0,
			Message: fmt.Sprintf("m%03d", i),
		})
	}

	got := v.Summary(100)

	want := "ns/ep spec.endpoints[0]: m000; ns/ep spec.endpoints[0]: m001; ns/ep spec.endpoints[0]: m002 (+7 more)"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestFindingString(t *testing.T) {
	ep := types.NamespacedName{Namespace: "ns", Name: "ep"}
	tests := map[string]Finding{
		"ns/ep spec.endpoints[2]: bad": {Endpoint: ep, Index: 2, Message: "bad"},
		"ns/ep: bad":                   {Endpoint: ep, Index: -1, Message: "bad"},
		"gateway: bad":                 {Index: -1, Message: "bad"},
	}
	for want, f := range tests {
		if got := f.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

func TestFindingsFrom_NamesTheSpecEntry(t *testing.T) {
	rendered := []byte(`{"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/x","method":"GET"},{"endpoint":"/y","method":"POST"}]}`)
	a, b := types.NamespacedName{Namespace: "ns", Name: "a"}, types.NamespacedName{Namespace: "ns", Name: "b"}
	eps := []v1alpha1.KrakenDEndpoint{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a"}, Spec: v1alpha1.KrakenDEndpointSpec{
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: "/a", Method: "GET"}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b"}, Spec: v1alpha1.KrakenDEndpointSpec{
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: "/y", Method: "POST"}, {Endpoint: "/x", Method: "GET"}}}},
	}
	atts := []renderer.Attribution{
		{Endpoint: b, Index: 2, Message: "- at '/endpoints/2/extra_config': bad"},
		{Endpoint: a, Index: 0, Message: "- at '/endpoints/0/endpoint': clash"},
		{Index: -1, Message: "'timeout' time: unknown unit"},
	}

	got := findingsFrom(atts, rendered, eps, "raw output")

	want := []Finding{
		{Endpoint: b, Index: 0, Message: "- at '/endpoints/2/extra_config': bad"},
		{Endpoint: a, Index: 0, Message: "- at '/endpoints/0/endpoint': clash"},
		{Index: -1, Message: "'timeout' time: unknown unit"},
	}
	if len(got) != len(want) {
		t.Fatalf("findings = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if f := findingsFrom(nil, rendered, eps, "  ERROR something  "); len(f) != 1 || f[0].Index != -1 ||
		f[0].Message != "ERROR something" {
		t.Errorf("empty attribution = %+v, want one gateway finding carrying the output", f)
	}
}
