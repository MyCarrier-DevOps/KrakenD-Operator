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

// nameKeyedMembers returns, in sorted name order, the members of value, the
// field key of an object, when it is a name-keyed map (see nameKeyedMaps).
func nameKeyedMembers(key string, value any) (members []any, ok bool) {
	byName, isMap := value.(map[string]any)
	if !isMap || !slices.Contains(nameKeyedMaps, key) {
		return nil, false
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		members = append(members, byName[name])
	}
	return members, true
}

// walkJSON calls visit for every object field below node, in sorted key
// order, and descends into a field's value when visit returns true. The
// members of a name-keyed map (see nameKeyedMembers) are not fields: they are
// walked as objects without being visited by name.
func walkJSON(node any, visit func(key string, value any) bool) {
	switch v := node.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if !visit(k, v[k]) {
				continue
			}
			if members, ok := nameKeyedMembers(k, v[k]); ok {
				for _, member := range members {
					walkJSON(member, visit)
				}
				continue
			}
			walkJSON(v[k], visit)
		}
	case []any:
		for _, child := range v {
			walkJSON(child, visit)
		}
	}
}

// nameKeyedMaps are the keys whose object value maps names a spec author
// chooses to objects: the JSON Schema keywords that map names to schemas, and
// the OpenAPI (and Swagger 2.0) maps of paths, webhooks, components, headers,
// links, callbacks, media types and encodings. A member of such a map is an
// object whatever it is called, so a schema, response or header named
// "example" is not taken for example data. "examples" is not one of them: its
// members are Example Objects, of which only a $ref is a reference.
var nameKeyedMaps = []string{
	"properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies",
	"paths", "webhooks", "schemas", "responses", "parameters", "requestBodies",
	"headers", "securitySchemes", "links", "callbacks", "pathItems", "content", "encoding",
}

// examplePayload reports whether value, the field key of an object, is
// example data rather than part of the spec's structure, and returns the
// Example Object references inside it. The payload is the value of an
// "example" key and the content of an "examples" key, except that each entry
// of an "examples" object may itself be a {"$ref": …} object, which is a real
// reference; those entries are returned. Callers apply it to the fields of an
// object, not to the members of a name-keyed map (see nameKeyedMembers),
// which are objects whatever they are named.
func examplePayload(key string, value any) (isPayload bool, refs []map[string]any) {
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
				if _, isRef := entry["$ref"].(string); isRef {
					refs = append(refs, entry)
				}
			}
		}
		return true, refs
	}
	return false, nil
}
