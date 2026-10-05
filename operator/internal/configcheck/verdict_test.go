package configcheck

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestVerdictSummary_BoundsAtAFindingBoundary(t *testing.T) {
	var v Verdict
	for range 1000 {
		v.Findings = append(v.Findings, Finding{
			Endpoint: types.NamespacedName{Namespace: "ns", Name: "ep"}, Index: 0,
			Message: "at '/endpoints/0/backend/0/extra_config': additional properties 'qos/circuit-breakr' not allowed",
		})
	}

	s := v.Summary(1024)

	if len(s) > 1024+len(" (+1000 more)") {
		t.Fatalf("summary is %d bytes, want at most the limit plus the count", len(s))
	}
	if !strings.HasPrefix(s, "ns/ep spec.endpoints[0]: at '/endpoints/0/") || !strings.HasSuffix(s, " more)") {
		t.Errorf("summary = %q", s)
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
