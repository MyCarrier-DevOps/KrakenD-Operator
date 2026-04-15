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
	"encoding/json"
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
		Raw: []byte(`{"qos/ratelimit/router":{"every":"2s","max_rate":10},"documentation/openapi":{"operation_id":"foo"}}`),
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
		{OperationID: "listUsers", Method: "HEAD"},
	})

	if out.Entries[0].Method != "HEAD" {
		t.Errorf("expected method HEAD, got %s", out.Entries[0].Method)
	}
	if out.OperationIDs["/api/users:HEAD"] != "listUsers" {
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
			Method:      "OPTIONS",
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
	if entry.Method != "OPTIONS" {
		t.Errorf("method = %s, want OPTIONS", entry.Method)
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
	if out.OperationIDs["/api/v3/users:OPTIONS"] != "listUsers" {
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
		if len(entry.InputHeaders) != 2 || entry.InputHeaders[0] != "Authorization" || entry.InputHeaders[1] != "X-Custom" {
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
		Raw: []byte(`{"backend/http":{"return_error_code":true,"return_error_msg":false},"qos/ratelimit/proxy":{"max_rate":100}}`),
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
