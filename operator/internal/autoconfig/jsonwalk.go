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

package autoconfig

import (
	"maps"
	"slices"
)

// walkJSON calls visit for every object member below node, in sorted key
// order, and descends into a member's value when visit returns true.
func walkJSON(node any, visit func(key string, value any) bool) {
	switch v := node.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if visit(k, v[k]) {
				walkJSON(v[k], visit)
			}
		}
	case []any:
		for _, child := range v {
			walkJSON(child, visit)
		}
	}
}

// examplePayload reports whether value, the member key of an object, is
// example data rather than part of the spec's structure, and returns the
// $refs that are real references inside it. The payload is the value of an
// "example" key and the content of an "examples" key, except that each entry
// of an "examples" object may itself be a $ref to an Example Object. A schema
// property literally named "example" or "examples" is taken for a payload too.
func examplePayload(key string, value any) (isPayload bool, refs []string) {
	switch key {
	case "example":
		return true, nil
	case "examples":
		entries, ok := value.(map[string]any)
		if !ok {
			return true, nil
		}
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			if entry, ok := entries[name].(map[string]any); ok {
				if ref, ok := entry["$ref"].(string); ok {
					refs = append(refs, ref)
				}
			}
		}
		return true, refs
	}
	return false, nil
}
