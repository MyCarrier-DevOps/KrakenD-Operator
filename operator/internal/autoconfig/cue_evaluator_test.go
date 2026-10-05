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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// overrideLookupDefs is a custom definition that reads _overrides by the
// operationId.
const overrideLookupDefs = `
import "regexp"
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
					// The entry-level extraConfig is also merged from the
					// override by the evaluator, so the backend carries the
					// observable result of the lookup.
					if _overrides[_key] != _|_ {
						"extraConfig": _overrides[_key]
					}
				}]
				// Override keys are the SanitizeName form of the operationId.
				_key: strings.Trim(regexp.ReplaceAll("[^a-z0-9-]", strings.ToLower(op.operationId), "-"), "-")
				if _overrides[_key] != _|_ {
					"extraConfig": _overrides[_key]
				}
				_operationId: op.operationId
			}
		}
	}
}
`

func TestCUEEvaluator_Overrides(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{"main.cue": overrideLookupDefs}

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
	// The evaluator merges the override into the entry itself, so the entry's
	// extraConfig proves nothing about the CUE lookup. The definition copies
	// the looked-up value onto the backend: that is the case-fold pin.
	if len(out.Entries[0].Backends) == 0 {
		t.Fatal("expected a backend")
	}
	be := out.Entries[0].Backends[0]
	if be.ExtraConfig == nil || !strings.Contains(string(be.ExtraConfig.Raw), `"auth/validator"`) {
		t.Errorf("expected the lookup of listUsers to find the override, got backend extraConfig %v", be.ExtraConfig)
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

func TestCUEEvaluator_URLTransform_HostMapping(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"test.cue": `
_spec: _
endpoint: {
	"/api/users:GET": {
		endpoint: "/api/users"
		method: "GET"
		backends: [{host: ["https://api.example.com"], urlPattern: "/api/users", method: "GET"}]
	}
	"/api/orders:POST": {
		endpoint: "/api/orders"
		method: "POST"
		backends: [
			{host: ["https://api.example.com"], urlPattern: "/api/orders", method: "POST"},
			{host: ["https://payments.example.com"], urlPattern: "/pay", method: "POST"},
		]
	}
}`,
	}

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(`{"paths": {}}`),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
		URLTransform: &v1alpha1.URLTransformSpec{
			HostMapping: []v1alpha1.HostMappingEntry{
				{From: "https://api.example.com", To: "http://user-service.default.svc.cluster.local:8080"},
				{From: "https://payments.example.com", To: "http://payment-service.default.svc.cluster.local:8080"},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out.Entries))
	}

	byKey := map[string]v1alpha1.EndpointEntry{}
	for _, e := range out.Entries {
		byKey[e.Endpoint+":"+e.Method] = e
	}

	users := byKey["/api/users:GET"]
	if users.Backends[0].Host[0] != "http://user-service.default.svc.cluster.local:8080" {
		t.Errorf("expected mapped host for users, got %s", users.Backends[0].Host[0])
	}

	orders := byKey["/api/orders:POST"]
	if orders.Backends[0].Host[0] != "http://user-service.default.svc.cluster.local:8080" {
		t.Errorf("expected mapped host for orders backend 0, got %s", orders.Backends[0].Host[0])
	}
	if orders.Backends[1].Host[0] != "http://payment-service.default.svc.cluster.local:8080" {
		t.Errorf("expected mapped host for orders backend 1, got %s", orders.Backends[1].Host[0])
	}
}

func TestCUEEvaluator_URLTransform_StripAndAddPrefix(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"test.cue": `
_spec: _
endpoint: {
	"/api/v1/users:GET": {
		endpoint: "/api/v1/users"
		method: "GET"
		backends: [{host: ["http://localhost"], urlPattern: "/api/v1/users", method: "GET"}]
		_operationId: "listUsers"
		_tags: ["users"]
	}
}`,
	}

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(`{"paths": {}}`),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
		URLTransform: &v1alpha1.URLTransformSpec{
			StripPathPrefix: "/api/v1",
			AddPathPrefix:   "/gateway",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}

	entry := out.Entries[0]
	if entry.Endpoint != "/gateway/users" {
		t.Errorf("expected endpoint /gateway/users, got %s", entry.Endpoint)
	}

	// Verify OperationIDs map key was updated
	if opID, ok := out.OperationIDs["/gateway/users:GET"]; !ok || opID != "listUsers" {
		t.Errorf("expected operationID under new key, got %v", out.OperationIDs)
	}
	if _, ok := out.OperationIDs["/api/v1/users:GET"]; ok {
		t.Error("old operationID key should have been removed")
	}

	// Verify Tags map key was updated
	if tags, ok := out.Tags["/gateway/users:GET"]; !ok || len(tags) != 1 || tags[0] != "users" {
		t.Errorf("expected tags under new key, got %v", out.Tags)
	}
}

func TestCUEEvaluator_URLTransform_NoMatchingHost(t *testing.T) {
	eval := NewCUEEvaluator()
	defs := map[string]string{
		"test.cue": `
_spec: _
endpoint: {
	"/test:GET": {
		endpoint: "/test"
		method: "GET"
		backends: [{host: ["http://unmatched.example.com"], urlPattern: "/test", method: "GET"}]
	}
}`,
	}

	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(`{"paths": {}}`),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
		URLTransform: &v1alpha1.URLTransformSpec{
			HostMapping: []v1alpha1.HostMappingEntry{
				{From: "https://other.example.com", To: "http://other.svc.cluster.local"},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Host should remain unchanged when no mapping matches
	if out.Entries[0].Backends[0].Host[0] != "http://unmatched.example.com" {
		t.Errorf("expected unchanged host, got %s", out.Entries[0].Backends[0].Host[0])
	}
}

func TestMergeExtraConfig_ShallowMerge(t *testing.T) {
	existing := &runtime.RawExtension{
		Raw: []byte(
			`{"qos/ratelimit/router":{"every":"2s","max_rate":10},"documentation/openapi":{"operation_id":"foo"}}`,
		),
	}
	override := &runtime.RawExtension{
		Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":20}}`),
	}

	result := mergeExtraConfig(existing, override)

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(result.Raw, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	// Override key should be replaced
	var rateLimit map[string]interface{}
	if err := json.Unmarshal(merged["qos/ratelimit/router"], &rateLimit); err != nil {
		t.Fatalf("unmarshal rate limit: %v", err)
	}
	if rateLimit["every"] != "1s" {
		t.Errorf("expected every=1s, got %v", rateLimit["every"])
	}
	if rateLimit["max_rate"] != float64(20) {
		t.Errorf("expected max_rate=20, got %v", rateLimit["max_rate"])
	}

	// Non-override key should be preserved
	if _, ok := merged["documentation/openapi"]; !ok {
		t.Error("documentation/openapi should be preserved")
	}
}

func TestMergeExtraConfig_NilExisting(t *testing.T) {
	override := &runtime.RawExtension{
		Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`),
	}
	result := mergeExtraConfig(nil, override)
	if string(result.Raw) != string(override.Raw) {
		t.Errorf("expected override to be returned as-is, got %s", result.Raw)
	}
}

func TestMergeExtraConfig_NilOverride(t *testing.T) {
	existing := &runtime.RawExtension{
		Raw: []byte(`{"qos/ratelimit/router":{"every":"2s"}}`),
	}
	result := mergeExtraConfig(existing, nil)
	if string(result.Raw) != string(existing.Raw) {
		t.Errorf("expected existing to be returned as-is, got %s", result.Raw)
	}
}

// --- applyFieldOverrides comprehensive tests ---

// testOutputWithEntries creates a CUEOutput with entries and operationID mappings.
func testOutputWithEntries() *CUEOutput {
	return &CUEOutput{
		Entries: []v1alpha1.EndpointEntry{
			{
				Endpoint: "/api/users",
				Method:   "GET",
				Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc"}, URLPattern: "/api/users", Method: "GET"},
				},
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"2s","max_rate":10}}`),
				},
			},
			{
				Endpoint: "/api/orders",
				Method:   "POST",
				Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://orders"}, URLPattern: "/api/orders", Method: "POST"},
					{Host: []string{"http://audit"}, URLPattern: "/audit", Method: "POST"},
				},
			},
		},
		OperationIDs: map[string]string{
			"/api/users:GET":   "listUsers",
			"/api/orders:POST": "createOrder",
		},
		Tags: map[string][]string{
			"/api/users:GET":   {"users"},
			"/api/orders:POST": {"orders"},
		},
	}
}

