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

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func sampleEntries() []v1alpha1.EndpointEntry {
	return []v1alpha1.EndpointEntry{
		{Endpoint: "/api/users", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://users-svc"}, URLPattern: "/users"}}},
		{Endpoint: "/api/users", Method: "POST", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://users-svc"}, URLPattern: "/users"}}},
		{Endpoint: "/api/orders", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://orders-svc"}, URLPattern: "/orders"}}},
		{Endpoint: "/internal/health", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://health-svc"}, URLPattern: "/health"}}},
		{Endpoint: "/internal/debug/pprof", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://debug-svc"}, URLPattern: "/pprof"}}},
	}
}

func sampleTags() map[string][]string {
	return map[string][]string{
		"/api/users:GET":            {"users", "public"},
		"/api/users:POST":           {"users", "admin"},
		"/api/orders:GET":           {"orders", "public"},
		"/internal/health:GET":      {"internal"},
		"/internal/debug/pprof:GET": {"internal", "debug"},
	}
}

func sampleOperationIDs() map[string]string {
	return map[string]string{
		"/api/users:GET":            "listUsers",
		"/api/users:POST":           "createUser",
		"/api/orders:GET":           "listOrders",
		"/internal/health:GET":      "healthCheck",
		"/internal/debug/pprof:GET": "debugPprof",
	}
}

func TestFilter_NoRules(t *testing.T) {
	f := NewFilter()
	entries := sampleEntries()
	result := f.Apply(entries, nil, nil, v1alpha1.FilterSpec{})
	if len(result) != len(entries) {
		t.Errorf("expected %d entries, got %d", len(entries), len(result))
	}
}

func TestFilter_IncludePaths(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		IncludePaths: []string{"/api/users"},
	})
	if len(result) != 2 {
		t.Errorf("expected 2, got %d", len(result))
	}
}

func TestFilter_IncludePathGlob(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		IncludePaths: []string{"/internal/*"},
	})
	if len(result) != 2 {
		t.Errorf("expected 2, got %d", len(result))
	}
}

func TestFilter_ExcludePaths(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		ExcludePaths: []string{"/internal/*"},
	})
	if len(result) != 3 {
		t.Errorf("expected 3, got %d", len(result))
	}
}

func TestFilter_IncludeTags(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		IncludeTags: []string{"public"},
	})
	if len(result) != 2 {
		t.Errorf("expected 2, got %d", len(result))
	}
}

func TestFilter_ExcludeTags(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		ExcludeTags: []string{"internal"},
	})
	if len(result) != 3 {
		t.Errorf("expected 3, got %d", len(result))
	}
}

func TestFilter_IncludeMethods(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		IncludeMethods: []string{"GET"},
	})
	if len(result) != 4 {
		t.Errorf("expected 4, got %d", len(result))
	}
}

func TestFilter_ExcludeOperationIds(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		ExcludeOperationIds: []string{"healthCheck", "debugPprof"},
	})
	if len(result) != 3 {
		t.Errorf("expected 3, got %d", len(result))
	}
}

func TestFilter_Combined(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		IncludePaths: []string{"/api/*"},
		ExcludeTags:  []string{"admin"},
	})
	if len(result) != 2 {
		t.Errorf("expected 2, got %d", len(result))
	}
}

func TestFilter_MethodCaseInsensitive(t *testing.T) {
	f := NewFilter()
	result := f.Apply(sampleEntries(), sampleTags(), sampleOperationIDs(), v1alpha1.FilterSpec{
		IncludeMethods: []string{"get"},
	})
	if len(result) != 4 {
		t.Errorf("expected 4, got %d", len(result))
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		path, pattern string
		expected      bool
	}{
		{"/api/users", "/api/users", true},
		{"/api/users", "/api/*", true},
		{"/internal/health", "/internal/*", true},
		{"/api/users", "/internal/*", false},
		{"/internal", "/internal/*", true},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.path, tt.pattern); got != tt.expected {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.expected)
		}
	}
}
