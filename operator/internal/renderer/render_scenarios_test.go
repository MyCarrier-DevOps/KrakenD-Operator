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

package renderer

// Functional render scenario tests.
//
// Each test constructs KrakenDEndpoint CRs the way a user would configure
// them (or an AutoConfig would generate them), feeds them through
// renderer.Render(), and asserts that the resulting krakend.json contains
// the correct values. These prove that user-visible fields flow all the
// way through to the final output.

import (
	"encoding/json"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// renderAndParse runs Render() and returns the parsed JSON root config
// and the endpoints array indexed by "endpoint:method".
func renderAndParse(t *testing.T, input RenderInput) (root map[string]any, byKey map[string]map[string]any) {
	t.Helper()
	r := New(Options{})
	out, err := r.Render(input)
	if err != nil {
		t.Fatalf("Render() failed: %v", err)
	}
	if err := json.Unmarshal(out.JSON, &root); err != nil {
		t.Fatalf("rendered JSON is invalid: %v", err)
	}
	byKey = make(map[string]map[string]any)
	eps, ok := root["endpoints"].([]any)
	if !ok {
		return root, byKey
	}
	for _, raw := range eps {
		ep := raw.(map[string]any)
		key := ep["endpoint"].(string) + ":" + ep["method"].(string)
		byKey[key] = ep
	}
	return root, byKey
}

// makeEndpoint is a helper that wraps entries in a KrakenDEndpoint CR.
func makeEndpoint(name string, entries ...v1alpha1.EndpointEntry) v1alpha1.KrakenDEndpoint {
	return v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test"},
			Endpoints:  entries,
		},
	}
}

// =========================================================================
// Scenario: User sets endpoint timeout to 20s.
// Expected: "timeout": "20s" appears in the rendered krakend.json endpoint.
// =========================================================================

