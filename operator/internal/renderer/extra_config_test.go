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

import (
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestBuildBackendExtraConfig_NoPolicyNoInline(t *testing.T) {
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
	}
	ec := buildBackendExtraConfig(backend, nil, "default")
	if ec != nil {
		t.Errorf("expected nil, got %v", ec)
	}
}

func TestBuildBackendExtraConfig_PolicyRawOnly(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/raw-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				Raw: &runtime.RawExtension{
					Raw: []byte(`{"custom/namespace":{"key":"value"}}`),
				},
			},
		},
	}
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
		PolicyRef:  &v1alpha1.PolicyRef{Name: "raw-policy"},
	}
	ec := buildBackendExtraConfig(backend, policies, "default")
	if ec == nil {
		t.Fatal("expected non-nil extra_config")
	}
	if _, ok := ec["custom/namespace"]; !ok {
		t.Error("expected custom/namespace from raw policy")
	}
}

func TestBuildBackendExtraConfig_TypedFieldsOverrideRaw(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/mixed-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				Raw: &runtime.RawExtension{
					Raw: []byte(`{"qos/circuit-breaker":{"interval":1,"timeout":1,"max_errors":1}}`),
				},
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval:  60,
					Timeout:   10,
					MaxErrors: 5,
				},
			},
		},
	}
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
		PolicyRef:  &v1alpha1.PolicyRef{Name: "mixed-policy"},
	}
	ec := buildBackendExtraConfig(backend, policies, "default")
	cb := ec["qos/circuit-breaker"].(map[string]any)
	if cb["interval"] != 60 {
		t.Errorf("expected typed interval 60 to override raw, got %v", cb["interval"])
	}
}

func TestBuildBackendExtraConfig_InlineOverridesAll(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval: 60, Timeout: 10, MaxErrors: 5,
				},
			},
		},
	}
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
		PolicyRef:  &v1alpha1.PolicyRef{Name: "policy"},
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"qos/circuit-breaker":{"interval":999}}`),
		},
	}
	ec := buildBackendExtraConfig(backend, policies, "default")
	cb := ec["qos/circuit-breaker"].(map[string]any)
	// Inline completely replaces the policy key
	if cb["interval"] != float64(999) {
		t.Errorf("expected inline interval 999, got %v", cb["interval"])
	}
}

func TestBuildBackendExtraConfig_AllTypedFields(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/full-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval: 60, Timeout: 10, MaxErrors: 5, LogStatusChange: true,
				},
				RateLimit: &v1alpha1.RateLimitSpec{
					MaxRate: 100, Capacity: 50,
				},
				Cache: &v1alpha1.CacheSpec{Shared: true},
			},
		},
	}
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
		PolicyRef:  &v1alpha1.PolicyRef{Name: "full-policy"},
	}
	ec := buildBackendExtraConfig(backend, policies, "default")
	if _, ok := ec["qos/circuit-breaker"]; !ok {
		t.Error("expected qos/circuit-breaker")
	}
	rl := ec["qos/ratelimit/proxy"].(map[string]any)
	if rl["max_rate"] != 100 {
		t.Errorf("expected max_rate 100, got %v", rl["max_rate"])
	}
	if rl["capacity"] != 50 {
		t.Errorf("expected capacity 50, got %v", rl["capacity"])
	}
	cache := ec["qos/http-cache"].(map[string]any)
	if cache["shared"] != true {
		t.Error("expected shared true")
	}
}

func TestBuildBackendExtraConfig_MissingPolicyRef(t *testing.T) {
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
		PolicyRef:  &v1alpha1.PolicyRef{Name: "nonexistent"},
	}
	ec := buildBackendExtraConfig(backend, nil, "default")
	if ec != nil {
		t.Errorf("expected nil for missing policy, got %v", ec)
	}
}

func TestBuildEndpointExtraConfig_ValidJSON(t *testing.T) {
	raw := []byte(`{"auth/validator":{"alg":"RS256"},"validation/cel":[{"expression":"size(req.body) < 1000"}]}`)
	ec := buildEndpointExtraConfig(raw)
	if ec == nil {
		t.Fatal("expected non-nil extra_config")
	}
	if _, ok := ec["auth/validator"]; !ok {
		t.Error("expected auth/validator")
	}
}

func TestBuildEndpointExtraConfig_InvalidJSON(t *testing.T) {
	ec := buildEndpointExtraConfig([]byte(`{invalid`))
	if ec != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestBuildEndpointExtraConfig_Empty(t *testing.T) {
	ec := buildEndpointExtraConfig(nil)
	if ec != nil {
		t.Error("expected nil for nil input")
	}
	ec = buildEndpointExtraConfig([]byte(`{}`))
	if ec != nil {
		t.Error("expected nil for empty object")
	}
}

func TestBuildBackendExtraConfig_RateLimitNoCapacity(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/rl": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
			},
		},
	}
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
		PolicyRef:  &v1alpha1.PolicyRef{Name: "rl"},
	}
	ec := buildBackendExtraConfig(backend, policies, "default")
	rl := ec["qos/ratelimit/proxy"].(map[string]any)
	if _, ok := rl["capacity"]; ok {
		t.Error("expected no capacity key when zero")
	}
}
