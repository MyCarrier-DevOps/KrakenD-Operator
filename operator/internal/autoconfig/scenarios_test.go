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

// Functional scenario tests for the autoconfig pipeline.
//
// Each test mirrors a realistic KrakenDAutoConfig CR and verifies the
// generated KrakenDEndpoint CRs match what the user expects.
// These test the BEHAVIOR, not individual functions.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// orderServiceSpec is a realistic OpenAPI spec matching the user's Order service.
var orderServiceSpec = []byte(`{
	"openapi": "3.0.3",
	"info": {"title": "Order Public API", "version": "1.0.0"},
	"paths": {
		"/api/Orders": {
			"post": {
				"operationId": "UploadOrder",
				"summary": "Create or Modify Orders",
				"description": "Create a new order or update existing by Reference ID",
				"tags": ["Orders"],
				"requestBody": {
					"content": {
						"application/json": {
							"schema": {"$ref": "#/components/schemas/OrderUploadRequest"}
						}
					}
				},
				"responses": {
					"200": {
						"description": "Request was successfully added to the processing queue",
						"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Result"}}}
					},
					"400": {
						"description": "Request has missing/invalid values",
						"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Result"}}}
					},
					"401": {"description": "Unauthorized"}
				}
			}
		},
		"/api/Orders/referenceId/{referenceId}": {
			"parameters": [
				{"name": "referenceId", "in": "path", "required": true, "description": "Reference ID"}
			],
			"get": {
				"operationId": "Order",
				"summary": "Get Order by reference id",
				"description": "Get an order by Reference Id",
				"tags": ["Orders"],
				"responses": {
					"200": {
						"description": "Request was successfully executed",
						"content": {"application/json": {"schema": {"$ref": "#/components/schemas/OrderModel"}}}
					},
					"404": {"description": "Resource not found"},
					"401": {"description": "Unauthorized"}
				}
			},
			"delete": {
				"operationId": "DeleteOrder",
				"summary": "Delete Order",
				"description": "Delete an order by Reference ID",
				"tags": ["Orders"],
				"responses": {
					"200": {
						"description": "Request was successfully added to the processing queue",
						"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Result"}}}
					},
					"400": {
						"description": "Request has missing/invalid values",
						"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Result"}}}
					},
					"401": {"description": "Unauthorized"}
				}
			}
		}
	}
}`)

// evaluateScenario runs the full CUE evaluation pipeline with the embedded
// defaults, returning entries keyed by operationID for easy assertion.
func evaluateScenario(t *testing.T, input CUEInput) map[string]v1alpha1.EndpointEntry {
	t.Helper()
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading embedded CUE defs: %v", err)
	}
	input.DefaultDefs = defs
	if input.ServiceName == "" {
		input.ServiceName = "_spec"
	}
	if input.SpecFormat == "" {
		input.SpecFormat = v1alpha1.SpecFormatJSON
	}

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), input)
	if err != nil {
		t.Fatalf("CUE evaluation failed: %v", err)
	}

	byOpID := make(map[string]v1alpha1.EndpointEntry, len(out.Entries))
	for _, entry := range out.Entries {
		key := entry.Endpoint + ":" + entry.Method
		opID := out.OperationIDs[key]
		if opID == "" {
			opID = key // fallback for operations without operationId
		}
		byOpID[opID] = entry
	}
	return byOpID
}