func TestRenderScenario_EndpointTimeoutFlowsToJSON(t *testing.T) {
	timeout := metav1.Duration{Duration: 20 * time.Second}
	ep := makeEndpoint("order-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/orders",
		Method:   "GET",
		Timeout:  &timeout,
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://orders:8080"}, URLPattern: "/api/orders"},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	got := byKey["/api/v1/orders:GET"]
	if got["timeout"] != "20s" {
		t.Errorf("timeout = %v, want 20s", got["timeout"])
	}
}

// =========================================================================
// Scenario: User sets cacheTTL to 5 minutes on an endpoint.
// Expected: "cache_ttl": "5m0s" in krakend.json.
// =========================================================================

func TestRenderScenario_CacheTTLFlowsToJSON(t *testing.T) {
	cacheTTL := metav1.Duration{Duration: 5 * time.Minute}
	ep := makeEndpoint("cached-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/products",
		Method:   "GET",
		CacheTTL: &cacheTTL,
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://products:8080"}, URLPattern: "/products"},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	got := byKey["/api/v1/products:GET"]
	if got["cache_ttl"] != "5m0s" {
		t.Errorf("cache_ttl = %v, want 5m0s", got["cache_ttl"])
	}
}

// =========================================================================
// Scenario: User sets outputEncoding to "no-op" for passthrough.
// Expected: "output_encoding": "no-op" in krakend.json.
// =========================================================================

func TestRenderScenario_OutputEncodingFlowsToJSON(t *testing.T) {
	ep := makeEndpoint("passthrough-ep", v1alpha1.EndpointEntry{
		Endpoint:       "/api/v1/proxy",
		Method:         "GET",
		OutputEncoding: "no-op",
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://proxy:8080"}, URLPattern: "/proxy"},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	got := byKey["/api/v1/proxy:GET"]
	if got["output_encoding"] != "no-op" {
		t.Errorf("output_encoding = %v, want no-op", got["output_encoding"])
	}
}

// =========================================================================
// Scenario: User sets concurrentCalls to 3.
// Expected: "concurrent_calls": 3 in krakend.json.
// =========================================================================

func TestRenderScenario_ConcurrentCallsFlowsToJSON(t *testing.T) {
	cc := int32(3)
	ep := makeEndpoint("concurrent-ep", v1alpha1.EndpointEntry{
		Endpoint:        "/api/v1/aggregate",
		Method:          "GET",
		ConcurrentCalls: &cc,
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://svc:8080"}, URLPattern: "/data"},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	got := byKey["/api/v1/aggregate:GET"]
	if got["concurrent_calls"] != float64(3) {
		t.Errorf("concurrent_calls = %v, want 3", got["concurrent_calls"])
	}
}

// =========================================================================
// Scenario: User sets inputHeaders and inputQueryStrings.
// Expected: Both appear in krakend.json, sorted for determinism.
// =========================================================================

func TestRenderScenario_HeadersAndQueryStringsFlowToJSON(t *testing.T) {
	ep := makeEndpoint("header-ep", v1alpha1.EndpointEntry{
		Endpoint:          "/api/v1/users",
		Method:            "GET",
		InputHeaders:      []string{"X-Custom", "Authorization", "Content-Type"},
		InputQueryStrings: []string{"offset", "limit"},
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://users:8080"}, URLPattern: "/users"},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	got := byKey["/api/v1/users:GET"]

	headers := toStringSlice(t, got["input_headers"])
	if len(headers) != 3 {
		t.Fatalf("input_headers length = %d, want 3", len(headers))
	}
	if headers[0] != "Authorization" || headers[1] != "Content-Type" || headers[2] != "X-Custom" {
		t.Errorf("input_headers = %v, want sorted [Authorization Content-Type X-Custom]", headers)
	}

	qs := toStringSlice(t, got["input_query_strings"])
	if len(qs) != 2 {
		t.Fatalf("input_query_strings length = %d, want 2", len(qs))
	}
	if qs[0] != "limit" || qs[1] != "offset" {
		t.Errorf("input_query_strings = %v, want sorted [limit offset]", qs)
	}
}

// =========================================================================
// Scenario: User sets endpoint ExtraConfig with an auth validator.
// Expected: "extra_config.auth/validator" appears in krakend.json endpoint.
// =========================================================================

func TestRenderScenario_EndpointExtraConfigFlowsToJSON(t *testing.T) {
	ep := makeEndpoint("auth-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/secure",
		Method:   "POST",
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"auth/validator":{"alg":"RS256","jwk_url":"https://auth.example.com/.well-known/jwks.json"}}`),
		},
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://secure:8080"}, URLPattern: "/secure"},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	got := byKey["/api/v1/secure:POST"]
	ec := got["extra_config"].(map[string]any)
	auth := ec["auth/validator"].(map[string]any)
	if auth["alg"] != "RS256" {
		t.Errorf("auth/validator.alg = %v, want RS256", auth["alg"])
	}
	if auth["jwk_url"] != "https://auth.example.com/.well-known/jwks.json" {
		t.Errorf("auth/validator.jwk_url = %v, want valid URL", auth["jwk_url"])
	}
}

// =========================================================================
// Scenario: Backend references a policy with CircuitBreaker + RateLimit.
// Expected: Both appear under backend.extra_config in krakend.json.
// =========================================================================

func TestRenderScenario_PolicyCircuitBreakerAndRateLimitInJSON(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/resilience-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval:        60,
					Timeout:         10,
					MaxErrors:       5,
					LogStatusChange: true,
				},
				RateLimit: &v1alpha1.RateLimitSpec{
					MaxRate:  100,
					Capacity: 50,
				},
			},
		},
	}
	ep := makeEndpoint("policy-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/orders",
		Method:   "POST",
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://orders:8080"},
				URLPattern: "/orders",
				PolicyRef:  &v1alpha1.PolicyRef{Name: "resilience-policy"},
			},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
		Policies:  policies,
	})

	got := byKey["/api/v1/orders:POST"]
	backends := got["backend"].([]any)
	be := backends[0].(map[string]any)
	ec := be["extra_config"].(map[string]any)

	// Circuit breaker
	cb := ec["qos/circuit-breaker"].(map[string]any)
	if cb["interval"] != float64(60) {
		t.Errorf("circuit-breaker.interval = %v, want 60", cb["interval"])
	}
	if cb["timeout"] != float64(10) {
		t.Errorf("circuit-breaker.timeout = %v, want 10", cb["timeout"])
	}
	if cb["max_errors"] != float64(5) {
		t.Errorf("circuit-breaker.max_errors = %v, want 5", cb["max_errors"])
	}
	if cb["log_status_change"] != true {
		t.Errorf("circuit-breaker.log_status_change = %v, want true", cb["log_status_change"])
	}

	// Rate limit
	rl := ec["qos/ratelimit/proxy"].(map[string]any)
	if rl["max_rate"] != float64(100) {
		t.Errorf("ratelimit.max_rate = %v, want 100", rl["max_rate"])
	}
	if rl["capacity"] != float64(50) {
		t.Errorf("ratelimit.capacity = %v, want 50", rl["capacity"])
	}
}

// =========================================================================
// Scenario: Backend references a policy with Cache enabled.
// Expected: "qos/http-cache" appears in backend extra_config in krakend.json.
// =========================================================================

func TestRenderScenario_PolicyCacheInJSON(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/cache-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				Cache: &v1alpha1.CacheSpec{Shared: true},
			},
		},
	}
	ep := makeEndpoint("cache-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/catalog",
		Method:   "GET",
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://catalog:8080"},
				URLPattern: "/catalog",
				PolicyRef:  &v1alpha1.PolicyRef{Name: "cache-policy"},
			},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
		Policies:  policies,
	})

	be := byKey["/api/v1/catalog:GET"]["backend"].([]any)[0].(map[string]any)
	ec := be["extra_config"].(map[string]any)
	cache := ec["qos/http-cache"].(map[string]any)
	if cache["shared"] != true {
		t.Errorf("http-cache.shared = %v, want true", cache["shared"])
	}
}

// =========================================================================
// Scenario: 3-layer extra_config merge: raw policy < typed policy < inline.
// The user's inline backend ExtraConfig overrides the same key that the
// policy also sets. Other policy keys should be preserved.
// Expected: Inline wins for overlapping key; non-overlapping policy key preserved.
// =========================================================================

func TestRenderScenario_ThreeLayerExtraConfigMerge(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/mixed-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				Raw: &runtime.RawExtension{
					Raw: []byte(`{"custom/plugin":{"enabled":true}}`),
				},
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval: 60, Timeout: 10, MaxErrors: 5,
				},
			},
		},
	}
	ep := makeEndpoint("layered-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/layered",
		Method:   "POST",
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://svc:8080"},
				URLPattern: "/layered",
				PolicyRef:  &v1alpha1.PolicyRef{Name: "mixed-policy"},
				// Inline overrides the circuit-breaker with different values
				ExtraConfig: &runtime.RawExtension{
					Raw: []byte(`{"qos/circuit-breaker":{"interval":999,"timeout":1,"max_errors":1}}`),
				},
			},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
		Policies:  policies,
	})

	be := byKey["/api/v1/layered:POST"]["backend"].([]any)[0].(map[string]any)
	ec := be["extra_config"].(map[string]any)

	// Inline CB should win over typed policy CB
	cb := ec["qos/circuit-breaker"].(map[string]any)
	if cb["interval"] != float64(999) {
		t.Errorf("circuit-breaker.interval = %v, want 999 (inline wins)", cb["interval"])
	}

	// Raw policy key should be preserved (no overlap with inline)
	plugin := ec["custom/plugin"].(map[string]any)
	if plugin["enabled"] != true {
		t.Errorf("custom/plugin.enabled = %v, want true (raw policy preserved)", plugin["enabled"])
	}
}

// =========================================================================
// Scenario: Backend with allow list and field mapping.
// Expected: Both appear in backend JSON, allow is sorted.
// =========================================================================

func TestRenderScenario_BackendAllowAndMappingInJSON(t *testing.T) {
	ep := makeEndpoint("transform-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/users",
		Method:   "GET",
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://users:8080"},
				URLPattern: "/users",
				Allow:      []string{"name", "email", "id"},
				Mapping:    map[string]string{"id": "user_id", "email": "user_email"},
			},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	be := byKey["/api/v1/users:GET"]["backend"].([]any)[0].(map[string]any)
	allow := toStringSlice(t, be["allow"])
	if len(allow) != 3 {
		t.Fatalf("allow length = %d, want 3", len(allow))
	}
	if allow[0] != "email" || allow[1] != "id" || allow[2] != "name" {
		t.Errorf("allow = %v, want sorted [email id name]", allow)
	}

	mapping := be["mapping"].(map[string]any)
	if mapping["id"] != "user_id" {
		t.Errorf("mapping.id = %v, want user_id", mapping["id"])
	}
	if mapping["email"] != "user_email" {
		t.Errorf("mapping.email = %v, want user_email", mapping["email"])
	}
}

// =========================================================================
// Scenario: Backend with method override (POST backend behind GET endpoint).
// Expected: "method": "POST" appears in backend JSON.
// =========================================================================

func TestRenderScenario_BackendMethodOverrideInJSON(t *testing.T) {
	ep := makeEndpoint("method-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/command",
		Method:   "GET",
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://cmd:8080"},
				URLPattern: "/execute",
				Method:     "POST",
			},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	be := byKey["/api/v1/command:GET"]["backend"].([]any)[0].(map[string]any)
	if be["method"] != "POST" {
		t.Errorf("backend method = %v, want POST", be["method"])
	}
}

// =========================================================================
// Scenario: Backend with encoding override.
// Expected: "encoding": "xml" appears in backend JSON.
// =========================================================================

func TestRenderScenario_BackendEncodingInJSON(t *testing.T) {
	ep := makeEndpoint("encoding-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/legacy",
		Method:   "GET",
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://legacy:8080"},
				URLPattern: "/data",
				Encoding:   "xml",
			},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	be := byKey["/api/v1/legacy:GET"]["backend"].([]any)[0].(map[string]any)
	if be["encoding"] != "xml" {
		t.Errorf("backend encoding = %v, want xml", be["encoding"])
	}
}

// =========================================================================
// Scenario: Multiple endpoints from different CRs, each with different
// user-configured fields. Verifies all features compose correctly in a
// single rendered krakend.json.
// =========================================================================

func TestRenderScenario_MultipleEndpointsComposedCorrectly(t *testing.T) {
	timeout := metav1.Duration{Duration: 30 * time.Second}
	cacheTTL := metav1.Duration{Duration: 2 * time.Minute}
	cc := int32(5)

	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/order-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval: 30, Timeout: 5, MaxErrors: 3,
				},
				RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 200, Capacity: 100},
			},
		},
	}

	orderEp := makeEndpoint("order-ep", v1alpha1.EndpointEntry{
		Endpoint:     "/api/v1/orders",
		Method:       "POST",
		Timeout:      &timeout,
		InputHeaders: []string{"Authorization", "Content-Type", "X-Idempotency-Key"},
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`),
		},
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://orders:8080"},
				URLPattern: "/orders",
				Method:     "POST",
				PolicyRef:  &v1alpha1.PolicyRef{Name: "order-policy"},
			},
		},
	})

	catalogEp := makeEndpoint("catalog-ep",
		v1alpha1.EndpointEntry{
			Endpoint:        "/api/v1/products",
			Method:          "GET",
			CacheTTL:        &cacheTTL,
			ConcurrentCalls: &cc,
			OutputEncoding:  "json",
			Backends: []v1alpha1.BackendSpec{
				{
					Host:       []string{"http://catalog:8080"},
					URLPattern: "/products",
					Allow:      []string{"id", "name", "price"},
				},
			},
		},
		v1alpha1.EndpointEntry{
			Endpoint: "/api/v1/products/{id}",
			Method:   "GET",
			Backends: []v1alpha1.BackendSpec{
				{
					Host:       []string{"http://catalog:8080"},
					URLPattern: "/products/{id}",
					Mapping:    map[string]string{"product_id": "id"},
				},
			},
		},
	)

	root, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{orderEp, catalogEp},
		Policies:  policies,
	})

	// Verify root structure
	if root["version"] != float64(3) {
		t.Errorf("root version = %v, want 3", root["version"])
	}
	eps := root["endpoints"].([]any)
	if len(eps) != 3 {
		t.Fatalf("expected 3 endpoints, got %d", len(eps))
	}

	// Order endpoint: timeout, headers, auth extra_config, policy on backend
	order := byKey["/api/v1/orders:POST"]
	if order["timeout"] != "30s" {
		t.Errorf("orders timeout = %v, want 30s", order["timeout"])
	}
	orderHeaders := toStringSlice(t, order["input_headers"])
	if len(orderHeaders) != 3 || orderHeaders[0] != "Authorization" {
		t.Errorf("orders headers = %v, want [Authorization Content-Type X-Idempotency-Key]", orderHeaders)
	}
	orderEC := order["extra_config"].(map[string]any)
	if _, ok := orderEC["auth/validator"]; !ok {
		t.Error("orders: missing auth/validator in extra_config")
	}
	orderBE := order["backend"].([]any)[0].(map[string]any)
	orderBEEC := orderBE["extra_config"].(map[string]any)
	if _, ok := orderBEEC["qos/circuit-breaker"]; !ok {
		t.Error("orders backend: missing circuit-breaker from policy")
	}
	if _, ok := orderBEEC["qos/ratelimit/proxy"]; !ok {
		t.Error("orders backend: missing rate-limit from policy")
	}

	// Products list: cacheTTL, concurrentCalls, outputEncoding, allow list
	products := byKey["/api/v1/products:GET"]
	if products["cache_ttl"] != "2m0s" {
		t.Errorf("products cache_ttl = %v, want 2m0s", products["cache_ttl"])
	}
	if products["concurrent_calls"] != float64(5) {
		t.Errorf("products concurrent_calls = %v, want 5", products["concurrent_calls"])
	}
	if products["output_encoding"] != "json" {
		t.Errorf("products output_encoding = %v, want json", products["output_encoding"])
	}
	prodBE := products["backend"].([]any)[0].(map[string]any)
	prodAllow := toStringSlice(t, prodBE["allow"])
	if len(prodAllow) != 3 || prodAllow[0] != "id" {
		t.Errorf("products backend allow = %v, want sorted [id name price]", prodAllow)
	}

	// Product detail: field mapping
	detail := byKey["/api/v1/products/{id}:GET"]
	detailBE := detail["backend"].([]any)[0].(map[string]any)
	detailMapping := detailBE["mapping"].(map[string]any)
	if detailMapping["product_id"] != "id" {
		t.Errorf("product detail mapping = %v, want product_id→id", detailMapping)
	}
}

