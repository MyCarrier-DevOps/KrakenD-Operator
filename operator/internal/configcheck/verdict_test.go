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
	"strings"
	"testing"
	"unicode/utf8"

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

func TestVerdictSummary_KeepsAPrefixOfAnOversizedFirstFinding(t *testing.T) {
	v := Verdict{Findings: []Finding{
		{Index: -1, Message: strings.Repeat("é", 100)},
		{Index: -1, Message: "second"},
	}}

	got := v.Summary(20)

	// "gateway: " is 9 bytes; the 20th byte splits an "é".
	if !strings.HasPrefix(got, "gateway: ééééé") || !strings.HasSuffix(got, " (+1 more)") {
		t.Errorf("summary = %q, want a prefix of the first finding and the count", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("summary = %q is not valid UTF-8", got)
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

func TestFindingsFrom_NamesTheWinnerOfTwoSameShapeEntries(t *testing.T) {
	rendered := []byte(`{"endpoints":[{"endpoint":"/a","method":"GET"}]}`)
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	eps := []v1alpha1.KrakenDEndpoint{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a"}, Spec: v1alpha1.KrakenDEndpointSpec{
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: "/z", Method: "GET"}, {Endpoint: "/a", Method: "GET"},
				{Endpoint: "/a", Method: "GET"}}}},
	}

	got := findingsFrom([]renderer.Attribution{{Endpoint: a, Index: 0, Message: "bad"}}, rendered, eps, "")

	if len(got) != 1 || got[0].Index != 1 {
		t.Errorf("findings = %+v, want one finding on spec.endpoints[1], the earlier of the pair", got)
	}
}

func TestFindingsFrom_LeavesTheIndexOpenWithoutAnExactMatch(t *testing.T) {
	rendered := []byte(`{"endpoints":[{"endpoint":"/a","method":"GET"}]}`)
	known := types.NamespacedName{Namespace: "ns", Name: "a"}
	missing := types.NamespacedName{Namespace: "ns", Name: "gone"}
	eps := []v1alpha1.KrakenDEndpoint{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a"}, Spec: v1alpha1.KrakenDEndpointSpec{
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: "/a", Method: "POST"}}}},
	}

	for name, ep := range map[string]types.NamespacedName{"no exact match": known, "endpoint not listed": missing} {
		got := findingsFrom([]renderer.Attribution{{Endpoint: ep, Index: 0, Message: "bad"}}, rendered, eps, "")

		if len(got) != 1 || got[0].Index != -1 || got[0].String() != ep.String()+": bad" {
			t.Errorf("%s: findings = %+v, want %q", name, got, ep.String()+": bad")
		}
	}
}

func TestFindingsFrom_ABlameWithoutAnEndpointIsAGatewayFinding(t *testing.T) {
	rendered := []byte(`{"endpoints":[{"endpoint":"/a","method":"GET"}]}`)

	got := findingsFrom([]renderer.Attribution{{Index: 3, Message: "bad"}}, rendered, nil, "")

	if len(got) != 1 || got[0].Index != -1 || got[0].String() != "gateway: bad" {
		t.Errorf("findings = %+v, want one gateway finding", got)
	}
}

func TestFindingsFrom_EmptyOutputStillGivesAReason(t *testing.T) {
	got := findingsFrom(nil, nil, nil, " \n ")

	if len(got) != 1 || got[0].Index != -1 || got[0].Message != "rejected with no output" {
		t.Errorf("findings = %+v, want one gateway finding saying the output was empty", got)
	}
}

func TestFindingsFrom_OutputIsOneLine(t *testing.T) {
	got := findingsFrom(nil, nil, nil, "first\n\n  second \r\nthird\n")

	if len(got) != 1 || got[0].Message != "first; second; third" {
		t.Errorf("findings = %+v, want the lines joined with \"; \"", got)
	}
}
