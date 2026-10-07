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
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/types"
)

// routeRules says which of KrakenD's router rules a render's entries must
// pass together: the per-path OPTIONS routes router.auto_options adds, and,
// on an EE render, the EE wildcard rule. A CE-fallback render is not an EE
// render: the wildcards it strips are refused on their own, as on CE.
type routeRules struct {
	autoOptions, eeWildcards bool
}

// routedRoute is a route the router registers for an entry of source.
type routedRoute struct {
	route  ginRoute
	source types.NamespacedName
}

// routeAdmission is what admitting a render's entries to the router decided.
type routeAdmission struct {
	// losers are the entries left out, by position in flat, each as the
	// EntryConflict it loses.
	losers map[int]EntryConflict
	// capped says resolution stopped at MaxRouteRefusals, so clashes among
	// the entries after that point are not resolved.
	capped bool
}

// dropRouteLosers removes from flat the entries routeLosers leaves out, adds
// each to its KrakenDEndpoint's conflicts, keeping each list sorted, and
// reports whether resolution stopped at its cap.
func dropRouteLosers(flat []flatEndpoint, conflicted map[types.NamespacedName][]EntryConflict,
	rules routeRules) (kept []flatEndpoint, capped bool) {
	adm := routeLosers(flat, rules)
	if len(adm.losers) == 0 {
		return flat, adm.capped
	}
	kept = make([]flatEndpoint, 0, len(flat)-len(adm.losers))
	for i, fe := range flat {
		c, lost := adm.losers[i]
		if !lost {
			kept = append(kept, fe)
			continue
		}
		conflicted[fe.Source] = append(conflicted[fe.Source], c)
		slices.SortFunc(conflicted[fe.Source], compareConflicts)
	}
	return kept, adm.capped
}

// routeLosers admits flat's entries to KrakenD's router in serving order
// (servedBefore). An entry loses to the first older entry of another
// KrakenDEndpoint it clashes with: gin refuses to register the entry's route
// next to the older entry's (the route check's rule, on the path the
// validation copy registers). An entry gin refuses on its own is left in for
// its endpoint's own check, which refuses it.
func routeLosers(flat []flatEndpoint, rules routeRules) routeAdmission {
	adm := routeAdmission{losers: map[int]EntryConflict{}}
	var served []routedRoute
	engine := gin.New()
	for _, i := range servingOrder(flat) {
		fe := flat[i]
		method, path := entryMethod(fe.Entry.Method), fe.Entry.Endpoint
		routes := entryRoutes(method, path, rules)
		if refusedAlone(routes) {
			continue // its own endpoint's check refuses it
		}
		winner, detail, refused := registerEntry(engine, served, routes)
		if !refused {
			for _, r := range routes {
				served = append(served, routedRoute{route: r, source: fe.Source})
			}
			continue
		}
		// gin can leave its tree half-updated after a refusal; rebuild it.
		engine = engineWith(routesOf(served))
		adm.losers[i] = EntryConflict{Endpoint: path, Method: fe.Entry.Method, Winner: winner, Detail: detail}
	}
	return adm
}

// servingOrder lists the positions of flat in serving order (servedBefore).
func servingOrder(flat []flatEndpoint) []int {
	order := make([]int, len(flat))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool { return servedBefore(flat[order[x]], flat[order[y]]) })
	return order
}

// entryMethod is the method KrakenD registers an entry of method under.
func entryMethod(method string) string {
	if method == "" {
		return http.MethodGet
	}
	return strings.ToUpper(method)
}

// entryRoutes lists the routes the router registers for an entry of method
// at path: its own, on the path the validation copy registers.
func entryRoutes(method, path string, _ routeRules) []ginRoute {
	return []ginRoute{{method: method, path: ginPath(path)}}
}

// refusedAlone reports whether gin refuses one of routes in an empty engine.
func refusedAlone(routes []ginRoute) bool {
	return slices.ContainsFunc(routes, func(r ginRoute) bool { return registerRoute(gin.New(), r) != "" })
}

// registerEntry registers routes in engine. When gin refuses one it reports
// refused, with the first served route that alone clashes with it: its
// endpoint as the winner and the clash as detail.
func registerEntry(engine *gin.Engine, served []routedRoute,
	routes []ginRoute) (winner types.NamespacedName, detail string, refused bool) {
	for _, r := range routes {
		refusal := registerRoute(engine, r)
		if refusal == "" {
			continue
		}
		for _, s := range served {
			if registerRoute(engineWith([]ginRoute{s.route}), r) == "" {
				continue
			}
			return s.source, fmt.Sprintf("%s clashes with %s: %s", r.describe(), s.route.describe(), refusal), true
		}
		return types.NamespacedName{}, "", true
	}
	return types.NamespacedName{}, "", false
}

// routesOf lists the routes of routed.
func routesOf(routed []routedRoute) []ginRoute {
	routes := make([]ginRoute, len(routed))
	for i, s := range routed {
		routes[i] = s.route
	}
	return routes
}

// routerOptionsOf reads the router block of a gateway-level extra_config.
func routerOptionsOf(ec map[string]any) RouterOptions {
	raw, err := json.Marshal(ec["router"])
	if err != nil {
		return RouterOptions{}
	}
	return ParseRouterOptions(raw)
}