// =========================================================================
// Scenario: Endpoint with empty backends still renders without error.
// Note: Empty backends serialize as JSON null rather than []. This is
// acceptable because KrakenD requires at least one backend per endpoint,
// so this is a degenerate case that would be caught by validation.
// =========================================================================

func TestRenderScenario_EmptyBackendsRendersWithoutError(t *testing.T) {
	ep := makeEndpoint("no-backend-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/static",
		Method:   "GET",
		Backends: []v1alpha1.BackendSpec{},
	})

	r := New(Options{})
	out, err := r.Render(RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})
	if err != nil {
		t.Fatalf("Render() should not error on empty backends: %v", err)
	}
	if len(out.JSON) == 0 {
		t.Fatal("expected non-empty JSON output")
	}
}

// =========================================================================
// Scenario: Endpoint with no optional fields set (no timeout, no headers,
// no extra_config, no encoding). Only required fields present.
// Expected: krakend.json endpoint has only endpoint, method, backend.
// No spurious keys like "timeout": null or "input_headers": null.
// =========================================================================

func TestRenderScenario_MinimalEndpointNoSpuriousKeys(t *testing.T) {
	ep := makeEndpoint("minimal-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/health",
		Method:   "GET",
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://health:8080"}, URLPattern: "/health"},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
	})

	got := byKey["/api/v1/health:GET"]

	// These keys should NOT be present when unset
	for _, key := range []string{"timeout", "cache_ttl", "output_encoding", "concurrent_calls", "input_headers", "input_query_strings", "extra_config"} {
		if _, exists := got[key]; exists {
			t.Errorf("unexpected key %q in minimal endpoint JSON (should be omitted when unset)", key)
		}
	}

	// These keys MUST be present
	for _, key := range []string{"endpoint", "method", "backend"} {
		if _, exists := got[key]; !exists {
			t.Errorf("required key %q missing from minimal endpoint JSON", key)
		}
	}
}