// extraConfigMap parses ExtraConfig into a keyed map for assertions.
func extraConfigMap(t *testing.T, ec *runtime.RawExtension) map[string]json.RawMessage {
	t.Helper()
	if ec == nil {
		t.Fatal("extraConfig is nil")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(ec.Raw, &m); err != nil {
		t.Fatalf("unmarshal extraConfig: %v", err)
	}
	return m
}

// extraConfigField unmarshals a specific key from ExtraConfig.
func extraConfigField(t *testing.T, ec *runtime.RawExtension, key string) map[string]interface{} {
	t.Helper()
	m := extraConfigMap(t, ec)
	raw, ok := m[key]
	if !ok {
		t.Fatalf("extraConfig missing key %q", key)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal %s: %v", key, err)
	}
	return result
}

// =========================================================================
// Scenario: User configures defaults (timeout, inputHeaders)
// Expected: ALL generated endpoints inherit those values.
// This was a real bug — spec.defaults was declared but never consumed.
// =========================================================================

func TestScenario_DefaultsAppliedToAllEndpoints(t *testing.T) {
	timeout := metav1.Duration{Duration: 20 * time.Second}

	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api.dev.svc:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			Timeout:      &timeout,
			InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
		},
	})

	for opID, entry := range entries {
		// Every endpoint should have the user-specified timeout, not the CUE default 3s
		if entry.Timeout == nil || entry.Timeout.Duration != 20*time.Second {
			t.Errorf("%s: expected timeout 20s, got %v", opID, entry.Timeout)
		}
		// Every endpoint should have exactly the user-specified headers
		if len(entry.InputHeaders) != 3 {
			t.Errorf("%s: expected 3 inputHeaders, got %v", opID, entry.InputHeaders)
		}
	}
}

// =========================================================================
// Scenario: User sets per-operation overrides (endpoint path, timeout,
// extraConfig rate limit). This mirrors the user's actual AutoConfig CR.
// Expected: Overrides replace CUE-generated values for matching operations.
// This was a real bug — extraConfig overrides were injected into CUE but
// never read by defaults.cue.
// =========================================================================

func TestScenario_PerOperationOverrides(t *testing.T) {
	defaultTimeout := metav1.Duration{Duration: 20 * time.Second}
	uploadTimeout := metav1.Duration{Duration: 30 * time.Second}

	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api.dev.svc:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			Timeout:      &defaultTimeout,
			InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "UploadOrder",
				Endpoint:    "/api/v1/orders",
				Timeout:     &uploadTimeout,
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":10,"strategy":"header","key":"Authorization"}}`),
				},
			},
			{
				OperationID: "DeleteOrder",
				Endpoint:    "/api/v1/orders/referenceId/{referenceId}",
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":10,"strategy":"header","key":"Authorization"}}`),
				},
			},
			{
				OperationID: "Order",
				Endpoint:    "/api/v1/orders/referenceId/{referenceId}",
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":10,"strategy":"header","key":"Authorization"}}`),
				},
			},
		},
	})

	// UploadOrder: path remapped, timeout overridden to 30s, rate limit every=1s
	upload := entries["UploadOrder"]
	if upload.Endpoint != "/api/v1/orders" {
		t.Errorf("UploadOrder: expected endpoint /api/v1/orders, got %s", upload.Endpoint)
	}
	if upload.Timeout == nil || upload.Timeout.Duration != 30*time.Second {
		t.Errorf("UploadOrder: expected timeout 30s (override), got %v", upload.Timeout)
	}
	rl := extraConfigField(t, upload.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "1s" {
		t.Errorf("UploadOrder: expected rate limit every=1s, got %v", rl["every"])
	}
	// documentation/openapi should still be present from CUE
	ec := extraConfigMap(t, upload.ExtraConfig)
	if _, ok := ec["documentation/openapi"]; !ok {
		t.Error("UploadOrder: documentation/openapi should be preserved")
	}

	// Order: path remapped, timeout is default 20s (no per-op timeout override)
	order := entries["Order"]
	if order.Endpoint != "/api/v1/orders/referenceId/{referenceId}" {
		t.Errorf("Order: expected endpoint /api/v1/orders/referenceId/{referenceId}, got %s", order.Endpoint)
	}
	if order.Timeout == nil || order.Timeout.Duration != 20*time.Second {
		t.Errorf("Order: expected timeout 20s (default), got %v", order.Timeout)
	}
	rl = extraConfigField(t, order.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "1s" {
		t.Errorf("Order: expected rate limit every=1s, got %v", rl["every"])
	}

	// DeleteOrder: path remapped, timeout is default 20s
	del := entries["DeleteOrder"]
	if del.Endpoint != "/api/v1/orders/referenceId/{referenceId}" {
		t.Errorf("DeleteOrder: expected endpoint override, got %s", del.Endpoint)
	}
	if del.Timeout == nil || del.Timeout.Duration != 20*time.Second {
		t.Errorf("DeleteOrder: expected timeout 20s (default), got %v", del.Timeout)
	}
}

// =========================================================================
// Scenario: User configures URL transforms (strip path prefix, add prefix,
// host mapping) plus overrides. Order of operations matters: URL transforms
// run before field overrides, so an endpoint override should take final
// precedence.
// =========================================================================

func TestScenario_URLTransformWithOverrides(t *testing.T) {
	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://old-host.internal:8080",
		URLTransform: &v1alpha1.URLTransformSpec{
			HostMapping: []v1alpha1.HostMappingEntry{
				{From: "http://old-host.internal:8080", To: "http://order-api.dev.svc:8080"},
			},
			StripPathPrefix: "/api",
			AddPathPrefix:   "/api/v2",
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				// Override should win over URL transform for this operation
				OperationID: "UploadOrder",
				Endpoint:    "/api/v1/orders",
			},
		},
	})

	// UploadOrder: override takes final precedence over URL transform
	upload := entries["UploadOrder"]
	if upload.Endpoint != "/api/v1/orders" {
		t.Errorf("UploadOrder: expected /api/v1/orders (override wins), got %s", upload.Endpoint)
	}
	// Backend host should be mapped
	if upload.Backends[0].Host[0] != "http://order-api.dev.svc:8080" {
		t.Errorf("UploadOrder: expected host mapping applied, got %s", upload.Backends[0].Host[0])
	}

	// Order: URL transform applied (no override), strip /api, add /api/v2
	order := entries["Order"]
	if order.Endpoint != "/api/v2/Orders/referenceId/{referenceId}" {
		t.Errorf("Order: expected URL transform result /api/v2/Orders/referenceId/{referenceId}, got %s", order.Endpoint)
	}
	if order.Backends[0].Host[0] != "http://order-api.dev.svc:8080" {
		t.Errorf("Order: expected host mapping applied, got %s", order.Backends[0].Host[0])
	}
}

