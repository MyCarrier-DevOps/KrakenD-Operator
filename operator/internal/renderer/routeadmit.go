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

// routedEntry is an entry of source, by method, path and route shape, for the
// EE wildcard rule.
type routedEntry struct {
	method, path, shape string
	source              types.NamespacedName
}

// routerClash is an older entry of another KrakenDEndpoint that an entry
// clashes with: its endpoint, and the clash as detail.
type routerClash struct {
	winner types.NamespacedName
	detail string
}

// routeAdmission is what admitting a render's entries to the router decided.
type routeAdmission struct {
	// losers are the entries left out, by position in flat, each as the
	// EntryConflicts it loses: one for every older KrakenDEndpoint it clashes
	// with.
	losers map[int][]EntryConflict
	// capped says resolution stopped at MaxRouteRefusals, so clashes among
	// the entries after that point are not resolved.
	capped bool
}

// dropRouteLosers removes from flat the entries routeLosers leaves out, adds
// each to its KrakenDEndpoint's conflicts, keeping each list sorted, and
// reports whether resolution stopped at its cap. shadowed are the entries
// that lost a same-shape conflict, which flat no longer holds.
func dropRouteLosers(flat, shadowed []flatEndpoint, conflicted map[types.NamespacedName][]EntryConflict,
	rules routeRules) (kept []flatEndpoint, capped bool) {
	adm := routeLosers(flat, shadowed, rules)
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
		conflicted[fe.Source] = append(conflicted[fe.Source], c...)
		slices.SortFunc(conflicted[fe.Source], compareConflicts)
	}
	return kept, adm.capped
}

