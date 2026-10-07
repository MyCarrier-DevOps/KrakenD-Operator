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

func TestRender_ARouterClashWithinOneEndpointIsLeftForItsOwnCheck(t *testing.T) {
	in := RenderInput{Gateway: routedGateway(v1alpha1.EditionCE, nil), Endpoints: []v1alpha1.KrakenDEndpoint{
		routed("one", 0, "GET", "/users/{id}", "/users/{userId}/orders"),
	}}

	out, err := New(Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}

	if got := servedEntries(t, out); len(got) != 2 || len(out.EntryConflicts) != 0 {
		t.Errorf("served %v with conflicts %+v; want both entries rendered, for the endpoint's own check to refuse", got, out.EntryConflicts)
	}
}

func TestRender_AnAutoOptionsClashKeepsTheOlderEndpointsEntry(t *testing.T) {
	endpoints := []v1alpha1.KrakenDEndpoint{routed("older", 0, "GET", "/a/{id}"), routed("newer", 1, "POST", "/a/{name}")}

	with, err := New(Options{}).Render(RenderInput{
		Gateway: routedGateway(v1alpha1.EditionCE, &v1alpha1.RouterConfig{AutoOptions: true}), Endpoints: endpoints})
	if err != nil {
		t.Fatal(err)
	}
	without, err := New(Options{}).Render(RenderInput{Gateway: routedGateway(v1alpha1.EditionCE, nil), Endpoints: endpoints})
	if err != nil {
		t.Fatal(err)
	}

	lost := with.EntryConflicts[types.NamespacedName{Namespace: "ns", Name: "newer"}]
	if len(lost) != 1 || !strings.Contains(lost[0].Detail, "OPTIONS") {
		t.Errorf("with auto_options newer lost %+v, want POST /a/{name} to its OPTIONS route", lost)
	}
	if len(without.EntryConflicts) != 0 {
		t.Errorf("without auto_options conflicts = %+v, want none: the methods' trees are apart", without.EntryConflicts)
	}
}

func TestRender_AnEEWildcardOverlapKeepsTheOlderEntry(t *testing.T) {
	tests := []struct {
		name      string
		gateway   *v1alpha1.KrakenDGateway
		fallback  bool
		endpoints []v1alpha1.KrakenDEndpoint
		lostPath  string
	}{
		{"the wildcard is older", routedGateway(v1alpha1.EditionEE, nil), false,
			[]v1alpha1.KrakenDEndpoint{routed("older", 0, "GET", "/p/*"), routed("newer", 1, "GET", "/p/x")}, "/p/x"},
		{"the wildcard is newer", routedGateway(v1alpha1.EditionEE, nil), false,
			[]v1alpha1.KrakenDEndpoint{routed("older", 0, "GET", "/p/x"), routed("newer", 1, "GET", "/p/*")}, "/p/*"},
		{"another method", routedGateway(v1alpha1.EditionEE, nil), false,
			[]v1alpha1.KrakenDEndpoint{routed("older", 0, "GET", "/p/*"), routed("newer", 1, "POST", "/p/x")}, ""},
		{"CE fallback strips the wildcard", routedGateway(v1alpha1.EditionEE, nil), true,
			[]v1alpha1.KrakenDEndpoint{routed("older", 0, "GET", "/p/*"), routed("newer", 1, "GET", "/p/x")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := New(Options{}).Render(RenderInput{Gateway: tt.gateway, CEFallback: tt.fallback, Endpoints: tt.endpoints})
			if err != nil {
				t.Fatal(err)
			}
			lost := out.EntryConflicts[types.NamespacedName{Namespace: "ns", Name: "newer"}]
			switch {
			case tt.lostPath == "" && len(lost) != 0:
				t.Errorf("newer lost %+v, want nothing", lost)
			case tt.lostPath != "" && (len(lost) != 1 || lost[0].Endpoint != tt.lostPath ||
				!strings.Contains(lost[0].Detail, "EE wildcard")):
				t.Errorf("newer lost %+v, want %s under the EE wildcard rule", lost, tt.lostPath)
			}
		})
	}
}

