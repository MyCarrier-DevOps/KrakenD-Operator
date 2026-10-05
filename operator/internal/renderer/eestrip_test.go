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

package renderer

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// eeFeatureInput is an EE gateway with two EE-only namespaces at every level
// and a wildcard endpoint. Go randomises map iteration, so a test that
// compares the stripped lists pins their sorting (with a single namespace per
// level there would be nothing to sort).
func eeFeatureInput(ceFallback bool) RenderInput {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{Edition: v1alpha1.EditionEE, Version: "2.13",
			Config: v1alpha1.GatewayConfig{ExtraConfig: &runtime.RawExtension{
				Raw: []byte(`{"auth/api-keys":{"keys":[]},"redis":{"connection_pools":[]},"security/cors":{"allow_origins":["*"]}}`),
			}},
		},
	}
	ep := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{Endpoints: []v1alpha1.EndpointEntry{
			{Endpoint: "/v1/*", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/x"}}},
			{Endpoint: "/users", Method: "GET",
				ExtraConfig: &runtime.RawExtension{Raw: []byte(`{"modifier/jmespath":{"expr":"a"},"auth/api-keys":{"roles":["admin"]},"qos/ratelimit/router":{"max_rate":10}}`)},
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/users",
					ExtraConfig: &runtime.RawExtension{Raw: []byte(
						`{"backend/http/client":{"proxy_address":"http://proxy"},"auth/gcp":{"audience":"a"},"qos/circuit-breaker":{"interval":60,"timeout":10,"max_errors":1}}`)},
				}}},
		}},
	}
	return RenderInput{Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{ep}, CEFallback: ceFallback}
}

func TestRender_CEFallbackStripsEEOnlyFeaturesAndListsThem(t *testing.T) {
	out, err := New(Options{}).Render(eeFeatureInput(true))
	if err != nil {
		t.Fatal(err)
	}
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	want := []StrippedEEFeature{
		{Source: a, Method: "GET", Endpoint: "/users", Feature: "extra_config auth/api-keys"},
		{Source: a, Method: "GET", Endpoint: "/users", Feature: "extra_config modifier/jmespath"},
		{Source: a, Method: "GET", Endpoint: "/users", Feature: "backend[0] extra_config auth/gcp"},
		{Source: a, Method: "GET", Endpoint: "/users", Feature: "backend[0] extra_config backend/http/client"},
		{Source: a, Method: "GET", Endpoint: "/v1/*", Feature: "wildcard endpoint"},
		{Feature: "extra_config auth/api-keys"},
		{Feature: "extra_config redis"},
	}
	if !reflect.DeepEqual(out.StrippedEEFeatures, want) {
		t.Errorf("StrippedEEFeatures = %+v, want %+v", out.StrippedEEFeatures, want)
	}
	var doc struct {
		Endpoints []struct {
			Endpoint    string         `json:"endpoint"`
			ExtraConfig map[string]any `json:"extra_config"`
			Backend     []struct {
				ExtraConfig map[string]any `json:"extra_config"`
			} `json:"backend"`
		} `json:"endpoints"`
		ExtraConfig map[string]any `json:"extra_config"`
	}
	if err := json.Unmarshal(out.JSON, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Endpoints) != 1 || doc.Endpoints[0].Endpoint != "/users" || len(out.Sources) != 1 {
		t.Fatalf("rendered endpoints = %+v (sources %v), want only /users", doc.Endpoints, out.Sources)
	}
	if _, ok := doc.Endpoints[0].ExtraConfig["auth/api-keys"]; ok {
		t.Error("the endpoint's auth/api-keys must be stripped")
	}
	if _, ok := doc.Endpoints[0].ExtraConfig["qos/ratelimit/router"]; !ok {
		t.Error("a CE namespace next to a stripped one must stay")
	}
	if _, ok := doc.Endpoints[0].Backend[0].ExtraConfig["backend/http/client"]; ok {
		t.Error("the backend's backend/http/client must be stripped")
	}
	if _, ok := doc.ExtraConfig["auth/api-keys"]; ok {
		t.Error("the root auth/api-keys must be stripped")
	}
	if _, ok := doc.ExtraConfig["security/cors"]; !ok {
		t.Error("the root security/cors (CE) must stay")
	}
}

