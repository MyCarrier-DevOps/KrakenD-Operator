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
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestFlattenEndpoints_Empty(t *testing.T) {
	flat, conflicted, invalid := flattenEndpoints(nil, nil)
	if len(flat) != 0 || len(conflicted) != 0 || len(invalid) != 0 {
		t.Error("expected all empty for nil input")
	}
}

func TestFlattenEndpoints_SingleEndpoint(t *testing.T) {
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default", CreationTimestamp: metav1.Now()},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/api/v1/users", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:80"}, URLPattern: "/users"}}},
				},
			},
		},
	}
	flat, conflicted, invalid := flattenEndpoints(endpoints, nil)
	if len(flat) != 1 {
		t.Fatalf("expected 1 flat endpoint, got %d", len(flat))
	}
	if len(conflicted) != 0 || len(invalid) != 0 {
		t.Error("expected no conflicts or invalids")
	}
}

func TestFlattenEndpoints_NoConflictDifferentPaths(t *testing.T) {
	now := metav1.Now()
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default", CreationTimestamp: now},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/a", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://a:80"}, URLPattern: "/a"}}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default", CreationTimestamp: now},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/b", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://b:80"}, URLPattern: "/b"}}},
				},
			},
		},
	}
	flat, conflicted, _ := flattenEndpoints(endpoints, nil)
	if len(flat) != 2 {
		t.Fatalf("expected 2 flat endpoints, got %d", len(flat))
	}
	if len(conflicted) != 0 {
		t.Error("expected no conflicts for different paths")
	}
}

func TestFlattenEndpoints_SamePathDifferentMethod(t *testing.T) {
	now := metav1.Now()
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default", CreationTimestamp: now},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/api", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://a:80"}, URLPattern: "/a"}}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default", CreationTimestamp: now},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/api", Method: "POST", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://b:80"}, URLPattern: "/b"}}},
				},
			},
		},
	}
	flat, conflicted, _ := flattenEndpoints(endpoints, nil)
	if len(flat) != 2 {
		t.Fatalf("expected 2 flat endpoints, got %d", len(flat))
	}
	if len(conflicted) != 0 {
		t.Error("expected no conflicts for same path different method")
	}
}

func TestFlattenEndpoints_ConflictOldestWins(t *testing.T) {
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Minute))
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep-new", Namespace: "default", CreationTimestamp: later},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/dup", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://new:80"}, URLPattern: "/new"}}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep-old", Namespace: "default", CreationTimestamp: now},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/dup", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://old:80"}, URLPattern: "/old"}}},
				},
			},
		},
	}

	flat, conflicted, _ := flattenEndpoints(endpoints, nil)
	if len(flat) != 1 {
		t.Fatalf("expected 1 flat endpoint after conflict, got %d", len(flat))
	}
	if flat[0].Source.Name != "ep-old" {
		t.Errorf("expected oldest (ep-old) to win, got %s", flat[0].Source.Name)
	}
	if len(conflicted) != 1 {
		t.Fatalf("expected 1 conflicted, got %d", len(conflicted))
	}
}

func TestFlattenEndpoints_SortedOutput(t *testing.T) {
	now := metav1.Now()
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default", CreationTimestamp: now},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/z", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://z:80"}, URLPattern: "/z"}}},
					{Endpoint: "/a", Method: "POST", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://a:80"}, URLPattern: "/a"}}},
					{Endpoint: "/a", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://a:80"}, URLPattern: "/a"}}},
				},
			},
		},
	}

	flat, _, _ := flattenEndpoints(endpoints, nil)
	if len(flat) != 3 {
		t.Fatalf("expected 3 flat endpoints, got %d", len(flat))
	}
	if flat[0].Entry.Endpoint != "/a" || flat[0].Entry.Method != "GET" {
		t.Errorf("expected /a GET first, got %s %s", flat[0].Entry.Endpoint, flat[0].Entry.Method)
	}
	if flat[1].Entry.Endpoint != "/a" || flat[1].Entry.Method != "POST" {
		t.Errorf("expected /a POST second, got %s %s", flat[1].Entry.Endpoint, flat[1].Entry.Method)
	}
	if flat[2].Entry.Endpoint != "/z" {
		t.Errorf("expected /z last, got %s", flat[2].Entry.Endpoint)
	}
}

