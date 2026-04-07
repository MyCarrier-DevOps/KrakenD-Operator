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
	"context"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCUEEvaluator_BasicEvaluation(t *testing.T) {
	eval := NewCUEEvaluator()

	// Minimal CUE definition that outputs a single endpoint
	defs := map[string]string{
		"main.cue": `
import "strings"

_spec: _
_env: string | *"dev"

endpoint: {
	for path, methods in _spec.paths {
		for method, op in methods {
			"\(path):\(strings.ToUpper(method))": {
				"endpoint": path
				"method": strings.ToUpper(method)
				"backends": [{
					"host": ["http://localhost"]
					"url_pattern": path
				}]
				_operationId: op.operationId
				_tags: op.tags
			}
		}
	}
}
`,
	}

	specJSON := []byte(`{
		"paths": {
			"/api/users": {
				"get": {
					"operationId": "listUsers",
					"tags": ["users", "public"]
				}
			}
		}
	}`)

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
	if out.Entries[0].Endpoint != "/api/users" {
		t.Errorf("expected /api/users, got %s", out.Entries[0].Endpoint)
	}
	if out.Entries[0].Method != "GET" {
		t.Errorf("expected GET, got %s", out.Entries[0].Method)
	}
	if opID, ok := out.OperationIDs["/api/users:GET"]; !ok || opID != "listUsers" {
		t.Errorf("expected operationId listUsers, got %v", out.OperationIDs)
	}
	if tags, ok := out.Tags["/api/users:GET"]; !ok || len(tags) != 2 {
		t.Errorf("expected 2 tags, got %v", tags)
	}
}

func TestCUEEvaluator_YAMLInput(t *testing.T) {
	eval := NewCUEEvaluator()

	defs := map[string]string{
		"main.cue": `
import "strings"

_spec: _
endpoint: {
	for path, methods in _spec.paths {
		for method, op in methods {
			"\(path):\(strings.ToUpper(method))": {
				"endpoint": path
				"method": strings.ToUpper(method)
				"backends": [{
					"host": ["http://svc"]
					"url_pattern": path
				}]
			}
		}
	}
}
`,
	}

	specYAML := []byte(`
paths:
  /api/items:
    get:
      operationId: listItems
`)

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specYAML,
		SpecFormat:  v1alpha1.SpecFormatYAML,
		DefaultDefs: defs,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
}

func TestCUEEvaluator_AutoDetectFormat(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"main.cue": `
_spec: _
endpoint: {
	"/test:GET": {
		"endpoint": "/test"
		"method": "GET"
		"backends": [{
			"host": ["http://svc"]
			"url_pattern": "/test"
		}]
	}
}
`,
	}

	specJSON := []byte(`{"paths": {}}`)
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(out.Entries))
	}
}

func TestCUEEvaluator_EnvironmentInjection(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"main.cue": `
_spec: _
_env: string

_host: {
	dev:  "http://dev-svc"
	prod: "http://prod-svc"
}

endpoint: {
	"/api:GET": {
		"endpoint": "/api"
		"method": "GET"
		"backends": [{
			"host": [_host[_env]]
			"url_pattern": "/api"
		}]
	}
}
`,
	}

	specJSON := []byte(`{"paths": {}}`)
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Environment: "prod",
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
	if out.Entries[0].Backends[0].Host[0] != "http://prod-svc" {
		t.Errorf("expected prod host, got %s", out.Entries[0].Backends[0].Host[0])
	}
}

func TestCUEEvaluator_CustomDefs(t *testing.T) {
	eval := NewCUEEvaluator()
	defaultDefs := map[string]string{
		"main.cue": `
_spec: _
_timeout: string | *"3s"
endpoint: {
	"/api:GET": {
		"endpoint": "/api"
		"method": "GET"
		"backends": [{
			"host": ["http://svc"]
			"url_pattern": "/api"
		}]
	}
}
`,
	}
	customDefs := map[string]string{
		"custom.cue": `
_timeout: "10s"
`,
	}

	specJSON := []byte(`{"paths": {}}`)
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defaultDefs,
		CustomDefs:  customDefs,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(out.Entries))
	}
}