// =========================================================================
// Scenario: User sets filter to include only GET methods.
// Expected: POST and DELETE operations are excluded.
// =========================================================================

func TestScenario_FilterByMethod(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    orderServiceSpec,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
		DefaultHost: "http://order-api.dev.svc:8080",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	filter := NewFilter()
	filtered := filter.Apply(out.Entries, out.Tags, out.OperationIDs, v1alpha1.FilterSpec{
		IncludeMethods: []string{"GET"},
	})

	if len(filtered) != 1 {
		t.Fatalf("expected 1 entry (GET only), got %d", len(filtered))
	}
	if filtered[0].Method != "GET" {
		t.Errorf("expected GET method, got %s", filtered[0].Method)
	}
}

// =========================================================================
// Scenario: User sets filter to exclude a specific operation by ID.
// Expected: That operation is not generated.
// =========================================================================

func TestScenario_FilterExcludeOperationID(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    orderServiceSpec,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
		DefaultHost: "http://order-api.dev.svc:8080",
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	filter := NewFilter()
	filtered := filter.Apply(out.Entries, out.Tags, out.OperationIDs, v1alpha1.FilterSpec{
		ExcludeOperationIds: []string{"DeleteOrder"},
	})

	if len(filtered) != 2 {
		t.Fatalf("expected 2 entries (excluding DeleteOrder), got %d", len(filtered))
	}
	for _, entry := range filtered {
		key := entry.Endpoint + ":" + entry.Method
		if out.OperationIDs[key] == "DeleteOrder" {
			t.Error("DeleteOrder should have been filtered out")
		}
	}
}

// =========================================================================
// Scenario: Full pipeline — defaults + overrides + filter + generator.
// Mirrors a realistic user workflow end-to-end.
// Expected: Generated KrakenDEndpoint CRs have correct metadata, correct
// spec values reflecting all configuration.
// =========================================================================

