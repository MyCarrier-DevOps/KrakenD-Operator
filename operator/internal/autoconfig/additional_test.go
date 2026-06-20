package autoconfig

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestBuildAdditionalEntries_BareEntrySynthesizesGetBackend(t *testing.T) {
	out := BuildAdditionalEntries(
		[]v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}},
		nil, "http://svc:8080",
	)
	if len(out) != 1 {
		t.Fatalf("want 1 entry, got %d", len(out))
	}
	e := out[0]
	if e.Endpoint != "/liveness" || e.Method != "GET" {
		t.Fatalf("want GET /liveness, got %s %s", e.Method, e.Endpoint)
	}
	if len(e.Backends) != 1 {
		t.Fatalf("want 1 backend, got %d", len(e.Backends))
	}
	b := e.Backends[0]
	if b.Host[0] != "http://svc:8080" || b.URLPattern != "/liveness" || b.Method != "GET" {
		t.Fatalf("backend synthesis wrong: %+v", b)
	}
}

func TestBuildAdditionalEntries_NoOpEncodingSetsOutputEncoding(t *testing.T) {
	out := BuildAdditionalEntries(
		[]v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness", Encoding: "no-op"}},
		nil, "http://svc",
	)
	if out[0].OutputEncoding != "no-op" {
		t.Fatalf("want output encoding no-op, got %q", out[0].OutputEncoding)
	}
	if out[0].Backends[0].Encoding != "no-op" {
		t.Fatalf("want backend encoding no-op, got %q", out[0].Backends[0].Encoding)
	}
}

func TestBuildAdditionalEntries_ExplicitBackendsUsedVerbatim(t *testing.T) {
	out := BuildAdditionalEntries([]v1alpha1.AdditionalEndpoint{{
		Endpoint: "/audit",
		Method:   "POST",
		Host:     "http://ignored", // shorthand must be ignored
		Backends: []v1alpha1.BackendSpec{{Host: []string{"http://audit:8080"}, URLPattern: "/v2/audit"}},
	}}, nil, "http://svc")
	b := out[0].Backends[0]
	if b.Host[0] != "http://audit:8080" || b.URLPattern != "/v2/audit" {
		t.Fatalf("explicit backend not used verbatim: %+v", b)
	}
}

func TestBuildAdditionalEntries_InheritDefaultsFillsOnly(t *testing.T) {
	tru := true
	defaults := &v1alpha1.Defaults{Endpoint: &v1alpha1.EndpointDefaults{
		InputHeaders: []string{"Authorization"},
		ExtraConfig:  &runtime.RawExtension{Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`)},
	}}
	out := BuildAdditionalEntries([]v1alpha1.AdditionalEndpoint{{
		Endpoint: "/audit", InheritDefaults: &tru,
	}}, defaults, "http://svc")
	if len(out[0].InputHeaders) != 1 || out[0].InputHeaders[0] != "Authorization" {
		t.Fatalf("defaults not inherited: %+v", out[0].InputHeaders)
	}
	if out[0].ExtraConfig == nil {
		t.Fatalf("expected inherited extraConfig")
	}
}

func TestBuildAdditionalEntries_NoInheritByDefault(t *testing.T) {
	defaults := &v1alpha1.Defaults{Endpoint: &v1alpha1.EndpointDefaults{
		InputHeaders: []string{"Authorization"},
	}}
	out := BuildAdditionalEntries([]v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}}, defaults, "http://svc")
	if out[0].InputHeaders != nil {
		t.Fatalf("defaults must NOT be inherited when InheritDefaults is nil: %+v", out[0].InputHeaders)
	}
}