func TestApplyFieldOverrides_Timeout(t *testing.T) {
	out := testOutputWithEntries()
	timeout := metav1.Duration{Duration: 30 * time.Second}
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "listUsers", Timeout: &timeout},
	})

	if out.Entries[0].Timeout == nil || out.Entries[0].Timeout.Duration != 30*time.Second {
		t.Errorf("expected timeout 30s, got %v", out.Entries[0].Timeout)
	}
}

func TestApplyFieldOverrides_CacheTTL(t *testing.T) {
	out := testOutputWithEntries()
	ttl := metav1.Duration{Duration: 5 * time.Minute}
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "listUsers", CacheTTL: &ttl},
	})

	if out.Entries[0].CacheTTL == nil || out.Entries[0].CacheTTL.Duration != 5*time.Minute {
		t.Errorf("expected cacheTTL 5m, got %v", out.Entries[0].CacheTTL)
	}
}

func TestApplyFieldOverrides_Endpoint(t *testing.T) {
	out := testOutputWithEntries()
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "listUsers", Endpoint: "/api/v2/users"},
	})

	if out.Entries[0].Endpoint != "/api/v2/users" {
		t.Errorf("expected endpoint /api/v2/users, got %s", out.Entries[0].Endpoint)
	}
	// OperationIDs map should be updated
	if out.OperationIDs["/api/v2/users:GET"] != "listUsers" {
		t.Errorf("expected OperationIDs to be remapped, got %v", out.OperationIDs)
	}
	if _, exists := out.OperationIDs["/api/users:GET"]; exists {
		t.Error("old key should be removed from OperationIDs")
	}
	// Tags map should be updated
	if tags := out.Tags["/api/v2/users:GET"]; len(tags) == 0 || tags[0] != "users" {
		t.Errorf("expected Tags to be remapped, got %v", out.Tags)
	}
	if _, exists := out.Tags["/api/users:GET"]; exists {
		t.Error("old key should be removed from Tags")
	}
}

func TestApplyFieldOverrides_Method(t *testing.T) {
	out := testOutputWithEntries()
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "listUsers", Method: "PATCH"},
	})

	if out.Entries[0].Method != "PATCH" {
		t.Errorf("expected method PATCH, got %s", out.Entries[0].Method)
	}
	if out.OperationIDs["/api/users:PATCH"] != "listUsers" {
		t.Errorf("expected OperationIDs to be remapped for method, got %v", out.OperationIDs)
	}
}

func TestApplyFieldOverrides_PolicyRef(t *testing.T) {
	out := testOutputWithEntries()
	policyRef := &v1alpha1.PolicyRef{Name: "rate-limit-policy", Namespace: "infra"}
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "createOrder", PolicyRef: policyRef},
	})

	for i, be := range out.Entries[1].Backends {
		if be.PolicyRef == nil {
			t.Errorf("backend[%d] should have PolicyRef", i)
			continue
		}
		if be.PolicyRef.Name != "rate-limit-policy" || be.PolicyRef.Namespace != "infra" {
			t.Errorf("backend[%d] PolicyRef = %+v, want rate-limit-policy/infra", i, be.PolicyRef)
		}
	}
}

func TestApplyFieldOverrides_BackendExtraConfig(t *testing.T) {
	out := testOutputWithEntries()
	backendEC := &runtime.RawExtension{
		Raw: []byte(`{"backend/http":{"return_error_code":true}}`),
	}
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{
			OperationID: "createOrder",
			Backends: []v1alpha1.BackendOverride{
				{Index: 0, ExtraConfig: backendEC},
			},
		},
	})

	if out.Entries[1].Backends[0].ExtraConfig == nil {
		t.Fatal("backend[0] should have ExtraConfig")
	}
	if string(out.Entries[1].Backends[0].ExtraConfig.Raw) != string(backendEC.Raw) {
		t.Errorf("backend[0] ExtraConfig = %s, want %s", out.Entries[1].Backends[0].ExtraConfig.Raw, backendEC.Raw)
	}
	// backend[1] should not be affected
	if out.Entries[1].Backends[1].ExtraConfig != nil {
		t.Error("backend[1] should not have ExtraConfig")
	}
}

func TestApplyFieldOverrides_BackendIndexOutOfBounds(t *testing.T) {
	out := testOutputWithEntries()
	backendEC := &runtime.RawExtension{
		Raw: []byte(`{"backend/http":{"return_error_code":true}}`),
	}
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{
			OperationID: "listUsers",
			Backends: []v1alpha1.BackendOverride{
				{Index: 99, ExtraConfig: backendEC},
			},
		},
	})

	// Should not panic; backend[0] should remain unmodified
	if out.Entries[0].Backends[0].ExtraConfig != nil {
		t.Error("backend[0] ExtraConfig should remain nil (out-of-bounds index should be skipped)")
	}
}

func TestApplyFieldOverrides_NonExistentOperationID(t *testing.T) {
	out := testOutputWithEntries()
	timeout := metav1.Duration{Duration: 30 * time.Second}
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "nonExistent", Timeout: &timeout},
	})

	// Nothing should change
	if out.Entries[0].Timeout != nil {
		t.Error("timeout should remain nil for unmatched operationID")
	}
	if len(out.UnmatchedOverrides) != 1 || out.UnmatchedOverrides[0] != "nonExistent" {
		t.Errorf("expected UnmatchedOverrides [nonExistent], got %v", out.UnmatchedOverrides)
	}
}

func TestApplyFieldOverrides_MatchedOverrideNotReported(t *testing.T) {
	out := testOutputWithEntries()
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "listUsers"},
	})

	if len(out.UnmatchedOverrides) != 0 {
		t.Errorf("expected no UnmatchedOverrides for a matched operationID, got %v", out.UnmatchedOverrides)
	}
}

func TestApplyFieldOverrides_ReportsOnlyUnmatchedInOrder(t *testing.T) {
	out := testOutputWithEntries()
	timeout := metav1.Duration{Duration: 30 * time.Second}
	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{OperationID: "listUsers", Timeout: &timeout},
		{OperationID: "ghostA"},
		{OperationID: "ghostB"},
	})

	if len(out.UnmatchedOverrides) != 2 || out.UnmatchedOverrides[0] != "ghostA" || out.UnmatchedOverrides[1] != "ghostB" {
		t.Errorf("expected UnmatchedOverrides [ghostA ghostB], got %v", out.UnmatchedOverrides)
	}
	if out.Entries[0].Timeout == nil || out.Entries[0].Timeout.Duration != 30*time.Second {
		t.Errorf("expected listUsers timeout 30s applied, got %v", out.Entries[0].Timeout)
	}
}