func TestScenario_FullPipelineWithAllFeatures(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading defs: %v", err)
	}

	defaultTimeout := metav1.Duration{Duration: 15 * time.Second}
	overrideTimeout := metav1.Duration{Duration: 45 * time.Second}

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    orderServiceSpec,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
		DefaultHost: "http://order-api.dev.svc:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			Timeout:      &defaultTimeout,
			InputHeaders: []string{"Authorization", "Content-Type"},
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "UploadOrder",
				Endpoint:    "/api/v1/orders",
				Timeout:     &overrideTimeout,
			},
		},
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Filter: exclude DeleteOrder
	filter := NewFilter()
	filtered := filter.Apply(out.Entries, out.Tags, out.OperationIDs, v1alpha1.FilterSpec{
		ExcludeOperationIds: []string{"DeleteOrder"},
	})

	if len(filtered) != 2 {
		t.Fatalf("expected 2 entries after filter, got %d", len(filtered))
	}

	// Generate KrakenDEndpoint CRs
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "order-public-api", Namespace: "krakend-operator"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "mycarrier-public-api"},
		},
	}
	gen := NewGenerator()
	genOut, err := gen.Generate(context.Background(), GenerateInput{
		AutoConfig:   ac,
		Entries:      filtered,
		OperationIDs: out.OperationIDs,
		GatewayRef:   ac.Spec.GatewayRef,
	})
	if err != nil {
		t.Fatalf("generator: %v", err)
	}

	if len(genOut.Endpoints) != 2 {
		t.Fatalf("expected 2 generated endpoints, got %d", len(genOut.Endpoints))
	}

	// Build lookup by operationID (from name suffix)
	byName := map[string]*v1alpha1.KrakenDEndpoint{}
	for _, ep := range genOut.Endpoints {
		byName[ep.Name] = ep
	}

	// Verify UploadOrder endpoint
	upload, ok := byName["order-public-api-uploadorder"]
	if !ok {
		t.Fatal("missing endpoint order-public-api-uploadorder")
	}
	if upload.Spec.GatewayRef.Name != "mycarrier-public-api" {
		t.Errorf("gatewayRef = %s, want mycarrier-public-api", upload.Spec.GatewayRef.Name)
	}
	if upload.Labels["gateway.krakend.io/auto-generated"] != "true" {
		t.Error("missing auto-generated label")
	}
	if len(upload.Spec.Endpoints) != 1 {
		t.Fatalf("expected 1 entry in spec, got %d", len(upload.Spec.Endpoints))
	}
	entry := upload.Spec.Endpoints[0]
	if entry.Endpoint != "/api/v1/orders" {
		t.Errorf("endpoint = %s, want /api/v1/orders", entry.Endpoint)
	}
	if entry.Timeout == nil || entry.Timeout.Duration != 45*time.Second {
		t.Errorf("timeout = %v, want 45s (override wins over default)", entry.Timeout)
	}
	if len(entry.InputHeaders) != 2 || entry.InputHeaders[0] != "Authorization" {
		t.Errorf("inputHeaders = %v, want [Authorization Content-Type]", entry.InputHeaders)
	}

	// Verify Order endpoint
	order, ok := byName["order-public-api-order"]
	if !ok {
		t.Fatal("missing endpoint order-public-api-order")
	}
	orderEntry := order.Spec.Endpoints[0]
	if orderEntry.Timeout == nil || orderEntry.Timeout.Duration != 15*time.Second {
		t.Errorf("Order timeout = %v, want 15s (from defaults)", orderEntry.Timeout)
	}

	// Verify DeleteOrder was NOT generated (filtered out)
	if _, ok := byName["order-public-api-deleteorder"]; ok {
		t.Error("DeleteOrder should have been filtered out")
	}
}

// =========================================================================
// Scenario: No defaults, no overrides, no transforms — just the raw
// CUE output. Verifies the CUE defaults produce sane baseline values.
// =========================================================================

