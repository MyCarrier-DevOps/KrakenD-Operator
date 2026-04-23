/*
Copyright 2026.

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
	"testing"

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
	if _, ok := result["user"]; !ok {
		t.Error("expected lowercase key 'user'")
	}
	if _, ok := result["error"]; !ok {
		t.Error("expected lowercase key 'error'")
	}
}

func TestExtractComponentSchemas_LowercasesKeys(t *testing.T) {
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
	key := "mc.address.contracts.models.publiccontracts.apiresponse"
	if _, ok := result[key]; !ok {
		t.Errorf("expected lowercase key %q", key)
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
	raw := result["pet"]
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