func TestRender_WithoutFallbackKeepsEEFeatures(t *testing.T) {
	out, err := New(Options{}).Render(eeFeatureInput(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.StrippedEEFeatures) != 0 || len(out.Sources) != 2 {
		t.Errorf("stripped %v with %d sources, want nothing stripped and both entries rendered",
			out.StrippedEEFeatures, len(out.Sources))
	}
	for _, kept := range []string{`"/v1/*"`, `"auth/api-keys"`, `"redis"`, `"modifier/jmespath"`,
		`"auth/gcp"`, `"backend/http/client"`} {
		if !strings.Contains(string(out.JSON), kept) {
			t.Errorf("the render lacks %s, which only a CE fallback removes", kept)
		}
	}
}

func TestEEOnlyNamespaces(t *testing.T) {
	for _, tc := range []struct {
		level       NamespaceLevel
		has, hasNot string
	}{
		{LevelService, "auth/api-keys", "security/cors"},
		{LevelEndpoint, "auth/api-keys", "qos/ratelimit/router"},
		{LevelBackend, "backend/http/client", "qos/circuit-breaker"},
	} {
		got := EEOnlyNamespaces(tc.level)
		if !slices.Contains(got, tc.has) || slices.Contains(got, tc.hasNot) {
			t.Errorf("%s: %v, want it to contain %q and not %q", tc.level, got, tc.has, tc.hasNot)
		}
		if !slices.IsSorted(got) {
			t.Errorf("%s: %v is not sorted", tc.level, got)
		}
		if len(got) == 0 {
			continue
		}
		got[0] = "mutated"
		if EEOnlyNamespaces(tc.level)[0] == "mutated" {
			t.Errorf("%s: the returned slice aliases the renderer's list", tc.level)
		}
	}
	if got := EEOnlyNamespaces("gateway"); got != nil {
		t.Errorf("unknown level: %v, want nil", got)
	}
}

func TestRender_CEEditionDropsEndpointDocumentation(t *testing.T) {
	ep := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			ComponentSchemas: map[string]runtime.RawExtension{"User": {Raw: []byte(`{"type":"object"}`)}},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/users", Method: "GET",
				ExtraConfig: &runtime.RawExtension{Raw: []byte(
					`{"documentation/openapi":{"summary":"List users"},"qos/ratelimit/router":{"max_rate":10}}`)},
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/users"}},
			}},
		},
	}
	for _, tc := range []struct {
		name       string
		edition    v1alpha1.Edition
		ceFallback bool
		wantDocs   bool
		wantListed bool
	}{
		{"CE edition drops it and lists nothing", v1alpha1.EditionCE, false, false, false},
		{"EE keeps it", v1alpha1.EditionEE, false, true, false},
		{"CE fallback strips it; the gateway level lists it", v1alpha1.EditionEE, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &v1alpha1.KrakenDGateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
				Spec:       v1alpha1.KrakenDGatewaySpec{Edition: tc.edition, Version: "2.13"},
			}
			out, err := New(Options{}).Render(RenderInput{
				Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{ep}, CEFallback: tc.ceFallback,
			})
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Endpoints []struct {
					ExtraConfig map[string]any `json:"extra_config"`
				} `json:"endpoints"`
				ExtraConfig map[string]any `json:"extra_config"`
			}
			if err := json.Unmarshal(out.JSON, &doc); err != nil {
				t.Fatal(err)
			}
			_, entryDocs := doc.Endpoints[0].ExtraConfig["documentation/openapi"]
			_, rootDocs := doc.ExtraConfig["documentation/openapi"]
			if entryDocs != tc.wantDocs || rootDocs != tc.wantDocs {
				t.Errorf("documentation/openapi on the entry %v, at the root %v; want both %v",
					entryDocs, rootDocs, tc.wantDocs)
			}
			if _, ok := doc.Endpoints[0].ExtraConfig["qos/ratelimit/router"]; !ok {
				t.Error("a CE namespace next to the dropped one must stay")
			}
			listed := slices.ContainsFunc(out.StrippedEEFeatures, func(f StrippedEEFeature) bool {
				return f.Feature == "extra_config documentation/openapi"
			})
			if listed != tc.wantListed {
				t.Errorf("StrippedEEFeatures = %+v: documentation/openapi listed %v, want %v",
					out.StrippedEEFeatures, listed, tc.wantListed)
			}
		})
	}
	if slices.Contains(EEOnlyNamespaces(LevelEndpoint), "documentation/openapi") {
		t.Error("EEOnlyNamespaces(LevelEndpoint) lists documentation/openapi, which a CE render drops: " +
			"admission would refuse every AutoConfig endpoint of a CE gateway")
	}
}