func TestScenario_BareMinimumAutoConfig(t *testing.T) {
	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api:8080",
	})

	if len(entries) != 3 {
		t.Fatalf("expected 3 operations (UploadOrder, Order, DeleteOrder), got %d", len(entries))
	}

	// All should have CUE default timeout "3s" (string from CUE, not metav1.Duration)
	for opID, entry := range entries {
		// Backend should point at the injected host
		if entry.Backends[0].Host[0] != "http://order-api:8080" {
			t.Errorf("%s: expected host http://order-api:8080, got %s", opID, entry.Backends[0].Host[0])
		}
		// Should have auth headers from CUE defaults (_defaultAuth: true)
		foundAuth := false
		for _, h := range entry.InputHeaders {
			if h == "Authorization" {
				foundAuth = true
			}
		}
		if !foundAuth {
			t.Errorf("%s: expected Authorization header from CUE defaults, got %v", opID, entry.InputHeaders)
		}
		// Should have rate limiting from CUE defaults
		rl := extraConfigField(t, entry.ExtraConfig, "qos/ratelimit/router")
		if rl["every"] != "2s" {
			t.Errorf("%s: expected CUE default rate limit every=2s, got %v", opID, rl["every"])
		}
		// Should have OpenAPI documentation
		ec := extraConfigMap(t, entry.ExtraConfig)
		if _, ok := ec["documentation/openapi"]; !ok {
			t.Errorf("%s: expected documentation/openapi from CUE", opID)
		}
	}

	// Verify methods match the spec
	if entries["UploadOrder"].Method != "POST" {
		t.Errorf("UploadOrder: expected POST, got %s", entries["UploadOrder"].Method)
	}
	if entries["Order"].Method != "GET" {
		t.Errorf("Order: expected GET, got %s", entries["Order"].Method)
	}
	if entries["DeleteOrder"].Method != "DELETE" {
		t.Errorf("DeleteOrder: expected DELETE, got %s", entries["DeleteOrder"].Method)
	}
}

// =========================================================================
// Scenario: Backend host should come from DefaultHost injection.
// The backend urlPattern should be the original OpenAPI path.
// =========================================================================

func TestScenario_BackendURLPatternPreservedAfterEndpointOverride(t *testing.T) {
	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api.dev.svc:8080",
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "UploadOrder",
				Endpoint:    "/api/v1/orders",
			},
		},
	})

	upload := entries["UploadOrder"]
	// Endpoint should be overridden
	if upload.Endpoint != "/api/v1/orders" {
		t.Errorf("endpoint = %s, want /api/v1/orders", upload.Endpoint)
	}
	// But the backend URL pattern should still be the original OpenAPI path
	if upload.Backends[0].URLPattern != "/api/Orders" {
		t.Errorf("backend urlPattern = %s, want /api/Orders (original path)", upload.Backends[0].URLPattern)
	}
}

// =========================================================================
// Scenario: PolicyRef in defaults applies to all backends across all
// endpoints. Per-operation PolicyRef override replaces it for one
// specific operation.
// =========================================================================

func TestScenario_DefaultPolicyRefOverriddenPerOperation(t *testing.T) {
	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			PolicyRef: &v1alpha1.PolicyRef{Name: "default-policy"},
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "UploadOrder",
				PolicyRef:   &v1alpha1.PolicyRef{Name: "upload-specific-policy"},
			},
		},
	})

	// UploadOrder: override policy
	for _, be := range entries["UploadOrder"].Backends {
		if be.PolicyRef == nil || be.PolicyRef.Name != "upload-specific-policy" {
			t.Errorf("UploadOrder backend: expected upload-specific-policy, got %v", be.PolicyRef)
		}
	}

	// Order: default policy
	for _, be := range entries["Order"].Backends {
		if be.PolicyRef == nil || be.PolicyRef.Name != "default-policy" {
			t.Errorf("Order backend: expected default-policy, got %v", be.PolicyRef)
		}
	}

	// DeleteOrder: default policy
	for _, be := range entries["DeleteOrder"].Backends {
		if be.PolicyRef == nil || be.PolicyRef.Name != "default-policy" {
			t.Errorf("DeleteOrder backend: expected default-policy, got %v", be.PolicyRef)
		}
	}
}

