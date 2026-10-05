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
	"maps"
	"slices"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestExtractComponentSchemas_Valid(t *testing.T) {
	spec := []byte(`{
		"components": {
			"schemas": {
				"User": {"type": "object", "properties": {"name": {"type": "string"}}},
				"Error": {"type": "object", "properties": {"code": {"type": "integer"}}}
			}
		}
	}`)

	result := ExtractComponentSchemas(spec)
	if len(result) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(result))
	}
	if _, ok := result["User"]; !ok {
		t.Error("expected key 'User' (original case preserved)")
	}
	if _, ok := result["Error"]; !ok {
		t.Error("expected key 'Error' (original case preserved)")
	}
}

func TestExtractComponentSchemas_PreservesCase(t *testing.T) {
	spec := []byte(`{
		"components": {
			"schemas": {
				"MC.Address.Contracts.Models.PublicContracts.ApiResponse": {"type": "object"}
			}
		}
	}`)

	result := ExtractComponentSchemas(spec)
	if len(result) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(result))
	}
	key := "MC.Address.Contracts.Models.PublicContracts.ApiResponse"
	if _, ok := result[key]; !ok {
		t.Errorf("expected key %q (original case preserved)", key)
	}
}

func TestExtractComponentSchemas_NoComponents(t *testing.T) {
	spec := []byte(`{"paths": {"/api/users": {}}}`)
	result := ExtractComponentSchemas(spec)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestExtractComponentSchemas_EmptySchemas(t *testing.T) {
	spec := []byte(`{"components": {"schemas": {}}}`)
	result := ExtractComponentSchemas(spec)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestExtractComponentSchemas_InvalidJSON(t *testing.T) {
	result := ExtractComponentSchemas([]byte(`not-json`))
	if result != nil {
		t.Errorf("expected nil for invalid JSON, got %v", result)
	}
}

func TestExtractComponentSchemas_PreservesRawContent(t *testing.T) {
	spec := []byte(`{
		"components": {
			"schemas": {
				"Pet": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "id": {"type": "integer"}}}
			}
		}
	}`)

	result := ExtractComponentSchemas(spec)
	raw := result["Pet"]
	if raw.Raw == nil {
		t.Fatal("expected non-nil raw data")
	}
	// Verify it round-trips as valid JSON
	var roundtrip runtime.RawExtension
	roundtrip.Raw = raw.Raw
	if roundtrip.Raw == nil {
		t.Error("round-trip failed")
	}
}

// docEntry returns an entry whose documentation/openapi extra config is doc.
func docEntry(path, doc string) v1alpha1.EndpointEntry {
	return v1alpha1.EndpointEntry{
		Endpoint:    path,
		Method:      "GET",
		Backends:    []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: path}},
		ExtraConfig: &runtime.RawExtension{Raw: []byte(`{"documentation/openapi":` + doc + `}`)},
	}
}

// petSchemas is a components map: Pet references Owner, Owner references
// Address; Error's example carries a $ref that is data; Unused stands alone.
func petSchemas() map[string]runtime.RawExtension {
	return map[string]runtime.RawExtension{
		"Pet":     {Raw: []byte(`{"type":"object","properties":{"owner":{"$ref":"#/components/schemas/Owner"}}}`)},
		"Owner":   {Raw: []byte(`{"type":"object","properties":{"address":{"$ref":"#/components/schemas/Address"}}}`)},
		"Address": {Raw: []byte(`{"type":"object"}`)},
		"Error":   {Raw: []byte(`{"type":"object","example":{"$ref":"#/components/schemas/Unused"}}`)},
		"Unused":  {Raw: []byte(`{"type":"object"}`)},
	}
}