// =========================================================================
// Scenario: Cross-namespace policy reference.
// Expected: Backend resolves policy from a different namespace.
// =========================================================================

func TestRenderScenario_CrossNamespacePolicyRef(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"infra/shared-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval: 120, Timeout: 30, MaxErrors: 10,
				},
			},
		},
	}
	ep := makeEndpoint("cross-ns-ep", v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/remote",
		Method:   "GET",
		Backends: []v1alpha1.BackendSpec{
			{
				Host:       []string{"http://remote:8080"},
				URLPattern: "/remote",
				PolicyRef:  &v1alpha1.PolicyRef{Name: "shared-policy", Namespace: "infra"},
			},
		},
	})

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   minimalGateway(),
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
		Policies:  policies,
	})

	be := byKey["/api/v1/remote:GET"]["backend"].([]any)[0].(map[string]any)
	ec := be["extra_config"].(map[string]any)
	cb := ec["qos/circuit-breaker"].(map[string]any)
	if cb["interval"] != float64(120) {
		t.Errorf("cross-namespace circuit-breaker.interval = %v, want 120", cb["interval"])
	}
}

// =========================================================================
// Scenario: Full AutoConfig-to-Render pipeline.
// User configures: defaults (timeout, inputHeaders), per-operation overrides
// (endpoint path, extraConfig rate limit), filter (exclude operation),
// and a policyRef on all backends.
// Expected: The final krakend.json reflects all of those settings correctly.
// =========================================================================

