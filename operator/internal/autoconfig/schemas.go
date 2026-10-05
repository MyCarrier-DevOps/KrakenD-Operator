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
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ExtractComponentSchemas parses the OpenAPI spec JSON and returns
// the components/schemas map . Keys are preserved with original case
// so internal $ref pointers inside schema bodies (which use the
// upstream spec's casing) resolve correctly.
// Returns nil when no schemas are present or the data cannot be parsed.
func ExtractComponentSchemas(specData []byte) map[string]runtime.RawExtension {
	var spec struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(specData, &spec); err != nil {
		return nil
	}
	if len(spec.Components.Schemas) == 0 {
		return nil
	}
	result := make(map[string]runtime.RawExtension, len(spec.Components.Schemas))
	for name, raw := range spec.Components.Schemas {
		result[name] = runtime.RawExtension{Raw: raw}
	}
	return result
}

// componentSchemaPrefix is the JSON pointer prefix of a component schema
// reference inside the spec.
const componentSchemaPrefix = "#/components/schemas/"

// SchemaClosure returns the component schemas entry's documentation
// references, directly or through the schemas it references, and the sorted
// references components cannot satisfy: a schema name components does not
// define, or a local "#/..." pointer outside components/schemas. closure is nil
// when empty.
func SchemaClosure(
	entry v1alpha1.EndpointEntry,
	components map[string]runtime.RawExtension,
) (closure map[string]runtime.RawExtension, unresolved []string) {
	closure = map[string]runtime.RawExtension{}
	missing := map[string]struct{}{}
	pending := documentationRefs(entry)
	for len(pending) > 0 {
		ref := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		name, ok := schemaName(ref)
		if !ok {
			missing[ref] = struct{}{}
			continue
		}
		if _, done := closure[name]; done {
			continue
		}
		body, ok := components[name]
		if !ok {
			missing[name] = struct{}{}
			continue
		}
		closure[name] = body
		var node any
		if json.Unmarshal(body.Raw, &node) == nil {
			pending = append(pending, schemaRefs(node)...)
		}
	}
	if len(closure) == 0 {
		closure = nil
	}
	return closure, slices.Sorted(maps.Keys(missing))
}

// schemaName returns the component schema name ref denotes: ref itself when
// it is a bare name (the documentation "ref" fields), or the unescaped last
// segment of a "#/components/schemas/<name>" pointer. ok is false for any
// other pointer.
func schemaName(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "#") {
		return ref, true
	}
	name, ok := strings.CutPrefix(ref, componentSchemaPrefix)
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~"), true
}

// documentationRefs returns the schema references in entry's
// documentation/openapi extra config: each "ref" name, and the "$ref"s of
// each example_schema (an object, or base64-encoded JSON). Example payloads
// are data and are not searched.
func documentationRefs(entry v1alpha1.EndpointEntry) []string {
	if entry.ExtraConfig == nil {
		return nil
	}
	var ec map[string]json.RawMessage
	if json.Unmarshal(entry.ExtraConfig.Raw, &ec) != nil {
		return nil
	}
	var doc any
	if json.Unmarshal(ec["documentation/openapi"], &doc) != nil {
		return nil
	}
	var refs []string
	walkJSON(doc, func(key string, value any) bool {
		if payload, _ := examplePayload(key, value); payload {
			return false
		}
		s, isString := value.(string)
		switch {
		case key == "ref" && isString:
			refs = append(refs, s)
		case key == "example_schema" && isString:
			var schema any
			if raw, err := base64.StdEncoding.DecodeString(s); err == nil && json.Unmarshal(raw, &schema) == nil {
				refs = append(refs, schemaRefs(schema)...)
			}
		case key == "example_schema":
			refs = append(refs, schemaRefs(value)...)
			return false
		}
		return true
	})
	return refs
}

// schemaRefs returns the local "#/..." $ref pointers inside a JSON schema.
// Examples are data and are not searched, but the members of a "properties"
// map are schemas whatever they are named, so a property called "example" is
// searched like any other.
func schemaRefs(schema any) []string {
	var refs []string
	walkJSON(schema, func(key string, value any) bool {
		if props, ok := value.(map[string]any); ok && key == "properties" {
			for _, name := range slices.Sorted(maps.Keys(props)) {
				refs = append(refs, schemaRefs(props[name])...)
			}
			return false
		}
		if payload, _ := examplePayload(key, value); payload {
			return false
		}
		if s, ok := value.(string); ok && key == "$ref" && strings.HasPrefix(s, "#") {
			refs = append(refs, s)
		}
		return true
	})
	return refs
}
