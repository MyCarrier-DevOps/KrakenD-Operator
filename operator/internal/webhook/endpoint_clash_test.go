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


package webhook

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

const ginClash = "':name' in new path '/a/:name/x' conflicts with existing wildcard ':id' in existing prefix '/a/:id'"

var (
	newEndpoint = types.NamespacedName{Namespace: "default", Name: "new"}
	oldEndpoint = types.NamespacedName{Namespace: "default", Name: "old"}
)

// clashWith answers Conflicts with lost whenever the replace set holds an
// endpoint named name, and with none otherwise: the clash is the candidate's.
func clashWith(name string, lost map[types.NamespacedName][]renderer.EntryConflict) func(
	*v1alpha1.KrakenDGateway, []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
	return func(_ *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		if slices.ContainsFunc(replace, func(ep v1alpha1.KrakenDEndpoint) bool { return ep.Name == name }) {
			return configcheck.RouteConflicts{Lost: lost}
		}
		return configcheck.RouteConflicts{}
	}
}

func TestEndpointAdmission_RefusesAnEntryTheRouterCannotServeNextToAnOlderOne(t *testing.T) {
	chk := &scriptedChecker{conflicts: clashWith("new", map[types.NamespacedName][]renderer.EntryConflict{
		newEndpoint: {{Endpoint: "/a/{name}/x", Method: "GET", Winner: oldEndpoint, Detail: ginClash}},
	})}
	v := &EndpointValidator{Client: fakeClient(testGateway()), Checker: chk}

	resp := review(t, v, "alice", testEndpoint("new", "/a/{name}/x"), nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	if text := responseText(resp); !strings.Contains(text, "default/old") || !strings.Contains(text, ginClash) {
		t.Errorf("denial %q must name default/old and the router's refusal", text)
	}
	if len(chk.calls) != 0 {
		t.Errorf("checks = %v, want none after a structural refusal", chk.calls)
	}
}

func TestEndpointAdmission_RefusesAWriteThatKeepsAnOlderEndpointOutOfTheRouter(t *testing.T) {
	chk := &scriptedChecker{conflicts: clashWith("new", map[types.NamespacedName][]renderer.EntryConflict{
		oldEndpoint: {{Endpoint: "/a/{name}/x", Method: "GET", Winner: newEndpoint, Detail: ginClash}},
	})}
	stored := testEndpoint("new", "/b")
	updated := testEndpoint("new", "/a/{id}")
	v := &EndpointValidator{Client: fakeClient(testGateway(), stored), Checker: chk}

	resp := review(t, v, "alice", updated, stored)

	if resp.Allowed || !strings.Contains(responseText(resp), "default/old") {
		t.Errorf("response = %+v, want a denial naming the endpoint the update would push out", resp.Result)
	}
}
