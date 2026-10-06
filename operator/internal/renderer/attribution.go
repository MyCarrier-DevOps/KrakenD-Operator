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
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// Attribution ties one krakend check finding to the rendered endpoint entry
// it names. Index is the entry's position in the rendered "endpoints" array,
// or -1 when the finding names no endpoint (root settings, plugins); Endpoint
// is then the zero value. Endpoint is also zero when Index is past the
// Sources it was attributed against.
type Attribution struct {
	Endpoint types.NamespacedName
	Index    int
	Message  string
}

var (
	lintPointerRe = regexp.MustCompile(`^- at '/endpoints/(\d+)[/']`)
	methodRunRe   = regexp.MustCompile(`(?:endpoint: |the ')(GET|POST|PUT|PATCH|DELETE) (/\S*)(?:, backend: |' endpoint)`)
	newPathRe     = regexp.MustCompile(`path '(/[^']*)'`)
	prefixRe      = regexp.MustCompile(`prefix '(/[^']*)'`)
	braceParamRe  = regexp.MustCompile(`\{([^}/]+)\}`)
)

// renderedRoute is one rendered endpoint entry as krakend's router sees it.
type renderedRoute struct {
	method string
	shape  string
}

// Attribute maps krakend check output back to the KrakenDEndpoints that
// produced the entries it names. renderedJSON is the rendered config, whose
// endpoints array is index-aligned with sources (RenderOutput.Sources). Each
// attributable output line yields one Attribution per entry it names; a line
// that names none yields one with Index -1.
func Attribute(renderedJSON []byte, sources []types.NamespacedName, checkOutput string) []Attribution {
	routes := parseRoutes(renderedJSON)
	var out []Attribution
	for _, raw := range strings.Split(checkOutput, "\n") {
		line := strings.TrimSpace(raw)
		if skipCheckLine(line) {
			continue
		}
		indices := matchLine(line, routes)
		if len(indices) == 0 {
			out = append(out, Attribution{Index: -1, Message: line})
			continue
		}
		for _, i := range indices {
			a := Attribution{Index: i, Message: line}
			if i < len(sources) {
				a.Endpoint = sources[i]
			}
			out = append(out, a)
		}
	}
	return out
}

// skipCheckLine reports lines that carry no finding.
func skipCheckLine(line string) bool {
	return line == "" || line == "Syntax OK!" ||
		strings.HasPrefix(line, "Parsing configuration file") ||
		strings.HasPrefix(line, "ERROR linting the configuration file")
}

// matchLine returns the indices of the rendered entries line names: a lint
// pointer's index, else every entry whose method and route shape exactly match
// the method and path krakend prints for the failing endpoint, else the entries a router error about a quoted
// path (and prefix) implicates; see matchRouterError.
func matchLine(line string, routes []renderedRoute) []int {
	if m := lintPointerRe.FindStringSubmatch(line); m != nil {
		i, err := strconv.Atoi(m[1])
		if err != nil || i >= len(routes) {
			return nil
		}
		return []int{i}
	}
	var indices []int
	seen := map[int]bool{}
	add := func(match func(renderedRoute) bool) {
		for i, r := range routes {
			if !seen[i] && match(r) {
				seen[i] = true
				indices = append(indices, i)
			}
		}
	}
	for _, m := range methodRunRe.FindAllStringSubmatch(line, -1) {
		// The anchors consume krakend's own delimiters, so the captured path is
		// exactly what krakend printed for the failing endpoint.
		method, shape := m[1], routeShape(m[2])
		add(func(r renderedRoute) bool { return r.method == method && r.shape == shape })
	}
	if len(indices) > 0 {
		return indices
	}
	return matchRouterError(line, routes)
}

// matchRouterError attributes a router error that quotes "path '…'" and maybe
// "prefix '…'" but names no method. krakend keeps one route tree per method,
// so only the methods holding an entry of the new path can be at fault; within
// those (for a duplicate-handler error, only those holding the shape twice;
// for a prefix conflict, only those also holding an entry at or under the
// prefix), the blamed entries are the new path and the prefix it collides with.
// A line that quotes no path, or one no entry has, names nothing.
func matchRouterError(line string, routes []renderedRoute) []int {
	m := newPathRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	newShape := routeShape(m[1])
	minCount := 1
	if strings.Contains(line, "handlers are already registered") {
		minCount = 2
	}
	prefix := ""
	if pm := prefixRe.FindStringSubmatch(line); pm != nil {
		prefix = routeShape(pm[1])
	}
	methods := methodsHolding(routes, newShape, minCount)
	if prefix != "" {
		// A conflict needs both paths in one method's tree.
		for method := range methods {
			if !methodHoldsUnder(routes, method, prefix) {
				delete(methods, method)
			}
		}
	}
	var indices []int
	for i, r := range routes {
		if methods[r.method] && (r.shape == newShape || underPrefix(r.shape, prefix)) {
			indices = append(indices, i)
		}
	}
	return indices
}

// methodsHolding returns the methods with at least atLeast entries of the shape.
func methodsHolding(routes []renderedRoute, shape string, atLeast int) map[string]bool {
	counts := map[string]int{}
	for _, r := range routes {
		if r.shape == shape {
			counts[r.method]++
		}
	}
	methods := map[string]bool{}
	for method, n := range counts {
		if n >= atLeast {
			methods[method] = true
		}
	}
	return methods
}

// methodHoldsUnder reports whether method has an entry at or under prefix.
func methodHoldsUnder(routes []renderedRoute, method, prefix string) bool {
	for _, r := range routes {
		if r.method == method && underPrefix(r.shape, prefix) {
			return true
		}
	}
	return false
}

// underPrefix reports whether shape is the prefix route or lies beneath it.
func underPrefix(shape, prefix string) bool {
	return prefix != "" && (shape == prefix || strings.HasPrefix(shape, prefix+"/"))
}

// routeShape spells a route the way krakend's router errors do: {param} is
// written :param, and an EE wildcard's trailing "*" is written as the
// ":Wildcard" parameter it is rewritten to for the CE binary (eeWildcardParam).
func routeShape(path string) string {
	if IsEEWildcard(path) {
		path = strings.TrimSuffix(path, "*") + eeWildcardParam
	}
	return braceParamRe.ReplaceAllString(path, ":$1")
}

// parseRoutes reads the method and route shape of every rendered entry.
func parseRoutes(renderedJSON []byte) []renderedRoute {
	var doc struct {
		Endpoints []struct {
			Endpoint string `json:"endpoint"`
			Method   string `json:"method"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(renderedJSON, &doc); err != nil {
		return nil
	}
	routes := make([]renderedRoute, len(doc.Endpoints))
	for i, ep := range doc.Endpoints {
		method := ep.Method
		if method == "" {
			method = "GET"
		}
		routes[i] = renderedRoute{method: method, shape: routeShape(ep.Endpoint)}
	}
	return routes
}