func TestApplyFieldOverrides_CombinedOverrides(t *testing.T) {
	out := testOutputWithEntries()
	timeout := metav1.Duration{Duration: 60 * time.Second}
	cacheTTL := metav1.Duration{Duration: 10 * time.Minute}
	policyRef := &v1alpha1.PolicyRef{Name: "my-policy"}
	extraConfig := &runtime.RawExtension{
		Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`),
	}

	applyFieldOverrides(out, []v1alpha1.OperationOverride{
		{
			OperationID: "listUsers",
			Endpoint:    "/api/v3/users",
			Method:      "PUT",
			Timeout:     &timeout,
			CacheTTL:    &cacheTTL,
			PolicyRef:   policyRef,
			ExtraConfig: extraConfig,
		},
	})

	entry := &out.Entries[0]
	if entry.Endpoint != "/api/v3/users" {
		t.Errorf("endpoint = %s, want /api/v3/users", entry.Endpoint)
	}
	if entry.Method != "PUT" {
		t.Errorf("method = %s, want PUT", entry.Method)
	}
	if entry.Timeout == nil || entry.Timeout.Duration != 60*time.Second {
		t.Errorf("timeout = %v, want 60s", entry.Timeout)
	}
	if entry.CacheTTL == nil || entry.CacheTTL.Duration != 10*time.Minute {
		t.Errorf("cacheTTL = %v, want 10m", entry.CacheTTL)
	}
	if entry.Backends[0].PolicyRef == nil || entry.Backends[0].PolicyRef.Name != "my-policy" {
		t.Errorf("PolicyRef = %v, want my-policy", entry.Backends[0].PolicyRef)
	}
	// ExtraConfig should be merged (auth key added, qos preserved)
	var ec map[string]json.RawMessage
	if err := json.Unmarshal(entry.ExtraConfig.Raw, &ec); err != nil {
		t.Fatal(err)
	}
	if _, ok := ec["auth/validator"]; !ok {
		t.Error("auth/validator should be added from override")
	}
	if _, ok := ec["qos/ratelimit/router"]; !ok {
		t.Error("qos/ratelimit/router should be preserved from original")
	}
	// Keys should be remapped
	if out.OperationIDs["/api/v3/users:PUT"] != "listUsers" {
		t.Errorf("OperationIDs not remapped: %v", out.OperationIDs)
	}
}

func TestApplyFieldOverrides_EmptySlice(t *testing.T) {
	out := testOutputWithEntries()
	originalEndpoint := out.Entries[0].Endpoint
	applyFieldOverrides(out, []v1alpha1.OperationOverride{})

	if out.Entries[0].Endpoint != originalEndpoint {
		t.Error("empty overrides should not modify entries")
	}
}

func TestApplyFieldOverrides_ExtraConfigMergeWithEmbeddedCUE(t *testing.T) {
	// End-to-end: verify that embedded CUE output with extraConfig gets
	// properly merged (not replaced) by an override.
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	specJSON := []byte(`{
		"paths": {
			"/api/items": {
				"get": {
					"operationId": "listItems",
					"tags": ["items"],
					"parameters": [{"name": "page", "in": "query"}]
				}
			}
		}
	}`)

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "listItems",
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":50}}`),
				},
			},
		},
		ServiceName: "_spec",
		DefaultHost: "http://items.svc:8080",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}

	var ec map[string]json.RawMessage
	if err := json.Unmarshal(out.Entries[0].ExtraConfig.Raw, &ec); err != nil {
		t.Fatalf("unmarshal extraConfig: %v", err)
	}

	// Rate limit override should be applied
	var rl map[string]interface{}
	if err := json.Unmarshal(ec["qos/ratelimit/router"], &rl); err != nil {
		t.Fatalf("unmarshal rate limit: %v", err)
	}
	if rl["every"] != "1s" {
		t.Errorf("expected every=1s from override, got %v", rl["every"])
	}
	if rl["max_rate"] != float64(50) {
		t.Errorf("expected max_rate=50 from override, got %v", rl["max_rate"])
	}

	// Documentation should be preserved from CUE
	if _, ok := ec["documentation/openapi"]; !ok {
		t.Error("documentation/openapi should be preserved from CUE evaluation")
	}
}

