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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// eeFeatureInput is an EE gateway with an EE-only namespace at every level
// and a wildcard endpoint.
func eeFeatureInput(ceFallback bool) RenderInput {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{Edition: v1alpha1.EditionEE, Version: "2.13",
			Config: v1alpha1.GatewayConfig{ExtraConfig: &runtime.RawExtension{
				Raw: []byte(`{"auth/api-keys":{"keys":[]},"security/cors":{"allow_origins":["*"]}}`),
			}},
		},
	}
	ep := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{Endpoints: []v1alpha1.EndpointEntry{
			{Endpoint: "/v1/*", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/x"}}},
			{Endpoint: "/users", Method: "GET",
				ExtraConfig: &runtime.RawExtension{Raw: []byte(`{"auth/api-keys":{"roles":["admin"]},"qos/ratelimit/router":{"max_rate":10}}`)},
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/users",
					ExtraConfig: &runtime.RawExtension{Raw: []byte(
						`{"backend/http/client":{"proxy_address":"http://proxy"},"qos/circuit-breaker":{"interval":60,"timeout":10,"max_errors":1}}`)},
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
		{Source: a, Method: "GET", Endpoint: "/users", Feature: "backend[0] extra_config backend/http/client"},
		{Source: a, Method: "GET", Endpoint: "/v1/*", Feature: "wildcard endpoint"},
		{Feature: "extra_config auth/api-keys"},
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
}