func TestCUEEvaluator_InvalidSpec(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"main.cue": `_spec: _
endpoint: {}`,
	}

	// Use spec data that is neither valid JSON nor valid YAML-convertible.
	_, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    []byte("\x00\x01\x02"),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
	})
	if err == nil {
		t.Error("expected error for invalid spec")
	}
}

func TestCUEEvaluator_NoDefs(t *testing.T) {
	eval := NewCUEEvaluator()
	_, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(`{}`),
		DefaultDefs: map[string]string{},
		ServiceName: "_spec",
	})
	if err == nil {
		t.Error("expected error for no definitions")
	}
}

func TestNormalizeToJSON_JSON(t *testing.T) {
	input := []byte(`{"key": "value"}`)
	out, err := normalizeToJSON(input, v1alpha1.SpecFormatJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(input) {
		t.Errorf("expected passthrough, got %s", string(out))
	}
}

func TestNormalizeToJSON_YAML(t *testing.T) {
	input := []byte("key: value\n")
	out, err := normalizeToJSON(input, v1alpha1.SpecFormatYAML)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `{"key":"value"}` {
		t.Errorf("expected JSON, got %s", string(out))
	}
}

func TestNormalizeToJSON_AutoDetect(t *testing.T) {
	jsonInput := []byte(`{"key": "value"}`)
	out, err := normalizeToJSON(jsonInput, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(jsonInput) {
		t.Errorf("auto-detect should return JSON as-is")
	}

	yamlInput := []byte("key: value\n")
	out, err = normalizeToJSON(yamlInput, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `{"key":"value"}` {
		t.Errorf("auto-detect should convert YAML, got %s", string(out))
	}
}

func TestCUEEvaluator_Overrides(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"main.cue": `
import "strings"

_spec: _
_overrides: [string]: _

endpoint: {
	for path, methods in _spec.paths {
		for method, op in methods {
			"\(path):\(strings.ToUpper(method))": {
				"endpoint": path
				"method": strings.ToUpper(method)
				"backends": [{
					"host": ["http://svc"]
					"url_pattern": path
				}]
				// sanitizeName lowercases operationId
				if _overrides[strings.ToLower(op.operationId)] != _|_ {
					"extraConfig": _overrides[strings.ToLower(op.operationId)]
				}
				_operationId: op.operationId
			}
		}
	}
}
`,
	}

	specJSON := []byte(`{
		"paths": {
			"/api/users": {
				"get": {
					"operationId": "listUsers",
					"tags": ["users"]
				}
			}
		}
	}`)

	overrides := []v1alpha1.OperationOverride{
		{
			OperationID: "listUsers",
			ExtraConfig: &runtime.RawExtension{
				Raw: []byte(`{"auth/validator": {"alg": "RS256"}}`),
			},
		},
	}

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Overrides:   overrides,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
	// Verify the extra_config was applied
	if out.Entries[0].ExtraConfig == nil {
		t.Error("expected extra_config from override to be present")
	}
}

func TestCUEEvaluator_OverridesWithoutExtraConfig(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"main.cue": `
_spec: _
endpoint: {
	"/test:GET": {
		"endpoint": "/test"
		"method": "GET"
		"backends": [{
			"host": ["http://svc"]
			"url_pattern": "/test"
		}]
	}
}
`,
	}

	// Override without ExtraConfig should be a no-op
	overrides := []v1alpha1.OperationOverride{
		{OperationID: "someOp"},
	}

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(`{"paths": {}}`),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Overrides:   overrides,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(out.Entries))
	}
}

func TestCUEEvaluator_OverridesNilRaw(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"main.cue": `
_spec: _
endpoint: {
	"/test:GET": {
		"endpoint": "/test"
		"method": "GET"
		"backends": [{
			"host": ["http://svc"]
			"url_pattern": "/test"
		}]
	}
}
`,
	}

	// Override with ExtraConfig but nil Raw should be a no-op
	overrides := []v1alpha1.OperationOverride{
		{
			OperationID: "someOp",
			ExtraConfig: &runtime.RawExtension{Raw: nil},
		},
	}

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(`{"paths": {}}`),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Overrides:   overrides,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(out.Entries))
	}
}
