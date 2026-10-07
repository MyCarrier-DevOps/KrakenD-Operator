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
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestEndpointAdmission_AClashTheGatewayAlreadyHasIsNotTheWrites(t *testing.T) {
	lost := map[types.NamespacedName][]renderer.EntryConflict{
		newEndpoint: {{Endpoint: "/a/{name}/x", Method: "GET", Winner: oldEndpoint, Detail: ginClash}},
	}
	chk := &scriptedChecker{conflicts: func(*v1alpha1.KrakenDGateway, []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		return configcheck.RouteConflicts{Lost: lost}
	}}
	stored := testEndpoint("new", "/a/{name}/x")
	updated := stored.DeepCopy()
	updated.Spec.Endpoints[0].Backends[0].URLPattern = "/changed"
	v := &EndpointValidator{Client: fakeClient(testGateway(), stored), Checker: chk}

	if resp := review(t, v, "alice", updated, stored); !resp.Allowed {
		t.Errorf("an update that keeps a stored clash was denied: %+v", resp.Result)
	}
}

func TestEndpointAdmission_ACreateRanksAfterEveryStoredEndpoint(t *testing.T) {
	var ranked []string
	chk := &scriptedChecker{conflicts: func(_ *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		for _, ep := range replace {
			ranked = append(ranked, ep.CreationTimestamp.UTC().Format("2006"))
		}
		return configcheck.RouteConflicts{}
	}}
	stored := testEndpoint("kept", "/b")
	stored.CreationTimestamp = metav1.NewTime(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	v := &EndpointValidator{Client: fakeClient(testGateway(), stored), Checker: chk}

	review(t, v, "alice", testEndpoint("new", "/a"), nil)
	updated := stored.DeepCopy()
	updated.Spec.Endpoints[0].Backends[0].URLPattern = "/changed"
	review(t, v, "alice", updated, stored)

	if !slices.Equal(ranked, []string{"9999", "2026"}) {
		t.Errorf("rendered creation years = %v, want a create after every stored endpoint (9999) and an update at its own (2026)", ranked)
	}
}

// servedFirst answers Conflicts as the renderer orders two endpoints that
// clash: the older wins, and namespace/name breaks a tie within one second.
// The loser loses GET /a/{name}/x to the winner.
func servedFirst(stored *v1alpha1.KrakenDEndpoint) func(*v1alpha1.KrakenDGateway, []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
	return func(_ *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		if len(replace) == 0 {
			return configcheck.RouteConflicts{}
		}
		winner, loser := replace[0], *stored
		if loser.CreationTimestamp.Before(&winner.CreationTimestamp) ||
			loser.CreationTimestamp.Equal(&winner.CreationTimestamp) && loser.Name < winner.Name {
			winner, loser = loser, winner
		}
		return configcheck.RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{
			{Namespace: loser.Namespace, Name: loser.Name}: {{Endpoint: "/a/{name}/x", Method: "GET",
				Winner: types.NamespacedName{Namespace: winner.Namespace, Name: winner.Name}, Detail: ginClash}},
		}}
	}
}

func TestEndpointAdmission_ACreateInTheSameSecondCannotPushAStoredEntryOut(t *testing.T) {
	second := metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	stored := testEndpoint("old", "/a/{name}/x")
	stored.CreationTimestamp = second
	created := testEndpoint("new", "/a/{id}")
	created.CreationTimestamp = second // the API server stamps a create before admission sees it
	v := &EndpointValidator{Client: fakeClient(testGateway(), stored), Checker: &scriptedChecker{conflicts: servedFirst(stored)}}

	resp := review(t, v, "alice", created, nil)

	want := "GET /a/{name}/x of KrakenDEndpoint default/old cannot be routed next to KrakenDEndpoint default/new"
	if resp.Allowed || !strings.Contains(responseText(resp), want) {
		t.Errorf("response = %+v, want a denial: the create ranks first by name and would push out %q", resp.Result, want)
	}
}

func TestEndpointAdmission_AClashCheckThatCannotRunIs500(t *testing.T) {
	chk := &scriptedChecker{conflictErr: errors.New("listing endpoints: etcd timeout")}
	v := &EndpointValidator{Client: fakeClient(testGateway()), Checker: chk}

	resp := review(t, v, "alice", testEndpoint("new", "/a"), nil)

	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want a transient 500", resp.Result)
	}
}

func TestEndpointAdmission_RefusesAWriteWhileClashResolutionIsCapped(t *testing.T) {
	chk := &scriptedChecker{conflicts: func(*v1alpha1.KrakenDGateway, []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		return configcheck.RouteConflicts{Capped: true}
	}}
	v := &EndpointValidator{Client: fakeClient(testGateway()), Checker: chk}

	resp := review(t, v, "alice", testEndpoint("new", "/a"), nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(responseText(resp), configcheck.ClashesCapped) {
		t.Errorf("response = %+v, want a 422 denial saying the clashes cannot be told apart", resp.Result)
	}
}
