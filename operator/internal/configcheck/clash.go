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
	"cmp"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/types"

	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// RouteConflicts is what a render of a gateway leaves out. Lost holds, by the
// KrakenDEndpoint that loses them, the entries the render does not serve
// because an older endpoint's entry (or an earlier entry of the same
// endpoint) is served instead. Capped says the render stopped resolving
// router clashes at renderer.MaxRouteRefusals, so Lost misses the clashes
// among the entries after that point.
type RouteConflicts struct {
	Lost   map[types.NamespacedName][]renderer.EntryConflict
	Capped bool
}

// Clash is an entry of Loser that KrakenD's router cannot serve next to an
// entry of Winner, an older endpoint, whether or not the render serves it,
// although their routes differ in shape. Detail is the router's refusal; it
// names methods and paths only, so it may be shown to either endpoint's owner.
type Clash struct {
	Loser            types.NamespacedName
	Method, Endpoint string
	Winner           types.NamespacedName
	Detail           string
}

// String renders c for a denial or a hold.
func (c Clash) String() string {
	return fmt.Sprintf("%s %s of KrakenDEndpoint %s cannot be routed next to KrakenDEndpoint %s, "+
		"which is older: %s", c.Method, c.Endpoint, c.Loser, c.Winner, c.Detail)
}

// NewClashes returns the router clashes of after that involve one of
// involving as the loser or as the winner; a nil involving takes every endpoint. Same-shape
// conflicts are left out: the route uniqueness rules and the oldest-wins
// render settle those. The result is sorted by loser, endpoint and method.
func NewClashes(_, after RouteConflicts, involving map[types.NamespacedName]bool) []Clash {
	var out []Clash
	for loser, lost := range after.Lost {
		for _, e := range lost {
			if e.Detail == "" || involving != nil && !involving[loser] && !involving[e.Winner] {
				continue
			}
			out = append(out, Clash{
				Loser: loser, Method: e.Method, Endpoint: e.Endpoint, Winner: e.Winner, Detail: e.Detail,
			})
		}
	}
	slices.SortFunc(out, func(a, b Clash) int {
		return cmp.Or(cmp.Compare(a.Loser.String(), b.Loser.String()),
			cmp.Compare(a.Endpoint, b.Endpoint), cmp.Compare(a.Method, b.Method))
	})
	return out
}