func TestFlattenEndpoints_InvalidPolicyRef(t *testing.T) {
	now := metav1.Now()
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "ep-bad", Namespace: "default", CreationTimestamp: now},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				Endpoints: []v1alpha1.EndpointEntry{
					{
						Endpoint: "/broken",
						Method:   "GET",
						Backends: []v1alpha1.BackendSpec{{
							Host:       []string{"http://svc:80"},
							URLPattern: "/broken",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "missing"},
						}},
					},
				},
			},
		},
	}
	_, _, invalid := flattenEndpoints(endpoints, nil)
	if len(invalid) != 1 {
		t.Fatalf("expected 1 invalid, got %d", len(invalid))
	}
}

func TestBuildEndpointJSON_AllFields(t *testing.T) {
	dur := metav1.Duration{Duration: 5 * time.Second}
	cc := int32(3)
	entry := v1alpha1.EndpointEntry{
		Endpoint:          "/api/v1/users",
		Method:            "GET",
		Timeout:           &dur,
		CacheTTL:          &dur,
		OutputEncoding:    "json",
		ConcurrentCalls:   &cc,
		InputHeaders:      []string{"X-Custom", "Authorization"},
		InputQueryStrings: []string{"page", "limit"},
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://users:8080"}, URLPattern: "/users", Method: "GET"},
		},
	}

	result := buildEndpointJSON(entry, nil, "default")
	if result["endpoint"] != "/api/v1/users" {
		t.Errorf("unexpected endpoint: %v", result["endpoint"])
	}
	if result["timeout"] != "5s" {
		t.Errorf("unexpected timeout: %v", result["timeout"])
	}
	if result["concurrent_calls"] != int32(3) {
		t.Errorf("unexpected concurrent_calls: %v", result["concurrent_calls"])
	}
	// Verify headers are sorted
	headers := result["input_headers"].([]string)
	if headers[0] != "Authorization" || headers[1] != "X-Custom" {
		t.Errorf("expected sorted headers, got %v", headers)
	}
}

