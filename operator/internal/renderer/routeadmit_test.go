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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

var routedT0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func routedGateway(edition v1alpha1.Edition, router *v1alpha1.RouterConfig) *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{Edition: edition, Version: "2.13",
			Config: v1alpha1.GatewayConfig{Router: router}},
	}
}

// routed is a KrakenDEndpoint created minutes after routedT0 with one entry
// of method per path.
func routed(name string, minutes int, method string, paths ...string) v1alpha1.KrakenDEndpoint {
	ep := v1alpha1.KrakenDEndpoint{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns",
		CreationTimestamp: metav1.NewTime(routedT0.Add(time.Duration(minutes) * time.Minute))}}
	for _, p := range paths {
		ep.Spec.Endpoints = append(ep.Spec.Endpoints, v1alpha1.EndpointEntry{Endpoint: p, Method: method,
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/x"}}})
	}
	return ep
}

// servedEntries lists the rendered entries as "METHOD path".
func servedEntries(t *testing.T, out *RenderOutput) []string {
	t.Helper()
	var doc struct {
		Endpoints []struct {
			Endpoint string `json:"endpoint"`
			Method   string `json:"method"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(out.JSON, &doc); err != nil {
		t.Fatal(err)
	}
	served := []string{}
	for _, e := range doc.Endpoints {
		served = append(served, e.Method+" "+e.Endpoint)
	}
	return served
}

// TestRender_ARenderWithoutClashesIsUnchanged pins the checksum of a render
// with no clash, so resolving clashes never changes (and rolls) the config
// of a gateway that has none.
func TestRender_ARenderWithoutClashesIsUnchanged(t *testing.T) {
	gw := routedGateway(v1alpha1.EditionEE, &v1alpha1.RouterConfig{AutoOptions: true, HealthPath: "/status"})
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{Raw: &runtime.RawExtension{Raw: []byte(`{"qos/http-cache":{"shared":true}}`)}}}
	withPolicy := routed("c", 2, "GET", "/c/{id}")
	withPolicy.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
	in := RenderInput{Gateway: gw, Policies: map[string]*v1alpha1.KrakenDBackendPolicy{"ns/p": policy},
		Endpoints: []v1alpha1.KrakenDEndpoint{
			routed("a", 0, "GET", "/users/{id}", "/users/{id}/orders"),
			routed("b", 1, "POST", "/users/{id}"),
			withPolicy,
			routed("w", 3, "GET", "/files/*"),
		}}

	out, err := New(Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}

	const want = "096f201b1129f20da995540ed15e26145719594bc642dab73bdba4ccd4af112d"
	if out.Checksum != want {
		t.Errorf("checksum = %s, want %s", out.Checksum, want)
	}
}

func TestRender_ARouterClashKeepsTheOlderEndpointsEntry(t *testing.T) {
	tests := []struct {
		name               string
		olderPath, newPath string
	}{
		{"the parameter route is older", "/users/{id}", "/users/{userId}/orders"},
		{"the nested route is older", "/users/{userId}/orders", "/users/{id}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := RenderInput{Gateway: routedGateway(v1alpha1.EditionCE, nil), Endpoints: []v1alpha1.KrakenDEndpoint{
				routed("older", 0, "GET", tt.olderPath), routed("newer", 1, "GET", tt.newPath),
			}}

			out, err := New(Options{}).Render(in)
			if err != nil {
				t.Fatal(err)
			}

			if got := servedEntries(t, out); len(got) != 1 || got[0] != "GET "+tt.olderPath {
				t.Errorf("served %v, want only the older endpoint's GET %s", got, tt.olderPath)
			}
			lost := out.EntryConflicts[types.NamespacedName{Namespace: "ns", Name: "newer"}]
			if len(lost) != 1 || lost[0].Endpoint != tt.newPath || lost[0].Winner.Name != "older" ||
				!strings.Contains(lost[0].Detail, "conflicts with existing wildcard") {
				t.Errorf("newer lost %+v, want its entry, to ns/older, with gin's refusal", lost)
			}
		})
	}
}