// =========================================================================
// Scenario: Empty inputQueryStrings in defaults overrides the CUE-generated
// query strings (which come from the OpenAPI spec's query parameters).
// =========================================================================

func TestScenario_EmptyQueryStringsOverridesCUE(t *testing.T) {
	// Use a spec with query parameters
	specWithQuery := []byte(`{
		"paths": {
			"/api/items": {
				"get": {
					"operationId": "listItems",
					"parameters": [
						{"name": "page", "in": "query"},
						{"name": "limit", "in": "query"}
					]
				}
			}
		}
	}`)

	entries := evaluateScenario(t, CUEInput{
		SpecData:    specWithQuery,
		DefaultHost: "http://items:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			InputQueryStrings: []string{},
		},
	})

	item := entries["listItems"]
	if item.InputQueryStrings == nil {
		t.Error("expected empty slice, got nil")
	}
	if len(item.InputQueryStrings) != 0 {
		t.Errorf("expected empty inputQueryStrings from defaults, got %v", item.InputQueryStrings)
	}
}

// =========================================================================
// Scenario: Per-operation overrides for OutputEncoding, ConcurrentCalls,
// InputHeaders, and InputQueryStrings. These fields were missing from
// OperationOverride causing CRD validation errors. Overrides should
// replace the default/CUE-generated values for the targeted operation
// only, leaving other operations unchanged.
// =========================================================================

func TestScenario_PerOperationFieldOverrides(t *testing.T) {
	defaultTimeout := metav1.Duration{Duration: 20 * time.Second}
	concurrentCalls := int32(3)

	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api.dev.svc:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			Timeout:      &defaultTimeout,
			InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID:       "UploadOrder",
				Endpoint:          "/api/v1/orders",
				OutputEncoding:    "no-op",
				ConcurrentCalls:   &concurrentCalls,
				InputHeaders:      []string{"Content-Type", "Environment"},
				InputQueryStrings: []string{"page", "limit"},
			},
		},
	})

	// UploadOrder: all four new fields overridden
	upload := entries["UploadOrder"]
	if upload.OutputEncoding != "no-op" {
		t.Errorf("UploadOrder: expected outputEncoding no-op, got %q", upload.OutputEncoding)
	}
	if upload.ConcurrentCalls == nil || *upload.ConcurrentCalls != 3 {
		t.Errorf("UploadOrder: expected concurrentCalls 3, got %v", upload.ConcurrentCalls)
	}
	if len(upload.InputHeaders) != 2 || upload.InputHeaders[0] != "Content-Type" || upload.InputHeaders[1] != "Environment" {
		t.Errorf("UploadOrder: expected inputHeaders [Content-Type Environment], got %v", upload.InputHeaders)
	}
	if len(upload.InputQueryStrings) != 2 || upload.InputQueryStrings[0] != "page" || upload.InputQueryStrings[1] != "limit" {
		t.Errorf("UploadOrder: expected inputQueryStrings [page limit], got %v", upload.InputQueryStrings)
	}

	// Order: should still have default values (no override)
	order := entries["Order"]
	if order.OutputEncoding == "no-op" {
		t.Error("Order: outputEncoding should not be no-op (not overridden)")
	}
	if order.ConcurrentCalls != nil {
		t.Errorf("Order: concurrentCalls should be nil (not overridden), got %v", order.ConcurrentCalls)
	}
	if len(order.InputHeaders) != 3 ||
		order.InputHeaders[0] != "Authorization" ||
		order.InputHeaders[1] != "X-MC-Api-Key" ||
		order.InputHeaders[2] != "Content-Type" {
		t.Errorf("Order: expected default inputHeaders [Authorization X-MC-Api-Key Content-Type], got %v", order.InputHeaders)
	}
	if len(order.InputQueryStrings) != 0 {
		t.Errorf("Order: expected no inputQueryStrings, got %v", order.InputQueryStrings)
	}
}