func TestBuildEndpointJSON_WithExtraConfig(t *testing.T) {
	entry := v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/orders",
		Method:   "POST",
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://orders:8080"}, URLPattern: "/orders"},
		},
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"auth/validator":{"alg":"RS256"}}`),
		},
	}
	result := buildEndpointJSON(entry, nil, "default")
	ec, ok := result["extra_config"].(map[string]any)
	if !ok {
		t.Fatal("expected extra_config")
	}
	if _, ok := ec["auth/validator"]; !ok {
		t.Error("expected auth/validator in endpoint extra_config")
	}
}

func TestBuildBackendJSON_AllFields(t *testing.T) {
	boolTrue := true
	backend := v1alpha1.BackendSpec{
		Host:                []string{"http://b:80", "http://a:80"},
		URLPattern:          "/api",
		Method:              "POST",
		Encoding:            "json",
		SD:                  "static",
		SDScheme:            "https",
		DisableHostSanitize: &boolTrue,
		InputHeaders:        []string{"X-Custom", "Authorization"},
		InputQueryStrings:   []string{"page", "limit"},
		Allow:               []string{"id", "name", "email"},
		Deny:                []string{"secret", "internal_id"},
		Group:               "user_data",
		Target:              "data.user",
		IsCollection:        &boolTrue,
		Mapping:             map[string]string{"id": "user_id"},
	}
	result := buildBackendJSON(backend, nil, "default")
	// Hosts should be sorted
	hosts, ok := result["host"].([]string)
	if !ok {
		t.Fatal("expected host to be []string")
	}
	if hosts[0] != "http://a:80" || hosts[1] != "http://b:80" {
		t.Errorf("expected sorted hosts, got %v", hosts)
	}
	// Allow should be sorted
	allow, ok := result["allow"].([]string)
	if !ok {
		t.Fatal("expected allow to be []string")
	}
	if allow[0] != "email" {
		t.Errorf("expected sorted allow list, got %v", allow)
	}
	// Deny should be sorted
	deny, ok := result["deny"].([]string)
	if !ok {
		t.Fatal("expected deny to be []string")
	}
	if deny[0] != "internal_id" || deny[1] != "secret" {
		t.Errorf("expected sorted deny list, got %v", deny)
	}
	// SD fields
	if result["sd"] != "static" {
		t.Errorf("expected sd=static, got %v", result["sd"])
	}
	if result["sd_scheme"] != "https" {
		t.Errorf("expected sd_scheme=https, got %v", result["sd_scheme"])
	}
	// Disable host sanitize
	if result["disable_host_sanitize"] != true {
		t.Errorf("expected disable_host_sanitize=true, got %v", result["disable_host_sanitize"])
	}
	// Group and target
	if result["group"] != "user_data" {
		t.Errorf("expected group=user_data, got %v", result["group"])
	}
	if result["target"] != "data.user" {
		t.Errorf("expected target=data.user, got %v", result["target"])
	}
	// IsCollection
	if result["is_collection"] != true {
		t.Errorf("expected is_collection=true, got %v", result["is_collection"])
	}
	// Input headers/query strings should be sorted
	ih, ok := result["input_headers"].([]string)
	if !ok {
		t.Fatal("expected input_headers to be []string")
	}
	if ih[0] != "Authorization" || ih[1] != "X-Custom" {
		t.Errorf("expected sorted input_headers, got %v", ih)
	}
	iqs, ok := result["input_query_strings"].([]string)
	if !ok {
		t.Fatal("expected input_query_strings to be []string")
	}
	if iqs[0] != "limit" || iqs[1] != "page" {
		t.Errorf("expected sorted input_query_strings, got %v", iqs)
	}
}

func TestBuildBackendJSON_WithPolicy(t *testing.T) {
	policies := map[string]*v1alpha1.KrakenDBackendPolicy{
		"default/my-policy": {
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
					Interval:  60,
					Timeout:   10,
					MaxErrors: 3,
				},
			},
		},
	}
	backend := v1alpha1.BackendSpec{
		Host:       []string{"http://svc:80"},
		URLPattern: "/api",
		PolicyRef:  &v1alpha1.PolicyRef{Name: "my-policy"},
	}
	result := buildBackendJSON(backend, policies, "default")
	ec, ok := result["extra_config"].(map[string]any)
	if !ok {
		t.Fatal("expected extra_config from policy")
	}
	if _, ok := ec["qos/circuit-breaker"]; !ok {
		t.Error("expected qos/circuit-breaker from policy")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{3 * time.Second, "3s"},
		{20 * time.Second, "20s"},
		{60 * time.Second, "1m"},  // NOT "1m0s"
		{90 * time.Second, "90s"}, // NOT "1m30s"
		{120 * time.Second, "2m"}, // exact minutes
		{time.Hour, "1h"},         // exact hour
		{2 * time.Hour, "2h"},     // exact hours
		{500 * time.Millisecond, "500ms"},
		{100 * time.Microsecond, "100µs"},
		{50 * time.Nanosecond, "50ns"},
		{1500 * time.Millisecond, "1500ms"}, // 1.5s → no exact seconds, use ms
	}
	for _, tt := range tests {
		got := formatDuration(tt.input)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildEndpointJSON_60sTimeout(t *testing.T) {
	dur := metav1.Duration{Duration: 60 * time.Second}
	entry := v1alpha1.EndpointEntry{
		Endpoint: "/api/v1/quote/rate",
		Method:   "POST",
		Timeout:  &dur,
		Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://quote:8080"}, URLPattern: "/api/v1/rate"},
		},
	}
	result := buildEndpointJSON(entry, nil, "default")
	if result["timeout"] != "1m" {
		t.Errorf("expected timeout '1m', got %q", result["timeout"])
	}
}
