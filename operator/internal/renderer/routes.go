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
	"path"
	"regexp"
	"strings"
)

// pathParamPattern matches a "/{name}" path segment the way KrakenD extracts
// endpoint parameters (lura's endpointURLKeysPattern).
var pathParamPattern = regexp.MustCompile(`/\{([a-zA-Z\-_0-9]+)\}`)

// PathParams returns the parameter names KrakenD extracts from an endpoint
// path, in order.
func PathParams(endpoint string) []string {
	matches := pathParamPattern.FindAllStringSubmatch(endpoint, -1)
	params := make([]string, 0, len(matches))
	for _, m := range matches {
		params = append(params, m[1])
	}
	return params
}

// ConflictKey returns the route KrakenD's router registers for endpoint, with
// every parameter name erased and the path cleaned as the router cleans it.
// Two paths with the same key (for example "/a/{id}" and "/a/{name}", or
// "/a//b" and "/a/b") cannot both be registered for one method.
func ConflictKey(endpoint string) string {
	shape := pathParamPattern.ReplaceAllString(endpoint, "/{}")
	cleaned := path.Clean("/" + shape)
	if strings.HasSuffix(shape, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

// RouteClashDetail says why the route of the entry (method, path) is not
// served because owner already serves otherPath, which has the same route.
func RouteClashDetail(method, path, otherPath, owner string) string {
	return ""
}