func TestEvaluate_OverrideOnOperationWithoutOperationIdIsReported(t *testing.T) {
	// Regression: an operation with no operationId in the OpenAPI spec has no
	// _operationId in the CUE output, so an override targeting it can never
	// match. It must be reported as unmatched rather than silently dropped.
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	specJSON := []byte(`{
		"paths": {
			"/api/v1/webhook-receiver/status": {
				"post": {
					"responses": {"202": {"description": "Accepted"}}
				}
			}
		}
	}`)

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "WebhookStatus",
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"documentation/openapi":{"audience":["internal"]}}`),
				},
			},
		},
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}

	var ec map[string]json.RawMessage
	if err := json.Unmarshal(out.Entries[0].ExtraConfig.Raw, &ec); err != nil {
		t.Fatalf("unmarshal extraConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(ec["documentation/openapi"], &doc); err != nil {
		t.Fatalf("unmarshal documentation/openapi: %v", err)
	}
	audience, ok := doc["audience"].([]any)
	if !ok || len(audience) != 1 || audience[0] != "public" {
		t.Errorf("expected audience [public] (override not applied), got %v", doc["audience"])
	}

	if len(out.UnmatchedOverrides) != 1 || out.UnmatchedOverrides[0] != "WebhookStatus" {
		t.Errorf("expected UnmatchedOverrides [WebhookStatus], got %v", out.UnmatchedOverrides)
	}
}

func TestEvaluate_OperationAudienceMustBeListOfStrings(t *testing.T) {
	// A non-list audience on an operation must fail that operation rather
	// than reach the gateway, where it fails `krakend check -t -n -c` and
	// blocks config updates for the whole gateway.
	for name, audience := range map[string]string{
		"map instead of list":   `{"internal": null}`,
		"null instead of list":  `null`,
		"list with a null item": `["internal", null]`,
	} {
		t.Run(name, func(t *testing.T) {
			out := evaluateEmbedded(t, `{"paths":{"/api/v1/users":{"get":{"operationId":"listUsers",`+
				`"audience":`+audience+`,"responses":{"200":{"description":"OK"}}}}}}`)
			if len(out.Entries) != 0 {
				t.Errorf("expected no entry for a non-list audience, got %+v", out.Entries)
			}
			if len(out.Failed) != 1 || !strings.Contains(out.Failed[0].Message, "audience") {
				t.Errorf("expected listUsers failed naming the audience field, got %+v", out.Failed)
			}
		})
	}
}

func TestEvaluate_OperationAudienceListOfStringsPassesThrough(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	specJSON := []byte(`{
		"paths": {
			"/api/v1/users": {
				"get": {
					"operationId": "listUsers",
					"audience": ["internal"],
					"responses": {"200": {"description": "OK"}}
				}
			}
		}
	}`)

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}

	var ec map[string]json.RawMessage
	if err := json.Unmarshal(out.Entries[0].ExtraConfig.Raw, &ec); err != nil {
		t.Fatalf("unmarshal extraConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(ec["documentation/openapi"], &doc); err != nil {
		t.Fatalf("unmarshal documentation/openapi: %v", err)
	}
	audience, ok := doc["audience"].([]any)
	if !ok || len(audience) != 1 || audience[0] != "internal" {
		t.Errorf("expected audience [internal], got %v", doc["audience"])
	}
}

func TestEvaluate_OperationNoAudienceDefaultsToPublic(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	specJSON := []byte(`{
		"paths": {
			"/api/v1/users": {
				"get": {
					"operationId": "listUsers",
					"responses": {"200": {"description": "OK"}}
				}
			}
		}
	}`)

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}

	var ec map[string]json.RawMessage
	if err := json.Unmarshal(out.Entries[0].ExtraConfig.Raw, &ec); err != nil {
		t.Fatalf("unmarshal extraConfig: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(ec["documentation/openapi"], &doc); err != nil {
		t.Fatalf("unmarshal documentation/openapi: %v", err)
	}
	audience, ok := doc["audience"].([]any)
	if !ok || len(audience) != 1 || audience[0] != "public" {
		t.Errorf("expected audience [public], got %v", doc["audience"])
	}
}

// --- applyDefaults tests ---

func TestApplyDefaults_Timeout(t *testing.T) {
	out := testOutputWithEntries()
	timeout := metav1.Duration{Duration: 20 * time.Second}
	applyDefaults(out, &v1alpha1.EndpointDefaults{Timeout: &timeout})

	for i, entry := range out.Entries {
		if entry.Timeout == nil || entry.Timeout.Duration != 20*time.Second {
			t.Errorf("entry[%d]: expected timeout 20s, got %v", i, entry.Timeout)
		}
	}
}

func TestApplyDefaults_InputHeaders(t *testing.T) {
	out := testOutputWithEntries()
	headers := []string{"Authorization", "X-Custom"}
	applyDefaults(out, &v1alpha1.EndpointDefaults{InputHeaders: headers})

	for i, entry := range out.Entries {
		if len(entry.InputHeaders) != 2 || entry.InputHeaders[0] != "Authorization" ||
			entry.InputHeaders[1] != "X-Custom" {
			t.Errorf("entry[%d]: expected [Authorization, X-Custom], got %v", i, entry.InputHeaders)
		}
	}
}

func TestApplyDefaults_InputQueryStrings(t *testing.T) {
	out := testOutputWithEntries()
	applyDefaults(out, &v1alpha1.EndpointDefaults{InputQueryStrings: []string{}})

	for i, entry := range out.Entries {
		if entry.InputQueryStrings == nil {
			t.Errorf("entry[%d]: expected empty slice, got nil", i)
		}
		if len(entry.InputQueryStrings) != 0 {
			t.Errorf("entry[%d]: expected empty query strings, got %v", i, entry.InputQueryStrings)
		}
	}
}

func TestApplyDefaultPolicyRef(t *testing.T) {
	out := testOutputWithEntries()
	policyRef := &v1alpha1.PolicyRef{Name: "default-policy"}
	applyDefaultPolicyRef(out, policyRef)

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.PolicyRef == nil || be.PolicyRef.Name != "default-policy" {
				t.Errorf("entry[%d].backend[%d]: expected default-policy, got %v", i, j, be.PolicyRef)
			}
		}
	}
}

func TestApplyDefaults_CacheTTL(t *testing.T) {
	out := testOutputWithEntries()
	ttl := metav1.Duration{Duration: 5 * time.Minute}
	applyDefaults(out, &v1alpha1.EndpointDefaults{CacheTTL: &ttl})

	for i, entry := range out.Entries {
		if entry.CacheTTL == nil || entry.CacheTTL.Duration != 5*time.Minute {
			t.Errorf("entry[%d]: expected cacheTTL 5m, got %v", i, entry.CacheTTL)
		}
	}
}

func TestApplyDefaults_OutputEncoding(t *testing.T) {
	out := testOutputWithEntries()
	applyDefaults(out, &v1alpha1.EndpointDefaults{OutputEncoding: "no-op"})

	for i, entry := range out.Entries {
		if entry.OutputEncoding != "no-op" {
			t.Errorf("entry[%d]: expected no-op, got %s", i, entry.OutputEncoding)
		}
	}
}

func TestApplyDefaults_ConcurrentCalls(t *testing.T) {
	out := testOutputWithEntries()
	cc := int32(3)
	applyDefaults(out, &v1alpha1.EndpointDefaults{ConcurrentCalls: &cc})

	for i, entry := range out.Entries {
		if entry.ConcurrentCalls == nil || *entry.ConcurrentCalls != 3 {
			t.Errorf("entry[%d]: expected concurrentCalls=3, got %v", i, entry.ConcurrentCalls)
		}
	}
}

func TestApplyDefaults_Nil(t *testing.T) {
	out := testOutputWithEntries()
	originalTimeout := out.Entries[0].Timeout
	applyDefaults(out, nil)

	if out.Entries[0].Timeout != originalTimeout {
		t.Error("nil defaults should not modify entries")
	}
}

func TestApplyBackendDefaults_ExtraConfig(t *testing.T) {
	out := testOutputWithEntries()
	defaults := &v1alpha1.BackendDefaults{
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"backend/http":{"return_error_code":true}}`),
		},
	}
	applyBackendDefaults(out, defaults)

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.ExtraConfig == nil {
				t.Fatalf("entry[%d].backend[%d]: expected extraConfig, got nil", i, j)
			}
			var ec map[string]json.RawMessage
			if err := json.Unmarshal(be.ExtraConfig.Raw, &ec); err != nil {
				t.Fatalf("entry[%d].backend[%d]: unmarshal: %v", i, j, err)
			}
			if _, ok := ec["backend/http"]; !ok {
				t.Errorf("entry[%d].backend[%d]: missing backend/http", i, j)
			}
		}
	}
}

func TestApplyBackendDefaults_Nil(t *testing.T) {
	out := testOutputWithEntries()
	original := out.Entries[0].Backends[0].ExtraConfig
	applyBackendDefaults(out, nil)

	if out.Entries[0].Backends[0].ExtraConfig != original {
		t.Error("nil backend defaults should not modify backends")
	}
}

func TestApplyDefaultPolicyRef_Nil(t *testing.T) {
	out := testOutputWithEntries()
	applyDefaultPolicyRef(out, nil)

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.PolicyRef != nil {
				t.Errorf("entry[%d].backend[%d]: expected nil PolicyRef, got %v", i, j, be.PolicyRef)
			}
		}
	}
}

func TestApplyDefaultPolicyRef_DoesNotOverrideExisting(t *testing.T) {
	out := testOutputWithEntries()
	out.Entries[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "existing"}
	applyDefaultPolicyRef(out, &v1alpha1.PolicyRef{Name: "default"})

	if out.Entries[0].Backends[0].PolicyRef.Name != "existing" {
		t.Errorf("expected existing PolicyRef preserved, got %s", out.Entries[0].Backends[0].PolicyRef.Name)
	}
	// Second entry's backend should get the default
	if out.Entries[1].Backends[0].PolicyRef == nil || out.Entries[1].Backends[0].PolicyRef.Name != "default" {
		t.Errorf("expected default PolicyRef on entry[1].backend[0], got %v", out.Entries[1].Backends[0].PolicyRef)
	}
}

// --- BackendDefaults scalar field tests ---

func TestApplyBackendDefaults_SD(t *testing.T) {
	out := testOutputWithEntries()
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{SD: "static"})

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.SD != "static" {
				t.Errorf("entry[%d].backend[%d]: expected sd=static, got %s", i, j, be.SD)
			}
		}
	}
}

func TestApplyBackendDefaults_SDDoesNotOverrideExisting(t *testing.T) {
	out := testOutputWithEntries()
	out.Entries[0].Backends[0].SD = "dns"
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{SD: "static"})

	if out.Entries[0].Backends[0].SD != "dns" {
		t.Errorf("expected existing sd=dns preserved, got %s", out.Entries[0].Backends[0].SD)
	}
}

func TestApplyBackendDefaults_Encoding(t *testing.T) {
	out := testOutputWithEntries()
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{Encoding: "safejson"})

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.Encoding != "safejson" {
				t.Errorf("entry[%d].backend[%d]: expected encoding=safejson, got %s", i, j, be.Encoding)
			}
		}
	}
}

func TestApplyBackendDefaults_EncodingDoesNotOverrideExisting(t *testing.T) {
	out := testOutputWithEntries()
	out.Entries[0].Backends[0].Encoding = "xml"
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{Encoding: "safejson"})

	if out.Entries[0].Backends[0].Encoding != "xml" {
		t.Errorf("expected existing encoding=xml preserved, got %s", out.Entries[0].Backends[0].Encoding)
	}
}

func TestApplyBackendDefaults_SDScheme(t *testing.T) {
	out := testOutputWithEntries()
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{SDScheme: "https"})

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.SDScheme != "https" {
				t.Errorf("entry[%d].backend[%d]: expected sdScheme=https, got %s", i, j, be.SDScheme)
			}
		}
	}
}

