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
	"context"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// NotYetCreated is the creationTimestamp to give an endpoint that has none
// when it is rendered with endpoints that do: the end of year 9999, after any
// real one. The API server sets an object's creationTimestamp before
// admission sees it, so only a caller that builds an endpoint itself (a test,
// or the AutoConfig precheck for an endpoint it has yet to create) hands one
// over without it; a zero timestamp would rank it the oldest of all.
var NotYetCreated = metav1.NewTime(time.Unix(253402300799, 0))

// ClashesCapped says why a write is refused while a gateway's render stops
// resolving router clashes at its cap (RouteConflicts.Capped).
const ClashesCapped = "the gateway has more router clashes than the operator resolves at once, " +
	"so a new one cannot be told apart from them; delete the clashing KrakenDEndpoints first " +
	"(a delete is always admitted)"

// RouteConflicts is what a render of a gateway leaves out. Lost holds, by the
// KrakenDEndpoint that loses them, the entries the render does not serve
// because they share a route shape with, or clash in the router with, an
// entry of an older endpoint (served or not), or an earlier entry of the same
// endpoint. Capped says the render stopped resolving
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

// clashKey identifies a clash by which entry of which endpoint loses to which
// endpoint. The router's refusal is left out: gin words it after the tree it
// has built so far, which other routes change.
type clashKey struct {
	loser, winner    types.NamespacedName
	method, endpoint string
}

// NewClashes returns the router clashes of after that before does not have,
// among those that involve one of involving as the loser or as the winner; a
// nil involving takes every endpoint. A clash before has is the same clash
// when the same entry of the same endpoint loses to the same endpoint, however
// the router words it. Same-shape conflicts are left out: the route uniqueness
// rules and the oldest-wins render settle those. The result is sorted by
// loser, endpoint and method.
func NewClashes(before, after RouteConflicts, involving map[types.NamespacedName]bool) []Clash {
	known := map[clashKey]bool{}
	for loser, lost := range before.Lost {
		for _, e := range lost {
			known[clashKey{loser: loser, winner: e.Winner, method: e.Method, endpoint: e.Endpoint}] = true
		}
	}
	var out []Clash
	for loser, lost := range after.Lost {
		for _, e := range lost {
			key := clashKey{loser: loser, winner: e.Winner, method: e.Method, endpoint: e.Endpoint}
			if e.Detail == "" || known[key] || involving != nil && !involving[loser] && !involving[e.Winner] {
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

// Conflicts renders gw's endpoints, with replace substituted or added by
// namespace/name, and returns what the render leaves out. It renders in
// process: no validation slot is held and nothing is executed.
func (c *Checker) Conflicts(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	replace []v1alpha1.KrakenDEndpoint) (RouteConflicts, error) {
	// Nothing read here leaves the Checker and the renderer never mutates its
	// inputs, so the cache's objects can be used without copying them.
	in, err := c.gather(ctx, gw, replace, nil, client.UnsafeDisableDeepCopy)
	if err != nil {
		return RouteConflicts{}, err
	}
	out, err := c.renderer.Render(in)
	if err != nil {
		return RouteConflicts{}, fmt.Errorf("rendering config: %w", err)
	}
	return RouteConflicts{Lost: out.EntryConflicts, Capped: out.RouteResolutionCapped}, nil
}
