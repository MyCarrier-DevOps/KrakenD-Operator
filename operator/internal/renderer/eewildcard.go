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
	"fmt"
	"sort"
	"strings"
)

// eeWildcardParam is the path parameter an EE wildcard's trailing "*" becomes
// in the copy the CE binary checks. The name mirrors the "*Wildcard"
// catch-all that the EE router registers for such a path.
const eeWildcardParam = "{Wildcard}"

// IsEEWildcard reports whether path is an EE wildcard endpoint: a trailing
// "/*" after at least one path segment ("/v1/*", not "/*").
func IsEEWildcard(path string) bool {
	return len(path) > len("/*") && strings.HasSuffix(path, "/*")
}

// rewriteEEWildcards replaces, in place, the trailing "*" of every EE
// wildcard endpoint with eeWildcardParam. Endpoints are never removed, so the
// copy stays index-aligned with the rendered config and its Sources.
func rewriteEEWildcards(endpoints []any) bool {
	changed := false
	for _, ep := range endpoints {
		m, ok := ep.(map[string]any)
		if !ok {
			continue
		}
		if path, ok := m["endpoint"].(string); ok && IsEEWildcard(path) {
			m["endpoint"] = strings.TrimSuffix(path, "*") + eeWildcardParam
			changed = true
		}
	}
	return changed
}

// eeWildcardFindings applies the EE router's rule for wildcard endpoints,
// which the CE binary cannot test. EE registers "/p/*" as the catch-all
// "/p/*Wildcard" in its method's route tree, so no other route of that
// method may start with "/p/" (measured with krakend-ee 2.13). Each conflict
// yields two krakend-style lint-pointer lines, one per endpoint, so Attribute
// maps them like any other finding.
func eeWildcardFindings(endpoints []any) []string {
	type route struct {
		index        int
		method, path string
	}
	routes := make([]route, 0, len(endpoints))
	for i, ep := range endpoints {
		m, ok := ep.(map[string]any)
		if !ok {
			continue
		}
		path, method := stringField(m, "endpoint"), stringField(m, "method")
		if method == "" {
			method = "GET"
		}
		routes = append(routes, route{index: i, method: method, path: path})
	}
	var findings []string
	for _, w := range routes {
		if !IsEEWildcard(w.path) {
			continue
		}
		prefix := strings.TrimSuffix(w.path, "*")
		for _, o := range routes {
			if o.index == w.index || o.method != w.method ||
				!strings.HasPrefix(routeShape(o.path), routeShape(prefix)) {
				continue
			}
			findings = append(findings,
				fmt.Sprintf("- at '/endpoints/%d/endpoint': EE wildcard '%s %s' conflicts with '%s %s' "+
					"(endpoints/%d): the EE router accepts no other %s route under %s",
					w.index, w.method, w.path, o.method, o.path, o.index, w.method, prefix),
				fmt.Sprintf("- at '/endpoints/%d/endpoint': '%s %s' conflicts with EE wildcard '%s %s' (endpoints/%d)",
					o.index, o.method, o.path, w.method, w.path, w.index))
		}
	}
	sort.Strings(findings)
	return findings
}

// stringField returns m[key] when it is a string, and "" otherwise.
func stringField(m map[string]any, key string) string {
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return v
}