// routeLosers admits flat's entries to KrakenD's router in serving order
// (servedBefore). An entry loses to any older entry of another
// KrakenDEndpoint it clashes with, whether that entry is served or was left
// out itself:
//   - gin refuses to register the entry's route, or the OPTIONS route
//     router.auto_options adds for its path, next to the older entry's (the
//     route check's rule, on the path the validation copy registers);
//   - on an EE render, the entry lies under an older EE wildcard of its
//     method, or is an EE wildcard over an older entry of its method (the EE
//     router's rule, eeWildcardFindings).
//
// A losing entry is recorded once for every older KrakenDEndpoint it clashes
// with, each with its own detail, so a write that adds one more clash is seen
// as new. Only the loss decides what is served: whether one older entry or
// several are named does not.
//
// Losing also to entries that were left out keeps every entry's outcome
// independent of whether older entries are served: removing an endpoint, or
// one of its entries, can bring entries back but never leave another out. For
// the same reason the pairwise check, and the record of an entry left out,
// list the OPTIONS route of the entry's path even when a served entry already
// added it (entryRoutes with nil options).
//
// The entries of shadowed lost a same-shape conflict and are left out
// already. Each is recorded at its place in serving order as an older entry
// left out, so a newer entry that clashes with it loses to it whether or not
// its same-shape winner is served: removing the winner can then promote it
// without leaving another entry out. It is neither counted nor reported
// again, and one gin refuses on its own records nothing, like an entry left
// in for its own check.
//
// An entry gin refuses on its own, or next to its own KrakenDEndpoint's
// entries, is left in for that endpoint's own check, which refuses it; so is
// a clash with the gateway's own routes, which are not registered here. A
// clash that needs several older routes together leaves the entry in too.
// Each entry lost, and each such clash, rebuilds the engine, so after
// MaxRouteRefusals of them resolution stops with capped set: the remaining
// entries are rendered as they are, and the full check reports what is left.
// The cap counts entries, not the clashes of one entry.
func routeLosers(flat, shadowed []flatEndpoint, rules routeRules) routeAdmission {
	adm := routeAdmission{losers: map[int][]EntryConflict{}}
	var served, dropped []routedRoute
	var older []routedEntry
	own := map[types.NamespacedName][]ginRoute{}
	ownEngines := map[types.NamespacedName]*gin.Engine{}
	options := map[string]bool{}
	engine := gin.New()
	refusals := 0
	all := append(slices.Clip(flat), shadowed...)
	for _, i := range servingOrder(all) {
		fe := all[i]
		if i >= len(flat) {
			pair := entryRoutes(entryMethod(fe.Entry.Method), fe.Entry.Endpoint, rules, nil)
			if !refusedAlone(pair) {
				for _, r := range pair {
					dropped = append(dropped, routedRoute{route: r, source: fe.Source})
				}
			}
			continue
		}
		if refusals == MaxRouteRefusals {
			adm.capped = true
			break
		}
		entry := routedEntry{method: entryMethod(fe.Entry.Method), path: fe.Entry.Endpoint, source: fe.Source}
		entry.shape = shapeOf(entry.path)
		routes := entryRoutes(entry.method, entry.path, rules, options)
		if refusedAlone(routes) || refusedByOwn(ownEngines, own, fe.Source, routes) {
			continue // its own endpoint's check refuses it
		}
		pair := entryRoutes(entry.method, entry.path, rules, nil)
		clashes := olderClashes(rules, older, dropped, entry, pair)
		if len(clashes) == 0 {
			if registerAll(engine, routes) {
				for _, r := range routes {
					served = append(served, routedRoute{route: r, source: fe.Source})
					if r.method == http.MethodOptions {
						options[r.path] = true
					}
				}
				own[fe.Source] = append(own[fe.Source], routes...)
				older = append(older, entry)
				continue
			}
			// gin can leave its tree half-updated after a refusal; rebuild it.
			engine = engineWith(routesOf(served))
		}
		clashes = withClashes(clashes, pairwiseClashes(served, entry.source, routes))
		refusals++
		// The entry's routes went into its own engine on trial; take them out.
		ownEngines[fe.Source] = engineWith(own[fe.Source])
		for _, c := range clashes {
			adm.losers[i] = append(adm.losers[i], EntryConflict{
				Endpoint: entry.path, Method: fe.Entry.Method, Winner: c.winner, Detail: c.detail,
			})
		}
		for _, r := range pair {
			dropped = append(dropped, routedRoute{route: r, source: fe.Source})
		}
		older = append(older, entry)
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
// at path: its own, on the path the validation copy registers, and, with
// auto_options, the OPTIONS route of that path unless options says a served
// entry already added it. With nil options it always lists that OPTIONS
// route, as the pairwise check against entries left out needs.
func entryRoutes(method, path string, rules routeRules, options map[string]bool) []ginRoute {
	p := ginPath(path)
	if rules.eeWildcards {
		p = routeShape(path)
	}
	routes := []ginRoute{{method: method, path: p}}
	if rules.autoOptions && !options[p] {
		routes = append(routes, ginRoute{method: http.MethodOptions, path: p})
	}
	return routes
}

// refusedAlone reports whether gin refuses one of routes in an empty engine.
func refusedAlone(routes []ginRoute) bool {
	return slices.ContainsFunc(routes, func(r ginRoute) bool { return registerRoute(gin.New(), r) != "" })
}

// sharedOptions reports whether a and b are the one OPTIONS route
// router.auto_options registers for a path that several entries share.
func sharedOptions(a, b ginRoute) bool {
	return a.method == http.MethodOptions && b.method == http.MethodOptions && a.path == b.path
}

// registerAll registers routes in engine and reports whether gin accepted
// every one. After a refusal the engine may be half-updated.
func registerAll(engine *gin.Engine, routes []ginRoute) bool {
	for _, r := range routes {
		if registerRoute(engine, r) != "" {
			return false
		}
	}
	return true
}

// routesOf lists the routes of routed.
func routesOf(routed []routedRoute) []ginRoute {
	routes := make([]ginRoute, len(routed))
	for i, s := range routed {
		routes[i] = s.route
	}
	return routes
}

// refusedByOwn reports whether gin refuses one of routes next to the served
// routes of source's own entries. It registers routes in source's own engine
// on trial; the caller rebuilds that engine when the entry is not served.
func refusedByOwn(engines map[types.NamespacedName]*gin.Engine, own map[types.NamespacedName][]ginRoute,
	source types.NamespacedName, routes []ginRoute) bool {
	engine, ok := engines[source]
	if !ok {
		engine = gin.New()
		engines[source] = engine
	}
	for _, r := range routes {
		if registerRoute(engine, r) != "" {
			engines[source] = engineWith(own[source])
			return true
		}
	}
	return false
}

// olderClashes lists the older entries of other KrakenDEndpoints that entry
// clashes with without registering it: under the EE wildcard rule, against
// every older entry, and in gin, pairwise, against the routes of the older
// entries left out. Served entries are checked by registering entry in the
// engine (registerAll) and, for the clashes it names, by pairwiseClashes.
func olderClashes(rules routeRules, older []routedEntry, dropped []routedRoute, entry routedEntry,
	routes []ginRoute) []routerClash {
	var clashes []routerClash
	if rules.eeWildcards {
		clashes = eeWildcardOverlaps(older, entry)
	}
	return withClashes(clashes, pairwiseClashes(dropped, entry.source, routes))
}

// pairwiseClashes lists, for each KrakenDEndpoint other than source, the first
// of its routes in against that one of routes alone clashes with in gin.
func pairwiseClashes(against []routedRoute, source types.NamespacedName, routes []ginRoute) []routerClash {
	var clashes []routerClash
	seen := map[types.NamespacedName]bool{}
	for _, a := range against {
		if a.source == source || seen[a.source] {
			continue
		}
		for _, r := range routes {
			if sharedOptions(a.route, r) {
				continue
			}
			if refusal := registerRoute(engineWith([]ginRoute{a.route}), r); refusal != "" {
				seen[a.source] = true
				clashes = append(clashes, routerClash{winner: a.source,
					detail: fmt.Sprintf("%s clashes with %s: %s", r.describe(), a.route.describe(), refusal)})
				break
			}
		}
	}
	return clashes
}

// withClashes appends the clashes of more with a KrakenDEndpoint that clashes
// lacks.
func withClashes(clashes, more []routerClash) []routerClash {
	for _, m := range more {
		if !slices.ContainsFunc(clashes, func(c routerClash) bool { return c.winner == m.winner }) {
			clashes = append(clashes, m)
		}
	}
	return clashes
}

// eeWildcardOverlaps applies the EE router's wildcard rule (eeWildcardFindings)
// between entry and the older entries of other KrakenDEndpoints. It reports,
// for each, the first that is an EE wildcard of the method entry lies under,
// or that lies under entry when entry is an EE wildcard.
func eeWildcardOverlaps(older []routedEntry, entry routedEntry) []routerClash {
	var clashes []routerClash
	for _, o := range older {
		if o.method != entry.method || o.source == entry.source {
			continue
		}
		if prefix := strings.TrimSuffix(o.path, "*"); IsEEWildcard(o.path) &&
			strings.HasPrefix(entry.shape, shapeOf(prefix)) {
			clashes = withClashes(clashes, []routerClash{{winner: o.source,
				detail: fmt.Sprintf("'%s %s' conflicts with EE wildcard '%s %s': "+
					"the EE router accepts no other %s route under %s",
					entry.method, entry.path, o.method, o.path, entry.method, prefix)}})
			continue
		}
		if prefix := strings.TrimSuffix(entry.path, "*"); IsEEWildcard(entry.path) &&
			strings.HasPrefix(o.shape, shapeOf(prefix)) {
			clashes = withClashes(clashes, []routerClash{{winner: o.source,
				detail: fmt.Sprintf("EE wildcard '%s %s' conflicts with '%s %s': "+
					"the EE router accepts no other %s route under %s",
					entry.method, entry.path, o.method, o.path, entry.method, prefix)}})
		}
	}
	return clashes
}

// routerOptionsOf reads the router block of a gateway-level extra_config.
func routerOptionsOf(ec map[string]any) RouterOptions {
	raw, err := json.Marshal(ec["router"])
	if err != nil {
		return RouterOptions{}
	}
	return ParseRouterOptions(raw)
}
