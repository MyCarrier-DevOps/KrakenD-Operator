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
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/types"

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewClashes(tt.before, after, tt.involving); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewClashes = %+v, want %+v", got, tt.want)
			}
		})
	}
}
