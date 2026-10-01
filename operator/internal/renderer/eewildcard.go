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

import "strings"

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