func TestApplyBackendDefaults_DisableHostSanitize(t *testing.T) {
	out := testOutputWithEntries()
	val := true
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{DisableHostSanitize: &val})

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.DisableHostSanitize == nil || *be.DisableHostSanitize != true {
				t.Errorf("entry[%d].backend[%d]: expected disableHostSanitize=true", i, j)
			}
		}
	}
}

func TestApplyBackendDefaults_DisableHostSanitizeDoesNotOverride(t *testing.T) {
	out := testOutputWithEntries()
	existing := false
	out.Entries[0].Backends[0].DisableHostSanitize = &existing
	val := true
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{DisableHostSanitize: &val})

	if *out.Entries[0].Backends[0].DisableHostSanitize != false {
		t.Error("expected existing disableHostSanitize=false preserved")
	}
}

func TestApplyBackendDefaults_InputHeaders(t *testing.T) {
	out := testOutputWithEntries()
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{InputHeaders: []string{"X-Forwarded-For"}})

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if len(be.InputHeaders) != 1 || be.InputHeaders[0] != "X-Forwarded-For" {
				t.Errorf("entry[%d].backend[%d]: expected [X-Forwarded-For], got %v", i, j, be.InputHeaders)
			}
		}
	}
}

func TestApplyBackendDefaults_InputQueryStrings(t *testing.T) {
	out := testOutputWithEntries()
	applyBackendDefaults(out, &v1alpha1.BackendDefaults{InputQueryStrings: []string{"page", "limit"}})

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if len(be.InputQueryStrings) != 2 {
				t.Errorf("entry[%d].backend[%d]: expected 2 query strings, got %v", i, j, be.InputQueryStrings)
			}
		}
	}
}

// --- Deep merge interaction tests ---
// These verify the 3-layer merge pipeline (CUE → defaults → overrides)
// produces the correct result when layers interact.

func TestDeepMergeJSON_BothObjects(t *testing.T) {
	base := json.RawMessage(`{"a":1,"b":{"x":10,"y":20}}`)
	patch := json.RawMessage(`{"b":{"y":99,"z":30},"c":3}`)
	result := deepMergeJSON(base, patch)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// "a" preserved from base
	if string(m["a"]) != "1" {
		t.Errorf("expected a=1 preserved, got %s", m["a"])
	}
	// "c" added from patch
	if string(m["c"]) != "3" {
		t.Errorf("expected c=3 from patch, got %s", m["c"])
	}
	// "b" recursively merged
	var b map[string]json.RawMessage
	if err := json.Unmarshal(m["b"], &b); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	if string(b["x"]) != "10" {
		t.Errorf("expected b.x=10 preserved, got %s", b["x"])
	}
	if string(b["y"]) != "99" {
		t.Errorf("expected b.y=99 from patch, got %s", b["y"])
	}
	if string(b["z"]) != "30" {
		t.Errorf("expected b.z=30 from patch, got %s", b["z"])
	}
}

func TestDeepMergeJSON_BaseNotObject(t *testing.T) {
	base := json.RawMessage(`"a string"`)
	patch := json.RawMessage(`{"key":"val"}`)
	result := deepMergeJSON(base, patch)
	if string(result) != `{"key":"val"}` {
		t.Errorf("expected patch to win when base is not object, got %s", result)
	}
}

func TestDeepMergeJSON_PatchNotObject(t *testing.T) {
	base := json.RawMessage(`{"key":"val"}`)
	patch := json.RawMessage(`42`)
	result := deepMergeJSON(base, patch)
	if string(result) != "42" {
		t.Errorf("expected patch to win when patch is not object, got %s", result)
	}
}

func TestDeepMergeJSON_ThreeLevelNesting(t *testing.T) {
	base := json.RawMessage(`{"l1":{"l2":{"l3_a":"keep","l3_b":"original"}}}`)
	patch := json.RawMessage(`{"l1":{"l2":{"l3_b":"replaced","l3_c":"new"}}}`)
	result := deepMergeJSON(base, patch)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var l1 map[string]json.RawMessage
	if err := json.Unmarshal(m["l1"], &l1); err != nil {
		t.Fatalf("unmarshal l1: %v", err)
	}
	var l2 map[string]json.RawMessage
	if err := json.Unmarshal(l1["l2"], &l2); err != nil {
		t.Fatalf("unmarshal l2: %v", err)
	}
	if string(l2["l3_a"]) != `"keep"` {
		t.Errorf("expected l3_a preserved, got %s", l2["l3_a"])
	}
	if string(l2["l3_b"]) != `"replaced"` {
		t.Errorf("expected l3_b replaced, got %s", l2["l3_b"])
	}
	if string(l2["l3_c"]) != `"new"` {
		t.Errorf("expected l3_c added, got %s", l2["l3_c"])
	}
}

func TestMergeExtraConfig_DeepMergePreservesNestedKeys(t *testing.T) {
	existing := &runtime.RawExtension{
		Raw: []byte(
			`{"backend/http":{"return_error_code":true,"return_error_msg":false},"qos/ratelimit/proxy":{"max_rate":100}}`,
		),
	}
	override := &runtime.RawExtension{
		Raw: []byte(`{"backend/http":{"return_error_msg":true}}`),
	}
	result := mergeExtraConfig(existing, override)

	var ec map[string]json.RawMessage
	if err := json.Unmarshal(result.Raw, &ec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// qos/ratelimit/proxy preserved (not in override)
	if _, ok := ec["qos/ratelimit/proxy"]; !ok {
		t.Error("expected qos/ratelimit/proxy preserved")
	}
	// backend/http deep-merged
	var http map[string]interface{}
	if err := json.Unmarshal(ec["backend/http"], &http); err != nil {
		t.Fatalf("unmarshal backend/http: %v", err)
	}
	if http["return_error_code"] != true {
		t.Error("expected return_error_code=true preserved")
	}
	if http["return_error_msg"] != true {
		t.Error("expected return_error_msg=true from override")
	}
}

func TestMergeExtraConfig_BothNil(t *testing.T) {
	result := mergeExtraConfig(nil, nil)
	if result != nil {
		t.Errorf("expected nil when both nil, got %v", result)
	}
}

func TestMergeExtraConfig_EmptyExistingRaw(t *testing.T) {
	existing := &runtime.RawExtension{Raw: []byte{}}
	override := &runtime.RawExtension{
		Raw: []byte(`{"key":"val"}`),
	}
	result := mergeExtraConfig(existing, override)
	if string(result.Raw) != `{"key":"val"}` {
		t.Errorf("expected override when existing empty, got %s", result.Raw)
	}
}

func TestApplyBackendDefaults_ExtraConfigDeepMergesWithExisting(t *testing.T) {
	out := testOutputWithEntries()
	// Give first backend an existing ExtraConfig
	out.Entries[0].Backends[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"qos/circuit-breaker":{"interval":60}}`),
	}
	defaults := &v1alpha1.BackendDefaults{
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"backend/http":{"return_error_code":true}}`),
		},
	}
	applyBackendDefaults(out, defaults)

	// First backend: should have BOTH circuit-breaker (existing) and backend/http (default)
	var ec map[string]json.RawMessage
	if err := json.Unmarshal(out.Entries[0].Backends[0].ExtraConfig.Raw, &ec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := ec["qos/circuit-breaker"]; !ok {
		t.Error("existing qos/circuit-breaker should be preserved after merge")
	}
	if _, ok := ec["backend/http"]; !ok {
		t.Error("default backend/http should be merged in")
	}
}

