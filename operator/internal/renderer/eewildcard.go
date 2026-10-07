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
	"context"
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

// routeShape spells a route the way krakend's router registers it: the
// parameters lura extracts are written :param (ginPath, which stops at the
// first "?"), and an EE wildcard's trailing "*" is written as the ":Wildcard"
// parameter it is rewritten to for the CE binary (eeWildcardParam).
func routeShape(path string) string {
	if IsEEWildcard(path) {
		path = strings.TrimSuffix(path, "*") + eeWildcardParam
	}
	return ginPath(path)
}

// shapeOf spells a route's shape; a variable so a test can count the calls.
var shapeOf = routeShape

// eeWildcardFindings applies the rules EE enforces for wildcard endpoints and
// the CE binary cannot test (measured with krakend-ee 2.13):
//   - Route conflicts. EE registers "/p/*" as the catch-all "/p/*Wildcard" in
//     its method's route tree, so no other route of that method may start
//     with "/p/". Each conflict yields two krakend-style lint-pointer lines,
//     one per endpoint.
//   - Parameters. The {Wildcard} parameter of the CE copy does not exist in
//     EE (eeWildcardParamFindings).
//   - Backends. A wildcard endpoint has exactly one backend
//     (eeWildcardBackendFindings).
//
// The route conflicts stop at MaxRouteRefusals, with one notice line, so the
// work and the output stay bounded however many routes conflict.
//
// Every finding is a lint-pointer line naming the entry's position.
func eeWildcardFindings(ctx context.Context, endpoints []any) ([]string, error) {
	type route struct {
		index               int
		method, path, shape string
	}
	routes := make([]route, 0, len(endpoints))
	for i, ep := range endpoints {
		m, ok := ep.(map[string]any)
		if !ok {
			continue
		}
		path := stringField(m, "endpoint")
		routes = append(routes, route{index: i, method: endpointMethod(m), path: path, shape: shapeOf(path)})
	}
	var findings []string
	conflicts := 0
	stopped := false
	for _, w := range routes {
		if stopped {
			break
		}
		if !IsEEWildcard(w.path) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wildcard check did not finish: %w", err)
		}
		prefix := strings.TrimSuffix(w.path, "*")
		prefixShape := shapeOf(prefix)
		for _, o := range routes {
			if o.index == w.index || o.method != w.method || !strings.HasPrefix(o.shape, prefixShape) {
				continue
			}
			if conflicts == MaxRouteRefusals {
				stopped = true
				break
			}
			conflicts++
			findings = append(findings,
				fmt.Sprintf("- at '/endpoints/%d/endpoint': EE wildcard '%s %s' conflicts with '%s %s' "+
					"(endpoints/%d): the EE router accepts no other %s route under %s",
					w.index, w.method, w.path, o.method, o.path, o.index, w.method, prefix),
				fmt.Sprintf("- at '/endpoints/%d/endpoint': '%s %s' conflicts with EE wildcard '%s %s' (endpoints/%d)",
					o.index, o.method, o.path, w.method, w.path, w.index))
		}
	}
	findings = append(findings, eeWildcardParamFindings(endpoints)...)
	findings = append(findings, eeWildcardBackendFindings(endpoints)...)
	sort.Strings(findings)
	if stopped {
		findings = append(findings, fmt.Sprintf("- EE wildcard check stopped after %d conflicts", conflicts))
	}
	return findings, nil
}

// eeWildcardParamFindings rejects a backend url_pattern that references
// {Wildcard} on an EE wildcard endpoint. The copy the CE binary checks
// declares that parameter, but the EE router does not, so EE refuses such a
// config with "undefined output param". An endpoint whose own path declares
// {Wildcard} is left alone: EE resolves the reference to that parameter.
func eeWildcardParamFindings(endpoints []any) []string {
	var findings []string
	for i, ep := range endpoints {
		m, ok := ep.(map[string]any)
		path := stringField(m, "endpoint")
		if !ok || !IsEEWildcard(path) || strings.Contains(path, eeWildcardParam) {
			continue
		}
		backends, ok := m["backend"].([]any)
		if !ok {
			continue
		}
		for j, b := range backends {
			bm, ok := b.(map[string]any)
			if !ok || !strings.Contains(stringField(bm, "url_pattern"), eeWildcardParam) {
				continue
			}
			findings = append(findings, fmt.Sprintf(
				"- at '/endpoints/%d/backend/%d/url_pattern': undefined output param 'Wildcard'! "+
					"endpoint: %s %s, backend: %d. input: [], output: [Wildcard]",
				i, j, endpointMethod(m), stringField(m, "endpoint"), j))
		}
	}
	return findings
}

// eeWildcardBackendFindings rejects an EE wildcard endpoint with more than one
// backend, which the EE binary refuses with "wildcard endpoint can only have
// 1 backend" while the CE copy of the endpoint is accepted.
func eeWildcardBackendFindings(endpoints []any) []string {
	var findings []string
	for i, ep := range endpoints {
		m, ok := ep.(map[string]any)
		if !ok || !IsEEWildcard(stringField(m, "endpoint")) {
			continue
		}
		if backends, ok := m["backend"].([]any); ok && len(backends) > 1 {
			findings = append(findings, fmt.Sprintf(
				"- at '/endpoints/%d/endpoint': %s %s: wildcard endpoint can only have 1 backend",
				i, endpointMethod(m), stringField(m, "endpoint")))
		}
	}
	return findings
}

// endpointMethod is the endpoint's method, GET when the config leaves it out.
func endpointMethod(m map[string]any) string {
	if method := stringField(m, "method"); method != "" {
		return method
	}
	return "GET"
}

// stringField returns m[key] when it is a string, and "" otherwise.
func stringField(m map[string]any, key string) string {
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return v
}
