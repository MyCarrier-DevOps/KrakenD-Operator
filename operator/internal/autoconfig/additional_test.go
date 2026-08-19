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

package autoconfig

import (
	"strings"
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
		OutputEncoding: "no-op",                   // must NOT override the explicit "json"
		InputHeaders:   []string{"Authorization"}, // must fill (entry leaves it unset)
		ExtraConfig:    &runtime.RawExtension{Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`)},
	}}
	out := BuildAdditionalEntries([]v1alpha1.AdditionalEndpoint{{
		Endpoint:        "/audit",
		OutputEncoding:  "json", // explicit — must survive
		InheritDefaults: &tru,
	}}, defaults, "http://svc")

	if out[0].OutputEncoding != "json" {
		t.Fatalf("explicit OutputEncoding overwritten by default: got %q, want json", out[0].OutputEncoding)
	}
	if len(out[0].InputHeaders) != 1 || out[0].InputHeaders[0] != "Authorization" {
		t.Fatalf("default InputHeaders not filled: %+v", out[0].InputHeaders)
	}
	if out[0].ExtraConfig == nil || !strings.Contains(string(out[0].ExtraConfig.Raw), "auth/validator") {
		t.Fatalf("inherited extraConfig missing: %+v", out[0].ExtraConfig)
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

func TestMergeAdditional_AppendsNonColliding(t *testing.T) {
	base := []v1alpha1.EndpointEntry{{Endpoint: "/api/users", Method: "GET"}}
	add := []v1alpha1.EndpointEntry{{Endpoint: "/liveness", Method: "GET"}}

	combined, replaced := MergeAdditional(base, add)

	if len(combined) != 2 {
		t.Fatalf("want 2 combined, got %d", len(combined))
	}
	if len(replaced) != 0 {
		t.Fatalf("want no replacements, got %v", replaced)
	}
}

func TestMergeAdditional_AdditionalWinsOnCollision(t *testing.T) {
	base := []v1alpha1.EndpointEntry{{
		Endpoint: "/api/users", Method: "GET",
		Backends: []v1alpha1.BackendSpec{{Host: []string{"http://old"}, URLPattern: "/old"}},
	}}
	add := []v1alpha1.EndpointEntry{{
		Endpoint: "/api/users", Method: "GET",
		Backends: []v1alpha1.BackendSpec{{Host: []string{"http://new"}, URLPattern: "/new"}},
	}}

	combined, replaced := MergeAdditional(base, add)

	if len(combined) != 1 {
		t.Fatalf("want 1 combined, got %d", len(combined))
	}
	if combined[0].Backends[0].Host[0] != "http://new" {
		t.Fatalf("additional did not win: %+v", combined[0].Backends[0])
	}
	if len(replaced) != 1 || replaced[0] != "/api/users:GET" {
		t.Fatalf("want replaced [/api/users:GET], got %v", replaced)
	}
}

func TestDeriveBasePath(t *testing.T) {
	mk := func(paths ...string) []v1alpha1.EndpointEntry {
		out := make([]v1alpha1.EndpointEntry, 0, len(paths))
		for _, p := range paths {
			out = append(out, v1alpha1.EndpointEntry{Endpoint: p, Method: "GET"})
		}
		return out
	}
	cases := []struct {
		name string
		in   []v1alpha1.EndpointEntry
		want string
	}{
		{"single", mk("/api/v1/quote/rate"), "/api/v1/quote"},
		{"same-parent", mk("/api/v1/quote/rate", "/api/v1/quote/quotes"), "/api/v1/quote"},
		{"divergent-resource", mk("/api/v1/quote/rate", "/api/v1/billing/inv"), "/api/v1"},
		{"drops-path-param", mk("/api/v1/quote/rate/{id}"), "/api/v1/quote/rate"},
		{"root-level-indeterminate", mk("/health"), ""},
		{"divergent-top-indeterminate", mk("/api/users", "/admin/things"), ""},
		{"empty-indeterminate", mk(), ""},
	}
	for _, c := range cases {
		if got := DeriveBasePath(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestScopeAdditionalEntries(t *testing.T) {
	entries := []v1alpha1.EndpointEntry{
		{Endpoint: "/liveness", Method: "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/liveness"}}},
		{Endpoint: "/api/v1/quote/ready", Method: "GET"}, // already prefixed → unchanged
	}
	ScopeAdditionalEntries(entries, "/api/v1/quote")

	if entries[0].Endpoint != "/api/v1/quote/liveness" {
		t.Fatalf("bare endpoint not scoped: %q", entries[0].Endpoint)
	}
	if entries[0].Backends[0].URLPattern != "/liveness" {
		t.Fatalf("backend urlPattern must be untouched: %q", entries[0].Backends[0].URLPattern)
	}
	if entries[1].Endpoint != "/api/v1/quote/ready" {
		t.Fatalf("already-prefixed endpoint changed: %q", entries[1].Endpoint)
	}
}

func TestScopeAdditionalEntries_EmptyBaseNoop(t *testing.T) {
	entries := []v1alpha1.EndpointEntry{{Endpoint: "/liveness", Method: "GET"}}
	ScopeAdditionalEntries(entries, "")
	if entries[0].Endpoint != "/liveness" {
		t.Fatalf("empty base must be a no-op, got %q", entries[0].Endpoint)
	}
}