func TestApplyBackendDefaults_AllFieldsCombined(t *testing.T) {
	out := testOutputWithEntries()
	disableHS := true
	defaults := &v1alpha1.BackendDefaults{
		SD:                  "static",
		SDScheme:            "https",
		Encoding:            "safejson",
		DisableHostSanitize: &disableHS,
		InputHeaders:        []string{"X-Request-ID"},
		InputQueryStrings:   []string{"page"},
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"backend/http":{"return_error_code":true}}`),
		},
	}
	applyBackendDefaults(out, defaults)

	for i, entry := range out.Entries {
		for j, be := range entry.Backends {
			if be.SD != "static" {
				t.Errorf("entry[%d].backend[%d]: sd=%s, want static", i, j, be.SD)
			}
			if be.SDScheme != "https" {
				t.Errorf("entry[%d].backend[%d]: sdScheme=%s, want https", i, j, be.SDScheme)
			}
			if be.Encoding != "safejson" {
				t.Errorf("entry[%d].backend[%d]: encoding=%s, want safejson", i, j, be.Encoding)
			}
			if be.DisableHostSanitize == nil || *be.DisableHostSanitize != true {
				t.Errorf("entry[%d].backend[%d]: disableHostSanitize should be true", i, j)
			}
			if len(be.InputHeaders) != 1 || be.InputHeaders[0] != "X-Request-ID" {
				t.Errorf("entry[%d].backend[%d]: inputHeaders=%v, want [X-Request-ID]", i, j, be.InputHeaders)
			}
			if len(be.InputQueryStrings) != 1 || be.InputQueryStrings[0] != "page" {
				t.Errorf("entry[%d].backend[%d]: inputQueryStrings=%v, want [page]", i, j, be.InputQueryStrings)
			}
			if be.ExtraConfig == nil {
				t.Errorf("entry[%d].backend[%d]: extraConfig should not be nil", i, j)
			}
		}
	}
}

func TestApplyDefaults_OverriddenByFieldOverrides(t *testing.T) {
	// Verify ordering: defaults are applied first, then per-operation overrides win.
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	specJSON := []byte(`{
		"paths": {
			"/api/items": {
				"get": {
					"operationId": "listItems",
					"tags": ["items"]
				}
			},
			"/api/items/{id}": {
				"parameters": [{"name": "id", "in": "path", "required": true}],
				"delete": {
					"operationId": "deleteItem",
					"tags": ["items"]
				}
			}
		}
	}`)

	defaultTimeout := metav1.Duration{Duration: 20 * time.Second}
	overrideTimeout := metav1.Duration{Duration: 60 * time.Second}

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Defaults: &v1alpha1.Defaults{Endpoint: &v1alpha1.EndpointDefaults{
			Timeout:      &defaultTimeout,
			InputHeaders: []string{"Authorization", "Content-Type"},
		}},
		Overrides: []v1alpha1.OperationOverride{
			{OperationID: "deleteItem", Timeout: &overrideTimeout},
		},
		ServiceName: "_spec",
		DefaultHost: "http://items.svc:8080",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out.Entries))
	}

	for _, entry := range out.Entries {
		opID := out.OperationIDs[entry.Endpoint+":"+entry.Method]
		switch opID {
		case "listItems":
			// Should have default timeout (20s)
			if entry.Timeout == nil || entry.Timeout.Duration != 20*time.Second {
				t.Errorf("listItems: expected timeout 20s from defaults, got %v", entry.Timeout)
			}
		case "deleteItem":
			// Should have override timeout (60s), not default (20s)
			if entry.Timeout == nil || entry.Timeout.Duration != 60*time.Second {
				t.Errorf("deleteItem: expected timeout 60s from override, got %v", entry.Timeout)
			}
		}
		// Both should have the default inputHeaders
		if len(entry.InputHeaders) != 2 || entry.InputHeaders[0] != "Authorization" {
			t.Errorf("%s: expected [Authorization, Content-Type], got %v", opID, entry.InputHeaders)
		}
	}
}

func TestApplyBackendDefaultsToBackend_FillsOnlyUnset(t *testing.T) {
	b := v1alpha1.BackendSpec{Encoding: "json"} // already set — must be preserved
	d := &v1alpha1.BackendDefaults{Encoding: "no-op", SD: "dns"}

	applyBackendDefaultsToBackend(&b, d)

	if b.Encoding != "json" {
		t.Errorf("Encoding overwritten: got %q, want json", b.Encoding)
	}
	if b.SD != "dns" {
		t.Errorf("SD not filled: got %q, want dns", b.SD)
	}
}

func TestApplyURLTransformToEntries_AddAndStripPrefix(t *testing.T) {
	entries := []v1alpha1.EndpointEntry{
		{Endpoint: "/liveness", Method: "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/liveness"}}},
	}
	ApplyURLTransformToEntries(entries, &v1alpha1.URLTransformSpec{
		StripPathPrefix: "/api/v1",
		AddPathPrefix:   "/api/v1/quote",
	})
	if entries[0].Endpoint != "/api/v1/quote/liveness" {
		t.Fatalf("want /api/v1/quote/liveness, got %s", entries[0].Endpoint)
	}
	if entries[0].Backends[0].URLPattern != "/liveness" {
		t.Fatalf("backend urlPattern must be untouched, got %s", entries[0].Backends[0].URLPattern)
	}
}

func TestApplyURLTransformToEntries_HostMapping(t *testing.T) {
	entries := []v1alpha1.EndpointEntry{
		{Endpoint: "/x", Method: "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://old"}, URLPattern: "/x"}}},
	}
	ApplyURLTransformToEntries(entries, &v1alpha1.URLTransformSpec{
		HostMapping: []v1alpha1.HostMappingEntry{{From: "http://old", To: "http://new"}},
	})
	if entries[0].Backends[0].Host[0] != "http://new" {
		t.Fatalf("host mapping not applied: %s", entries[0].Backends[0].Host[0])
	}
}

func TestApplyURLTransformToEntries_NilTransformNoop(t *testing.T) {
	entries := []v1alpha1.EndpointEntry{{Endpoint: "/x", Method: "GET"}}
	ApplyURLTransformToEntries(entries, nil)
	if entries[0].Endpoint != "/x" {
		t.Fatalf("nil transform must be a no-op, got %s", entries[0].Endpoint)
	}
}

// evaluateEmbedded evaluates specJSON against the embedded default CUE
// definitions with the given overrides.
func evaluateEmbedded(t *testing.T, specJSON string, overrides ...v1alpha1.OperationOverride) *CUEOutput {
	t.Helper()
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}
	out, err := NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(specJSON),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Overrides:   overrides,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return out
}

func TestEvaluate_OverrideExtraConfigWithNonIdentifierOperationID(t *testing.T) {
	// SanitizeName maps "_" to "-", and a leading digit is not a CUE
	// identifier: the override label must be quoted.
	for _, opID := range []string{"get_a", "get-a", "1getA", "___"} {
		t.Run(opID, func(t *testing.T) {
			spec := fmt.Sprintf(`{"paths":{"/a":{"get":{"operationId":%q,`+
				`"responses":{"200":{"description":"OK"}}}}}}`, opID)
			out := evaluateEmbedded(t, spec, v1alpha1.OperationOverride{
				OperationID: opID,
				ExtraConfig: &runtime.RawExtension{Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`)},
			})
			if len(out.Entries) != 1 {
				t.Fatalf("expected 1 entry, got %d", len(out.Entries))
			}
			// These rows pin that the injected label compiles; the entry is
			// merged by the evaluator, not by a CUE lookup.
			if out.Entries[0].ExtraConfig == nil ||
				!strings.Contains(string(out.Entries[0].ExtraConfig.Raw), `"auth/validator"`) {
				t.Errorf("expected the override's auth/validator, got %s", out.Entries[0].ExtraConfig.Raw)
			}
		})
	}
}