// =========================================================================
// Scenario: Default extraConfig is applied to all endpoints. Per-operation
// extraConfig overrides merge with (and take precedence over) the default.
// CUE-generated extraConfig (e.g. documentation/openapi) is preserved.
// =========================================================================

func TestScenario_DefaultExtraConfig(t *testing.T) {
	defaultTimeout := metav1.Duration{Duration: 20 * time.Second}

	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api.dev.svc:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			Timeout:      &defaultTimeout,
			InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
			ExtraConfig: &runtime.RawExtension{
				Raw: []byte(`{"qos/ratelimit/router":{"every":"2s","max_rate":5,"strategy":"header","key":"Authorization"}}`),
			},
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "UploadOrder",
				Endpoint:    "/api/v1/orders",
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":25}}`),
				},
			},
		},
	})

	// Order: should have default rate limit (not overridden)
	order := entries["Order"]
	rl := extraConfigField(t, order.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "2s" {
		t.Errorf("Order: expected default rate limit every=2s, got %v", rl["every"])
	}
	// CUE-generated documentation/openapi should still be present
	ec := extraConfigMap(t, order.ExtraConfig)
	if _, ok := ec["documentation/openapi"]; !ok {
		t.Error("Order: documentation/openapi should be preserved from CUE")
	}

	// UploadOrder: per-operation override deep-merges with the default rate limit config
	upload := entries["UploadOrder"]
	rl = extraConfigField(t, upload.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "1s" {
		t.Errorf("UploadOrder: expected override rate limit every=1s, got %v", rl["every"])
	}
	if rl["max_rate"] != float64(25) {
		t.Errorf("UploadOrder: expected override max_rate=25, got %v", rl["max_rate"])
	}
	if rl["strategy"] != "header" {
		t.Errorf("UploadOrder: expected preserved default strategy=header, got %v", rl["strategy"])
	}
	if rl["key"] != "Authorization" {
		t.Errorf("UploadOrder: expected preserved default key=Authorization, got %v", rl["key"])
	}
	// CUE-generated documentation/openapi should still be present
	ec = extraConfigMap(t, upload.ExtraConfig)
	if _, ok := ec["documentation/openapi"]; !ok {
		t.Error("UploadOrder: documentation/openapi should be preserved from CUE")
	}

	// DeleteOrder: should also have default rate limit
	del := entries["DeleteOrder"]
	rl = extraConfigField(t, del.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "2s" {
		t.Errorf("DeleteOrder: expected default rate limit every=2s, got %v", rl["every"])
	}
}

// =========================================================================
// Scenario: Default extraConfig sets one key (auth/validator), per-operation
// override sets a DIFFERENT key (qos/ratelimit/router). Both must coexist
// alongside CUE-generated keys (documentation/openapi). This proves that
// the 3-layer merge (CUE → defaults → overrides) is additive and stable.
// =========================================================================

func TestScenario_DefaultAndOverrideDifferentExtraConfigKeys(t *testing.T) {
	defaultTimeout := metav1.Duration{Duration: 20 * time.Second}

	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api.dev.svc:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			Timeout:      &defaultTimeout,
			InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
			ExtraConfig: &runtime.RawExtension{
				Raw: []byte(`{"auth/validator":{"alg":"RS256","audience":["https://api.example.com"]}}`),
			},
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "UploadOrder",
				Endpoint:    "/api/v1/orders",
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":10,"strategy":"header","key":"Authorization"}}`),
				},
			},
		},
	})

	// UploadOrder: should have ALL three layers
	upload := entries["UploadOrder"]
	ec := extraConfigMap(t, upload.ExtraConfig)

	// Layer 1: CUE-generated documentation/openapi
	if _, ok := ec["documentation/openapi"]; !ok {
		t.Error("UploadOrder: missing CUE-generated documentation/openapi")
	}
	// Layer 2: default auth/validator
	if _, ok := ec["auth/validator"]; !ok {
		t.Error("UploadOrder: missing default auth/validator")
	}
	auth := extraConfigField(t, upload.ExtraConfig, "auth/validator")
	if auth["alg"] != "RS256" {
		t.Errorf("UploadOrder: expected auth/validator alg=RS256, got %v", auth["alg"])
	}
	// Layer 3: per-operation rate limit
	rl := extraConfigField(t, upload.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "1s" {
		t.Errorf("UploadOrder: expected rate limit every=1s, got %v", rl["every"])
	}

	// Order: should have CUE + default, but NOT the per-operation rate limit
	order := entries["Order"]
	ec = extraConfigMap(t, order.ExtraConfig)

	if _, ok := ec["documentation/openapi"]; !ok {
		t.Error("Order: missing CUE-generated documentation/openapi")
	}
	if _, ok := ec["auth/validator"]; !ok {
		t.Error("Order: missing default auth/validator")
	}
	// No per-operation override was set for Order, so rate limit should
	// only be present if CUE generated one (it does from defaults.cue)
	// — the key point is auth/validator coexists with everything else.

	// DeleteOrder: same as Order — CUE + default, no per-operation override
	del := entries["DeleteOrder"]
	ec = extraConfigMap(t, del.ExtraConfig)

	if _, ok := ec["documentation/openapi"]; !ok {
		t.Error("DeleteOrder: missing CUE-generated documentation/openapi")
	}
	if _, ok := ec["auth/validator"]; !ok {
		t.Error("DeleteOrder: missing default auth/validator")
	}
}