func TestRender_CEFallbackDropsEntryDocsWithoutListingThem(t *testing.T) {
	entry := func(path, extraConfig string) v1alpha1.EndpointEntry {
		return v1alpha1.EndpointEntry{
			Endpoint: path, Method: "GET",
			ExtraConfig: &runtime.RawExtension{Raw: []byte(extraConfig)},
			Backends:    []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: path}},
		}
	}
	docsOnly := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "docs-only", Namespace: "ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			ComponentSchemas: map[string]runtime.RawExtension{"User": {Raw: []byte(`{"type":"object"}`)}},
			Endpoints: []v1alpha1.EndpointEntry{
				entry("/users", `{"documentation/openapi":{"audience":["public"]}}`),
			},
		},
	}
	withKeys := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "with-keys", Namespace: "ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{Endpoints: []v1alpha1.EndpointEntry{
			entry("/keys", `{"auth/api-keys":{"roles":["admin"]},"documentation/openapi":{"audience":["public"]}}`),
		}},
	}
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Edition: v1alpha1.EditionEE, Version: "2.13"},
	}

	out, err := New(Options{}).Render(RenderInput{
		Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{docsOnly, withKeys}, CEFallback: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var perEntry []StrippedEEFeature
	gatewayDocs := false
	for _, f := range out.StrippedEEFeatures {
		if f.Source == (types.NamespacedName{}) {
			gatewayDocs = gatewayDocs || f.Feature == "extra_config documentation/openapi"
			continue
		}
		perEntry = append(perEntry, f)
	}
	want := []StrippedEEFeature{{
		Source: types.NamespacedName{Namespace: "ns", Name: "with-keys"}, Method: "GET", Endpoint: "/keys",
		Feature: "extra_config auth/api-keys",
	}}
	if !reflect.DeepEqual(perEntry, want) {
		t.Errorf("per-endpoint features = %+v, want only %+v: docs-only namespaces are not listed per endpoint",
			perEntry, want)
	}
	if !gatewayDocs {
		t.Errorf("StrippedEEFeatures = %+v, want the gateway-level documentation/openapi listed",
			out.StrippedEEFeatures)
	}
	if strings.Contains(string(out.JSON), "documentation/openapi") {
		t.Errorf("the fallback render still carries documentation/openapi: %s", out.JSON)
	}
}

func TestRender_CEFallbackKeepsSourcesAlignedAcrossCRs(t *testing.T) {
	backend := []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/x"}}
	in := eeFeatureInput(true)
	// "a" sorts first and serves only an EE wildcard; "b" serves two entries.
	in.Endpoints = []v1alpha1.KrakenDEndpoint{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"}, Spec: v1alpha1.KrakenDEndpointSpec{
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: "/files/*", Method: "GET", Backends: backend}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}, Spec: v1alpha1.KrakenDEndpointSpec{
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/z", Method: "GET", Backends: backend},
				{Endpoint: "/m", Method: "GET", Backends: backend},
			},
		}},
	}

	out, err := New(Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Endpoints []struct {
			Endpoint string `json:"endpoint"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(out.JSON, &doc); err != nil {
		t.Fatal(err)
	}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}
	if wantSources := []types.NamespacedName{b, b}; !slices.Equal(out.Sources, wantSources) {
		t.Errorf("Sources = %v, want %v: the stripped wildcard of ns/a is not rendered", out.Sources, wantSources)
	}
	if len(doc.Endpoints) != 2 || doc.Endpoints[0].Endpoint != "/m" || doc.Endpoints[1].Endpoint != "/z" {
		t.Errorf("rendered endpoints = %+v, want /m then /z, aligned with Sources", doc.Endpoints)
	}
	wantStripped := []StrippedEEFeature{
		{Source: types.NamespacedName{Namespace: "ns", Name: "a"}, Method: "GET", Endpoint: "/files/*",
			Feature: FeatureWildcardEndpoint},
		{Feature: "extra_config auth/api-keys"},
		{Feature: "extra_config redis"},
	}
	if !slices.Equal(out.StrippedEEFeatures, wantStripped) {
		t.Errorf("StrippedEEFeatures = %+v, want %+v", out.StrippedEEFeatures, wantStripped)
	}
}

func TestCEDrops(t *testing.T) {
	tests := []struct {
		name  string
		level NamespaceLevel
		raw   string
		want  []CEDrop
	}{
		{"whole namespace", LevelEndpoint, `{"auth/api-keys":{"roles":["a"]},"qos/ratelimit/router":{"max_rate":1}}`,
			[]CEDrop{{Namespace: "auth/api-keys"}}},
		// CE honors send_body_on_redirect, so a block with nothing else drops nothing.
		{"CE-honored keys only", LevelBackend, `{"backend/http/client":{"send_body_on_redirect":true}}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ec map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tt.raw), &ec); err != nil {
				t.Fatal(err)
			}
			if got := CEDrops(tt.level, ec); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CEDrops = %+v, want %+v", got, tt.want)
			}
		})
	}
}