func TestCUEEvaluator_OverridesKeyedBySanitizedOperationID(t *testing.T) {
	spec := `{"paths":{"/a":{"get":{"operationId":"get_a"}}}}`
	out, err := NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(spec),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: map[string]string{"main.cue": overrideLookupDefs},
		Overrides: []v1alpha1.OperationOverride{{
			OperationID: "get_a",
			ExtraConfig: &runtime.RawExtension{Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`)},
		}},
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
	if len(out.Entries[0].Backends) == 0 {
		t.Fatal("expected a backend")
	}
	be := out.Entries[0].Backends[0]
	if be.ExtraConfig == nil || !strings.Contains(string(be.ExtraConfig.Raw), `"auth/validator"`) {
		t.Errorf("expected the definition to find the override by the sanitized operationId, got %v", be.ExtraConfig)
	}
}

func TestEvaluate_UnsupportedMethodsAreSkippedAndReported(t *testing.T) {
	out := evaluateEmbedded(t, `{"paths":{"/a":{
		"get":{"operationId":"getA","responses":{"200":{"description":"OK"}}},
		"head":{"operationId":"headA","responses":{"200":{"description":"OK"}}},
		"options":{"responses":{"200":{"description":"OK"}}},
		"trace":{"operationId":"traceA","responses":{"200":{"description":"OK"}}}}}}`)

	if len(out.Entries) != 1 || out.Entries[0].Method != "GET" {
		t.Fatalf("expected only the GET entry, got %+v", out.Entries)
	}
	var got []string
	for _, s := range out.Skipped {
		if s.Reason != v1alpha1.ReasonUnsupportedMethod {
			t.Errorf("skipped %s %s: reason %q", s.Method, s.Path, s.Reason)
		}
		got = append(got, s.Method+" "+s.Path+" "+s.OperationID)
	}
	if want := []string{"HEAD /a headA", "OPTIONS /a ", "TRACE /a traceA"}; !slices.Equal(got, want) {
		t.Errorf("skipped = %q, want %q", got, want)
	}
}

func TestEvaluate_SkippedOperationsCarryTheTransformedPath(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}
	out, err := NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
		SpecData: []byte(`{"paths":{"/v1/a":{"head":{"operationId":"headA",` +
			`"responses":{"200":{"description":"OK"}}}}}}`),
		SpecFormat:   v1alpha1.SpecFormatJSON,
		DefaultDefs:  defs,
		URLTransform: &v1alpha1.URLTransformSpec{StripPathPrefix: "/v1", AddPathPrefix: "/api"},
		ServiceName:  "_spec",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Path != "/api/a" {
		t.Errorf("skipped = %+v, want one entry at /api/a", out.Skipped)
	}
}

func TestEvaluate_OverrideMethodToSupportedGeneratesTheEntry(t *testing.T) {
	out := evaluateEmbedded(t, `{"paths":{"/a":{"head":{"operationId":"headA",`+
		`"responses":{"200":{"description":"OK"}}}}}}`,
		v1alpha1.OperationOverride{OperationID: "headA", Method: "GET"})

	if len(out.UnmatchedOverrides) != 0 {
		t.Errorf("unmatched overrides = %q, want none", out.UnmatchedOverrides)
	}
	if len(out.Entries) != 1 || out.Entries[0].Method != "GET" {
		t.Errorf("entries = %+v, want one GET entry", out.Entries)
	}
	if len(out.Skipped) != 0 {
		t.Errorf("skipped = %+v, want none", out.Skipped)
	}
}

func TestEvaluate_OverrideMethodToUnsupportedIsSkipped(t *testing.T) {
	// A stored override from before admission rejected such methods.
	out := evaluateEmbedded(t, `{"paths":{"/a":{"get":{"operationId":"getA",`+
		`"responses":{"200":{"description":"OK"}}}}}}`,
		v1alpha1.OperationOverride{OperationID: "getA", Method: "HEAD"})

	if len(out.Entries) != 0 {
		t.Errorf("entries = %+v, want none", out.Entries)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Method != "HEAD" || out.Skipped[0].OperationID != "getA" ||
		out.Skipped[0].Reason != v1alpha1.ReasonUnsupportedMethod {
		t.Errorf("skipped = %+v, want HEAD /a getA UnsupportedMethod", out.Skipped)
	}
	if _, ok := out.OperationIDs["/a:HEAD"]; ok {
		t.Errorf("operationIds still lists the skipped entry: %v", out.OperationIDs)
	}
}

func TestEvaluate_InvalidEntryFailsOnlyItsOperation(t *testing.T) {
	// A response without a description fails concrete validation for its
	// entry only.
	out := evaluateEmbedded(t, `{"paths":{
		"/a":{"get":{"operationId":"getA","responses":{"200":{"description":"OK"}}}},
		"/b":{"get":{"operationId":"getB","tags":["b"],"responses":{"200":{}}}}}}`)

	if len(out.Entries) != 1 || out.Entries[0].Endpoint != "/a" {
		t.Fatalf("expected only the /a entry, got %+v", out.Entries)
	}
	if len(out.Failed) != 1 {
		t.Fatalf("expected 1 failed operation, got %+v", out.Failed)
	}
	f := out.Failed[0]
	if f.Method != "GET" || f.Path != "/b" || f.OperationID != "getB" || !slices.Equal(f.Tags, []string{"b"}) ||
		f.Reason != v1alpha1.ReasonCUEEvaluationFailed || !strings.Contains(f.Message, "description") {
		t.Errorf("unexpected failed operation %+v", f)
	}
}

func TestEvaluate_UndecodableEntryFailsOnlyItsOperation(t *testing.T) {
	// timeout "30" is valid CUE but not a duration: the entry cannot be
	// decoded, and must fail its operation instead of disappearing.
	out := evaluateEmbedded(t, `{"paths":{
		"/a":{"get":{"operationId":"getA","responses":{"200":{"description":"OK"}}}},
		"/b":{"get":{"operationId":"getB","timeout":"30","responses":{"200":{"description":"OK"}}}}}}`)

	if len(out.Entries) != 1 || out.Entries[0].Endpoint != "/a" {
		t.Fatalf("expected only the /a entry, got %+v", out.Entries)
	}
	if len(out.Failed) != 1 || out.Failed[0].OperationID != "getB" ||
		!strings.Contains(out.Failed[0].Message, "missing unit in duration") {
		t.Errorf("expected getB failed with the duration error, got %+v", out.Failed)
	}
}

func TestEvaluate_ErrorOutsideEntriesFailsEvaluation(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}
	_, err = NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(`{"paths":{"/a":{"get":{"operationId":"getA","responses":{"200":{"description":"OK"}}}}}}`),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		CustomDefs:  map[string]string{"custom.cue": `_defaultTimeout: "5s"`},
		ServiceName: "_spec",
	})
	if err == nil || !strings.Contains(err.Error(), "_defaultTimeout") {
		t.Errorf("expected a whole-evaluation error naming _defaultTimeout, got %v", err)
	}
}

func TestEvaluate_OverrideOnFailedOperationIsHeldNotUnmatched(t *testing.T) {
	out := evaluateEmbedded(t, `{"paths":{"/b":{"get":{"operationId":"getB","responses":{"200":{}}}}}}`,
		v1alpha1.OperationOverride{OperationID: "getB"})
	if len(out.UnmatchedOverrides) != 0 {
		t.Errorf("expected no unmatched overrides for a failed target, got %v", out.UnmatchedOverrides)
	}
}

func TestEvaluate_FailedOperationCarriesTransformedPath(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}
	out, err := NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
		SpecData:     []byte(`{"paths":{"/b":{"get":{"operationId":"getB","responses":{"200":{}}}}}}`),
		SpecFormat:   v1alpha1.SpecFormatJSON,
		DefaultDefs:  defs,
		URLTransform: &v1alpha1.URLTransformSpec{AddPathPrefix: "/svc"},
		ServiceName:  "_spec",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Failed) != 1 || out.Failed[0].Path != "/svc/b" {
		t.Errorf("expected the failed operation at /svc/b, got %+v", out.Failed)
	}
}

func TestEvaluate_FailedOperationsAreSortedByTransformedPath(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}
	// Stripping /z reorders the paths: /z/a becomes /a and sorts before /b.
	out, err := NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
		SpecData: []byte(`{"paths":{
			"/b":{"get":{"operationId":"getB","responses":{"200":{}}}},
			"/z/a":{"get":{"operationId":"getA","responses":{"200":{}}}}}}`),
		SpecFormat:   v1alpha1.SpecFormatJSON,
		DefaultDefs:  defs,
		URLTransform: &v1alpha1.URLTransformSpec{StripPathPrefix: "/z"},
		ServiceName:  "_spec",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Failed) != 2 || out.Failed[0].Path != "/a" || out.Failed[1].Path != "/b" {
		t.Errorf("expected failed operations sorted /a, /b, got %+v", out.Failed)
	}
}

func TestEvaluate_FailedMessageIsStableAcrossRuns(t *testing.T) {
	// The message lands in status, so a different text on each pass would
	// rewrite it forever. CUE reports a conflict in the order its operands
	// were unified, which follows the order the definition files load in.
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}
	messages := map[string]bool{}
	for range 100 {
		out, err := NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
			SpecData:    []byte(`{"paths":{"/b":{"get":{"operationId":"getB","responses":{"200":{"description":"OK"}}}}}}`),
			SpecFormat:  v1alpha1.SpecFormatJSON,
			DefaultDefs: defs,
			CustomDefs: map[string]string{
				"a.cue": `endpoint: "/b:GET": timeout: "1s"`,
				"b.cue": `endpoint: "/b:GET": timeout: "2s"`,
				"c.cue": `endpoint: "/b:GET": timeout: "3s"`,
			},
			ServiceName: "_spec",
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if len(out.Failed) != 1 {
			t.Fatalf("expected 1 failed operation, got %+v", out.Failed)
		}
		messages[out.Failed[0].Message] = true
	}
	if len(messages) != 1 {
		t.Errorf("expected one message across runs, got %d: %v", len(messages), messages)
	}
}

// evaluateWithCustomDefs evaluates specJSON against the embedded default
// definitions plus custom definition files.
func evaluateWithCustomDefs(specJSON string, custom map[string]string) (*CUEOutput, error) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		return nil, err
	}
	return NewCUEEvaluator().Evaluate(context.Background(), CUEInput{
		SpecData:    []byte(specJSON),
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		CustomDefs:  custom,
		ServiceName: "_spec",
	})
}

const twoOperationSpec = `{"paths":{
	"/a":{"get":{"operationId":"getA","responses":{"200":{"description":"OK"}}}},
	"/b":{"get":{"operationId":"getB","responses":{"200":{"description":"OK"}}}}}}`

func TestEvaluate_ErrorUnderEndpointPatternFailsEvaluation(t *testing.T) {
	// A typo in a pattern constraint reaches every entry, and no one entry
	// owns the error: dropping it would publish every endpoint without the
	// validator.
	_, err := evaluateWithCustomDefs(twoOperationSpec, map[string]string{
		"custom.cue": `_authCfg: {x: 1}
endpoint: [string]: extraConfig: "auth/validator": _authCgf`,
	})
	if err == nil || !strings.Contains(err.Error(), "_authCgf") {
		t.Errorf("expected a whole-evaluation error naming _authCgf, got %v", err)
	}
}

func TestEvaluate_RootErrorOnEntryFailsThatEntry(t *testing.T) {
	// The reference is unresolved, which only the root validation reports:
	// the entry alone decodes, with the default timeout.
	out, err := evaluateWithCustomDefs(twoOperationSpec, map[string]string{
		"custom.cue": `endpoint: "/b:GET": timeout: _overrides["getb"].t`,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Entries) != 1 || out.Entries[0].Endpoint != "/a" {
		t.Fatalf("expected only the /a entry, got %+v", out.Entries)
	}
	if len(out.Failed) != 1 || out.Failed[0].OperationID != "getB" ||
		!strings.Contains(out.Failed[0].Message, `reference "_overrides" not found`) {
		t.Errorf("expected getB failed with the root error, got %+v", out.Failed)
	}
}

func TestEvaluate_ConflictInHiddenLabelUnderEndpointFailsEvaluation(t *testing.T) {
	// The entry falls back to its default through a disjunction, so it
	// decodes, while the conflict in the hidden label belongs to no entry.
	_, err := evaluateWithCustomDefs(twoOperationSpec, map[string]string{
		"a.cue": `endpoint: _h: t: "1s"`,
		"b.cue": `endpoint: _h: t: "2s"`,
		"c.cue": `endpoint: "/b:GET": timeout: *endpoint._h.t | "3s"`,
	})
	if err == nil || !strings.Contains(err.Error(), "_h") {
		t.Errorf("expected a whole-evaluation error naming _h, got %v", err)
	}
}

func TestEvaluate_OverrideRemapsAFailedOperation(t *testing.T) {
	// A HEAD operation that fails CUE and an override that remaps it to GET
	// /v2/users: the failure belongs to the route the override gives it.
	out := evaluateEmbedded(t, `{"paths":{"/legacy/users":{"head":{"operationId":"headUsers","responses":{"200":{}}}}}}`,
		v1alpha1.OperationOverride{OperationID: "headUsers", Endpoint: "/v2/users", Method: "GET"})

	if len(out.Failed) != 1 || out.Failed[0].Method != "GET" || out.Failed[0].Path != "/v2/users" {
		t.Errorf("expected the failure at GET /v2/users, got %+v", out.Failed)
	}
}

func TestEvaluate_FailedOperationWithUnsupportedMethodIsSkipped(t *testing.T) {
	// A HEAD operation can never publish, so its CUE failure must not hold
	// anything back: it is skipped like any other unsupported method.
	out := evaluateEmbedded(t, `{"paths":{"/legacy/users":{"head":{"operationId":"headUsers","responses":{"200":{}}}}}}`)

	if len(out.Failed) != 0 {
		t.Errorf("expected no failed operation, got %+v", out.Failed)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Method != "HEAD" || out.Skipped[0].OperationID != "headUsers" ||
		out.Skipped[0].Reason != v1alpha1.ReasonUnsupportedMethod {
		t.Errorf("expected headUsers skipped as UnsupportedMethod, got %+v", out.Skipped)
	}
}

func TestEvaluate_FailedOperationWithUnknownMethodStaysFailed(t *testing.T) {
	// A conflicting method leaves the entry's method unknown. The operation
	// is not known to be unsupported, so it must keep failing the sync
	// rather than be skipped and let its endpoint be deleted as stale.
	out, err := evaluateWithCustomDefs(`{"paths":{}}`, map[string]string{
		"a.cue": `endpoint: getUser: {
	endpoint: "/users/{id}"
	method: "GET"
	backends: [{host: ["http://x"], urlPattern: "/users/{id}", method: "GET"}]
	_operationId: "getUser"
}`,
		"b.cue": `endpoint: getUser: method: "POST"`,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Skipped) != 0 {
		t.Errorf("expected nothing skipped, got %+v", out.Skipped)
	}
	if len(out.Failed) != 1 || out.Failed[0].OperationID != "getUser" {
		t.Errorf("expected getUser failed, got %+v", out.Failed)
	}
}

func TestEvaluate_FailedOperationWithLabelMethodStaysFailed(t *testing.T) {
	// The label suffix HEAD is not a method the entry declares: its method
	// is not concrete, so the failure must not be skipped.
	out, err := evaluateWithCustomDefs(`{"paths":{}}`, map[string]string{
		"a.cue": `endpoint: "users:HEAD": {
	endpoint: "/users"
	method: string
	_operationId: "listUsers"
}`,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(out.Skipped) != 0 || len(out.Failed) != 1 || out.Failed[0].OperationID != "listUsers" {
		t.Errorf("expected listUsers failed and nothing skipped, got failed %+v skipped %+v", out.Failed, out.Skipped)
	}
}