// =========================================================================
// Scenario: Default extraConfig sets qos/ratelimit/router with multiple
// sub-fields. Per-operation override sets the SAME top-level key but only
// changes one sub-field. Deep merge preserves the unspecified sub-fields
// from the default while applying the override's changes.
// =========================================================================

func TestScenario_OverrideSameExtraConfigKeyDeepMerge(t *testing.T) {
	defaultTimeout := metav1.Duration{Duration: 20 * time.Second}

	entries := evaluateScenario(t, CUEInput{
		SpecData:    orderServiceSpec,
		DefaultHost: "http://order-api.dev.svc:8080",
		Defaults: &v1alpha1.EndpointDefaults{
			Timeout:      &defaultTimeout,
			InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
			ExtraConfig: &runtime.RawExtension{
				Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":10,"strategy":"header","key":"Authorization"}}`),
			},
		},
		Overrides: []v1alpha1.OperationOverride{
			{
				OperationID: "UploadOrder",
				Endpoint:    "/api/v1/orders",
			// Only specifies "every"; deep merge preserves the remaining default sub-fields.
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/ratelimit/router":{"every":"2s"}}`),
				},
			},
		},
	})

	// UploadOrder: deep merge preserves default sub-fields not in the override.
	// Only "every" is overwritten; max_rate, strategy, key survive from defaults.
	upload := entries["UploadOrder"]
	rl := extraConfigField(t, upload.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "2s" {
		t.Errorf("UploadOrder: expected every=2s, got %v", rl["every"])
	}
	if rl["max_rate"] != float64(10) {
		t.Errorf("UploadOrder: expected max_rate=10 (preserved from default), got %v", rl["max_rate"])
	}
	if rl["strategy"] != "header" {
		t.Errorf("UploadOrder: expected strategy=header (preserved from default), got %v", rl["strategy"])
	}
	if rl["key"] != "Authorization" {
		t.Errorf("UploadOrder: expected key=Authorization (preserved from default), got %v", rl["key"])
	}

	// Order: no override, so default sub-fields are all present
	order := entries["Order"]
	rl = extraConfigField(t, order.ExtraConfig, "qos/ratelimit/router")
	if rl["every"] != "1s" {
		t.Errorf("Order: expected default every=1s, got %v", rl["every"])
	}
	if rl["max_rate"] != float64(10) {
		t.Errorf("Order: expected default max_rate=10, got %v", rl["max_rate"])
	}
	if rl["strategy"] != "header" {
		t.Errorf("Order: expected default strategy=header, got %v", rl["strategy"])
	}
	if rl["key"] != "Authorization" {
		t.Errorf("Order: expected default key=Authorization, got %v", rl["key"])
	}
}