// TestRender_RemovingAnEndpointLeavesNoOtherEntryOut pins that an entry loses
// to an older entry it clashes with whether or not that entry is served, so
// a write that removes an entry can bring others back but never leave
// another out: x's GET /a/{id} keeps z's GET /a/{name}/x out, and z's entry
// keeps v's GET /a/{id}/y out with x or without it.
func TestRender_RemovingAnEndpointLeavesNoOtherEntryOut(t *testing.T) {
	ce := routedGateway(v1alpha1.EditionCE, nil)
	x, z := routed("x", 0, "GET", "/a/{id}"), routed("z", 1, "GET", "/a/{name}/x")
	v, u := routed("v", 2, "GET", "/a/{id}/y"), routed("u", 3, "GET", "/b")
	autoOptions := routedGateway(v1alpha1.EditionCE, &v1alpha1.RouterConfig{AutoOptions: true})
	s, d := routed("s", 0, "GET", "/a/{id}"), routed("d", 1, "GET", "/a/{name}/x")
	e := routed("e", 2, "POST", "/a/{id}")
	tests := []struct {
		name          string
		gateway       *v1alpha1.KrakenDGateway
		before, after []v1alpha1.KrakenDEndpoint
		gone          string
		loser, winner string
	}{
		{"deleting x", ce, []v1alpha1.KrakenDEndpoint{x, z, v, u}, []v1alpha1.KrakenDEndpoint{z, v, u},
			"GET /a/{id}", "v", "z"},
		{"moving x off /a/{id}", ce, []v1alpha1.KrakenDEndpoint{x, z, v, u},
			[]v1alpha1.KrakenDEndpoint{routed("x", 0, "GET", "/c"), z, v, u}, "GET /a/{id}", "v", "z"},
		{"deleting x on EE", routedGateway(v1alpha1.EditionEE, nil), []v1alpha1.KrakenDEndpoint{
			routed("x", 0, "GET", "/p/{id}"), routed("z", 1, "GET", "/p/*"), routed("v", 2, "GET", "/p/q"), u,
		}, []v1alpha1.KrakenDEndpoint{routed("z", 1, "GET", "/p/*"), routed("v", 2, "GET", "/p/q"), u},
			"GET /p/{id}", "v", "z"},
		// With auto_options, e's POST /a/{id} adds the OPTIONS /a/{id} route,
		// which clashes with the OPTIONS route of d's left-out GET /a/{name}/x,
		// whether or not s's GET /a/{id} already serves that OPTIONS route.
		{"deleting s with auto_options", autoOptions, []v1alpha1.KrakenDEndpoint{s, d, e},
			[]v1alpha1.KrakenDEndpoint{d, e}, "GET /a/{id}", "e", "d"},
		{"moving s off /a/{id} with auto_options", autoOptions, []v1alpha1.KrakenDEndpoint{s, d, e},
			[]v1alpha1.KrakenDEndpoint{routed("s", 0, "GET", "/c"), d, e}, "GET /a/{id}", "e", "d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, err := New(Options{}).Render(RenderInput{Gateway: tt.gateway, Endpoints: tt.before})
			if err != nil {
				t.Fatal(err)
			}
			after, err := New(Options{}).Render(RenderInput{Gateway: tt.gateway, Endpoints: tt.after})
			if err != nil {
				t.Fatal(err)
			}

			served := map[string]bool{}
			for _, e := range servedEntries(t, after) {
				served[e] = true
			}
			for _, e := range servedEntries(t, before) {
				if e != tt.gone && !served[e] {
					t.Errorf("the write left out %s, which it never clashed with", e)
				}
			}
			loser := types.NamespacedName{Namespace: "ns", Name: tt.loser}
			for _, out := range []*RenderOutput{before, after} {
				if lost := out.EntryConflicts[loser]; len(lost) != 1 || lost[0].Winner.Name != tt.winner {
					t.Errorf("%s lost %+v, want its entry, to the older ns/%s, in both renders", tt.loser, lost, tt.winner)
				}
			}
		})
	}
}