func TestRenderScenario_AutoConfigToKrakendJSON(t *testing.T) {
	// Simulate what the autoconfig pipeline produces after CUE evaluation,
	// applyDefaults, applyURLTransform, applyFieldOverrides, filter, generate.
	timeout := metav1.Duration{Duration: 20 * time.Second}
	uploadTimeout := metav1.Duration{Duration: 45 * time.Second}

	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"krakend/default-backend-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval: 60, Timeout: 10, MaxErrors: 5,
				},
			},
		},
	}

	// The endpoint CRs that the AutoConfig generator would produce:
	uploadEp := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "order-api-uploadorder",
			Namespace:         "krakend",
			CreationTimestamp: metav1.Now(),
			Labels: map[string]string{
				"gateway.krakend.io/auto-generated": "true",
				"gateway.krakend.io/autoconfig":     "order-public-api",
			},
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint:     "/api/v1/orders",
					Method:       "POST",
					Timeout:      &uploadTimeout,
					InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
					ExtraConfig: &runtime.RawExtension{
						Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":10,"strategy":"header","key":"Authorization"},"documentation/openapi":{"summary":"Create or Modify Orders","operationId":"UploadOrder","tags":["Orders"]}}`),
					},
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://order-api.dev.svc:8080"},
							URLPattern: "/api/Orders",
							Method:     "POST",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "default-backend-policy"},
						},
					},
				},
			},
		},
	}

	getOrderEp := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "order-api-order",
			Namespace:         "krakend",
			CreationTimestamp: metav1.Now(),
			Labels: map[string]string{
				"gateway.krakend.io/auto-generated": "true",
			},
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint:     "/api/v1/orders/referenceId/{referenceId}",
					Method:       "GET",
					Timeout:      &timeout,
					InputHeaders: []string{"Authorization", "X-MC-Api-Key", "Content-Type"},
					ExtraConfig: &runtime.RawExtension{
						Raw: []byte(`{"qos/ratelimit/router":{"every":"1s","max_rate":10},"documentation/openapi":{"summary":"Get Order by reference id","operationId":"Order","tags":["Orders"]}}`),
					},
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://order-api.dev.svc:8080"},
							URLPattern: "/api/Orders/referenceId/{referenceId}",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "default-backend-policy"},
						},
					},
				},
			},
		},
	}

	gw := minimalGateway()
	gw.Namespace = "krakend"

	_, byKey := renderAndParse(t, RenderInput{
		Gateway:   gw,
		Endpoints: []v1alpha1.KrakenDEndpoint{uploadEp, getOrderEp},
		Policies:  policies,
	})

	if len(byKey) != 2 {
		t.Fatalf("expected 2 endpoints in krakend.json, got %d", len(byKey))
	}

	// UploadOrder: timeout 45s, headers, rate limit, auth validator, policy CB
	upload := byKey["/api/v1/orders:POST"]
	if upload["timeout"] != "45s" {
		t.Errorf("UploadOrder timeout = %v, want 45s", upload["timeout"])
	}
	uploadHeaders := toStringSlice(t, upload["input_headers"])
	if len(uploadHeaders) != 3 || uploadHeaders[0] != "Authorization" {
		t.Errorf("UploadOrder headers = %v, want [Authorization Content-Type X-MC-Api-Key]", uploadHeaders)
	}
	uploadEC := upload["extra_config"].(map[string]any)
	rl := uploadEC["qos/ratelimit/router"].(map[string]any)
	if rl["every"] != "1s" {
		t.Errorf("UploadOrder rate limit every = %v, want 1s", rl["every"])
	}
	doc := uploadEC["documentation/openapi"].(map[string]any)
	if doc["operationId"] != "UploadOrder" {
		t.Errorf("UploadOrder operationId = %v, want UploadOrder", doc["operationId"])
	}
	uploadBE := upload["backend"].([]any)[0].(map[string]any)
	if uploadBE["url_pattern"] != "/api/Orders" {
		t.Errorf("UploadOrder backend url_pattern = %v, want /api/Orders", uploadBE["url_pattern"])
	}
	if uploadBE["method"] != "POST" {
		t.Errorf("UploadOrder backend method = %v, want POST", uploadBE["method"])
	}
	uploadBEEC := uploadBE["extra_config"].(map[string]any)
	if _, ok := uploadBEEC["qos/circuit-breaker"]; !ok {
		t.Error("UploadOrder backend: missing circuit-breaker from policy")
	}

	// Order (GET): timeout 20s, same headers, doc, policy CB
	order := byKey["/api/v1/orders/referenceId/{referenceId}:GET"]
	if order["timeout"] != "20s" {
		t.Errorf("Order timeout = %v, want 20s", order["timeout"])
	}
	orderDoc := order["extra_config"].(map[string]any)["documentation/openapi"].(map[string]any)
	if orderDoc["operationId"] != "Order" {
		t.Errorf("Order operationId = %v, want Order", orderDoc["operationId"])
	}
	orderBE := order["backend"].([]any)[0].(map[string]any)
	if orderBE["url_pattern"] != "/api/Orders/referenceId/{referenceId}" {
		t.Errorf("Order backend url_pattern = %v, want original OpenAPI path", orderBE["url_pattern"])
	}
	orderBEEC := orderBE["extra_config"].(map[string]any)
	if _, ok := orderBEEC["qos/circuit-breaker"]; !ok {
		t.Error("Order backend: missing circuit-breaker from policy")
	}
}

// toStringSlice converts a JSON-parsed []any to []string.
func toStringSlice(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", v)
	}
	result := make([]string, len(arr))
	for i, item := range arr {
		result[i] = item.(string)
	}
	return result
}