func TestSchemaClosure_FollowsRefsTransitively(t *testing.T) {
	allOf := base64.StdEncoding.EncodeToString([]byte(`{"allOf":[{"$ref":"#/components/schemas/Pet"}]}`))
	entry := docEntry("/pets", `{"request_definition":[{"ref":"Error"}],`+
		`"response_definition":{"200":{"example_schema":"`+allOf+`","example":{"ref":"Unused"}}}}`)

	closure, unresolved := SchemaClosure(entry, petSchemas())

	if got, want := slices.Sorted(maps.Keys(closure)), []string{"Address", "Error", "Owner", "Pet"}; !slices.Equal(got, want) {
		t.Errorf("closure = %v, want %v", got, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestSchemaClosure_ReportsUnresolvableRefs(t *testing.T) {
	entry := docEntry("/pets", `{"response_definition":{"200":{"ref":"Ghost"},`+
		`"400":{"example_schema":{"type":"object","properties":{"x":{"$ref":"#/definitions/Legacy"}}}}}}`)

	closure, unresolved := SchemaClosure(entry, petSchemas())

	if closure != nil {
		t.Errorf("closure = %v, want nil", closure)
	}
	if want := []string{"#/definitions/Legacy", "Ghost"}; !slices.Equal(unresolved, want) {
		t.Errorf("unresolved = %v, want %v", unresolved, want)
	}
}

// A schema property may be named "example" or "examples". Its value is a
// schema, so a $ref there is a real reference, unlike an example payload.
func TestSchemaClosure_SeesRefsUnderPropertiesNamedExample(t *testing.T) {
	components := petSchemas()
	components["Odd"] = runtime.RawExtension{Raw: []byte(`{"type":"object","properties":{` +
		`"example":{"$ref":"#/components/schemas/Address","example":{"$ref":"#/components/schemas/Unused"}},` +
		`"examples":{"type":"array","items":{"$ref":"#/components/schemas/Owner"}}}}`)}
	entry := docEntry("/odd", `{"response_definition":{"200":{"ref":"Odd"}}}`)

	closure, unresolved := SchemaClosure(entry, components)

	if got, want := slices.Sorted(maps.Keys(closure)), []string{"Address", "Odd", "Owner"}; !slices.Equal(got, want) {
		t.Errorf("closure = %v, want %v", got, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestSchemaClosure_TerminatesOnCycles(t *testing.T) {
	components := map[string]runtime.RawExtension{
		"Node": {Raw: []byte(`{"properties":{"next":{"$ref":"#/components/schemas/Node"},` +
			`"peer":{"$ref":"#/components/schemas/Other"}}}`)},
		"Other": {Raw: []byte(`{"properties":{"back":{"$ref":"#/components/schemas/Node"}}}`)},
	}
	entry := docEntry("/nodes", `{"response_definition":{"200":{"ref":"Node"}}}`)

	closure, unresolved := SchemaClosure(entry, components)

	if got, want := slices.Sorted(maps.Keys(closure)), []string{"Node", "Other"}; !slices.Equal(got, want) {
		t.Errorf("closure = %v, want %v", got, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

// An "examples" entry that is a $ref points at an Example Object under
// components/examples, which is not a schema, so the closure neither attaches
// nor reports it.
func TestSchemaClosure_IgnoresRefsToExampleObjects(t *testing.T) {
	components := map[string]runtime.RawExtension{
		"Pet": {Raw: []byte(`{"type":"object","examples":{"fido":{"$ref":"#/components/examples/Fido"}}}`)},
	}
	entry := docEntry("/pets", `{"response_definition":{"200":{"ref":"Pet"}}}`)

	closure, unresolved := SchemaClosure(entry, components)

	if got, want := slices.Sorted(maps.Keys(closure)), []string{"Pet"}; !slices.Equal(got, want) {
		t.Errorf("closure = %v, want %v", got, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

// A pointer into a component schema needs that schema's root attached, and
// is not an unresolved reference.
func TestSchemaClosure_AttachesTheRootOfAPointerIntoASchema(t *testing.T) {
	entry := docEntry("/pets", `{"response_definition":{"200":{"example_schema":`+
		`{"$ref":"#/components/schemas/Pet/$defs/Tag"}}}}`)

	closure, unresolved := SchemaClosure(entry, petSchemas())

	if got, want := slices.Sorted(maps.Keys(closure)), []string{"Address", "Owner", "Pet"}; !slices.Equal(got, want) {
		t.Errorf("closure = %v, want %v", got, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}

func TestSchemaClosure_PointerIntoASchemaVariants(t *testing.T) {
	tests := []struct {
		name           string
		ref            string
		wantClosure    []string
		wantUnresolved []string
	}{
		{"property pointer", "#/components/schemas/Pet/properties/id", []string{"Address", "Owner", "Pet"}, nil},
		{"escaped name", "#/components/schemas/Pet~1Cat/properties/id", nil, []string{"Pet/Cat"}},
		{"missing root", "#/components/schemas/Ghost/$defs/Tag", nil, []string{"Ghost"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := docEntry("/pets", `{"response_definition":{"200":{"example_schema":{"$ref":"`+tt.ref+`"}}}}`)

			closure, unresolved := SchemaClosure(entry, petSchemas())

			if got := slices.Sorted(maps.Keys(closure)); !slices.Equal(got, tt.wantClosure) {
				t.Errorf("closure = %v, want %v", got, tt.wantClosure)
			}
			if !slices.Equal(unresolved, tt.wantUnresolved) {
				t.Errorf("unresolved = %v, want %v", unresolved, tt.wantUnresolved)
			}
		})
	}
}

// A discriminator mapping names the schemas a response may be, so they belong
// to the closure of a schema that declares one: Dog inherits from Pet, which
// maps back to it.
func TestSchemaClosure_FollowsDiscriminatorMappings(t *testing.T) {
	components := map[string]runtime.RawExtension{
		"Pet": {Raw: []byte(`{"type":"object","discriminator":{"propertyName":"kind",` +
			`"mapping":{"dog":"#/components/schemas/Dog"}}}`)},
		"Dog": {Raw: []byte(`{"allOf":[{"$ref":"#/components/schemas/Pet"},{"type":"object"}]}`)},
	}
	entry := docEntry("/pets", `{"response_definition":{"200":{"ref":"Pet"}}}`)

	closure, unresolved := SchemaClosure(entry, components)

	if got, want := slices.Sorted(maps.Keys(closure)), []string{"Dog", "Pet"}; !slices.Equal(got, want) {
		t.Errorf("closure = %v, want %v", got, want)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %v, want none", unresolved)
	}
}
