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
	"context"
	"fmt"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

func TestNewClashes_KeepsTheNewRouterClashesOfTheGivenEndpoints(t *testing.T) {
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}
	c := types.NamespacedName{Namespace: "ns", Name: "c"}
	clash := renderer.EntryConflict{Endpoint: "/u/{n}/x", Method: "GET", Winner: a, Detail: "wildcard conflict"}
	sameShape := renderer.EntryConflict{Endpoint: "/u/{n}", Method: "GET", Winner: a}
	after := RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{b: {clash, sameShape}, c: {clash}}}
	tests := []struct {
		name      string
		before    RouteConflicts
		involving map[types.NamespacedName]bool
		want      []Clash
	}{
		{"every new clash", RouteConflicts{}, nil, []Clash{
			{Loser: b, Method: "GET", Endpoint: "/u/{n}/x", Winner: a, Detail: "wildcard conflict"},
			{Loser: c, Method: "GET", Endpoint: "/u/{n}/x", Winner: a, Detail: "wildcard conflict"},
		}},
		{"only those of the loser asked about", RouteConflicts{}, map[types.NamespacedName]bool{b: true}, []Clash{
			{Loser: b, Method: "GET", Endpoint: "/u/{n}/x", Winner: a, Detail: "wildcard conflict"},
		}},
		{"the winner asked about", RouteConflicts{}, map[types.NamespacedName]bool{a: true}, []Clash{
			{Loser: b, Method: "GET", Endpoint: "/u/{n}/x", Winner: a, Detail: "wildcard conflict"},
			{Loser: c, Method: "GET", Endpoint: "/u/{n}/x", Winner: a, Detail: "wildcard conflict"},
		}},
		{"a clash the gateway already has, whatever the router's words", RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{
			b: {{Endpoint: "/u/{n}/x", Method: "GET", Winner: a, Detail: "wildcard conflict in existing prefix '/u/:id'"}},
			c: {clash},
		}}, nil, nil},
		{"a clash differing from a stored one in a single field is new", RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{
			b: {
				{Endpoint: "/u/{n}/y", Method: "GET", Winner: a, Detail: "wildcard conflict"},
				{Endpoint: "/u/{n}/x", Method: "POST", Winner: a, Detail: "wildcard conflict"},
				{Endpoint: "/u/{n}/x", Method: "GET", Winner: c, Detail: "wildcard conflict"},
			},
			c: {clash},
		}}, nil, []Clash{
			{Loser: b, Method: "GET", Endpoint: "/u/{n}/x", Winner: a, Detail: "wildcard conflict"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewClashes(tt.before, after, tt.involving); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewClashes = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConflicts_RendersTheGatewayWithTheReplacement(t *testing.T) {
	chk := newChecker(&fakeValidator{}, endpoint("a", "/a/{id}"))
	replace := []v1alpha1.KrakenDEndpoint{*endpoint("b", "/a/{name}")}

	conflicts, err := chk.Conflicts(context.Background(), gateway(v1alpha1.EditionCE), replace)

	if err != nil {
		t.Fatal(err)
	}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}
	want := []renderer.EntryConflict{{Endpoint: "/a/{name}", Method: "GET", Winner: types.NamespacedName{Namespace: "ns", Name: "a"}}}
	if !reflect.DeepEqual(conflicts.Lost[b], want) || conflicts.Capped {
		t.Errorf("Conflicts = %+v, want ns/b losing its same-shape entry to ns/a, uncapped", conflicts)
	}
}

func TestClash_StringNamesBothEndpointsAndTheRoutersWords(t *testing.T) {
	clash := Clash{
		Loser: types.NamespacedName{Namespace: "ns", Name: "b"}, Method: "GET", Endpoint: "/u/{n}/x",
		Winner: types.NamespacedName{Namespace: "ns", Name: "a"}, Detail: "wildcard conflict",
	}

	want := "GET /u/{n}/x of KrakenDEndpoint ns/b cannot be routed next to KrakenDEndpoint ns/a, " +
		"which is older: wildcard conflict"
	if got := clash.String(); got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestConflicts_ReportsARenderThatStoppedResolvingClashes(t *testing.T) {
	var stored []v1alpha1.KrakenDEndpoint
	for i := range renderer.MaxRouteRefusals + 1 {
		stored = append(stored, *endpoint(fmt.Sprintf("old-%02d", i), fmt.Sprintf("/c%d/{id}", i)),
			*endpoint(fmt.Sprintf("x-new-%02d", i), fmt.Sprintf("/c%d/{name}/x", i)))
	}
	chk := newChecker(&fakeValidator{})

	conflicts, err := chk.Conflicts(context.Background(), gateway(v1alpha1.EditionCE), stored)

	if err != nil {
		t.Fatal(err)
	}
	if !conflicts.Capped || len(conflicts.Lost) != renderer.MaxRouteRefusals {
		t.Errorf("capped = %v with %d losers, want capped after %d", conflicts.Capped, len(conflicts.Lost), renderer.MaxRouteRefusals)
	}
}

func TestNewClashes_TakesEachWinnerOfOneEntryAsAClashOfItsOwn(t *testing.T) {
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}
	c := types.NamespacedName{Namespace: "ns", Name: "c"}
	toA := renderer.EntryConflict{Endpoint: "/u/{n}/x", Method: "GET", Winner: a, Detail: "clash with a"}
	toC := renderer.EntryConflict{Endpoint: "/u/{n}/x", Method: "GET", Winner: c, Detail: "clash with c"}
	before := RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{b: {toA}}}
	after := RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{b: {toA, toC}}}

	got := NewClashes(before, after, map[types.NamespacedName]bool{c: true})

	want := []Clash{{Loser: b, Method: "GET", Endpoint: "/u/{n}/x", Winner: c, Detail: "clash with c"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NewClashes = %+v, want only the entry's new clash with ns/c", got)
	}
}

func TestNewClashes_OrdersAnEntrysClashesByWinner(t *testing.T) {
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}
	c := types.NamespacedName{Namespace: "ns", Name: "c"}
	after := RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{b: {
		{Endpoint: "/u/{n}/x", Method: "GET", Winner: c, Detail: "clash with c"},
		{Endpoint: "/u/{n}/x", Method: "GET", Winner: a, Detail: "clash with a"},
	}}}

	got := NewClashes(RouteConflicts{}, after, nil)

	if len(got) != 2 || got[0].Winner != a || got[1].Winner != c {
		t.Errorf("NewClashes = %+v, want the clash with ns/a, then the one with ns/c", got)
	}
}
