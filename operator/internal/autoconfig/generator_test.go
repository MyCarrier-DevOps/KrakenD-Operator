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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGenerator_BasicGeneration(t *testing.T) {
	g := NewGenerator()
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ac", Namespace: "default"},
	}
	entries := []v1alpha1.EndpointEntry{
		{Endpoint: "/api/users", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/users"}}},
	}
	opIDs := map[string]string{"/api/users:GET": "listUsers"}

	out, err := g.Generate(context.Background(), GenerateInput{
		AutoConfig:   ac,
		Entries:      entries,
		OperationIDs: opIDs,
		GatewayRef:   v1alpha1.GatewayRef{Name: "my-gw"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(out.Endpoints))
	}

	ep := out.Endpoints[0]
	if ep.Name != "my-ac-listusers" {
		t.Errorf("expected my-ac-listusers, got %s", ep.Name)
	}
	if ep.Namespace != "default" {
		t.Errorf("expected default namespace, got %s", ep.Namespace)
	}
	if ep.Labels["gateway.krakend.io/auto-generated"] != "true" {
		t.Error("missing auto-generated label")
	}
	if ep.Labels["gateway.krakend.io/autoconfig"] != "my-ac" {
		t.Error("missing autoconfig label")
	}
	if ep.Spec.GatewayRef.Name != "my-gw" {
		t.Errorf("expected gatewayRef my-gw, got %s", ep.Spec.GatewayRef.Name)
	}
}

func TestGenerator_NameWithoutOperationID(t *testing.T) {
	g := NewGenerator()
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
	}
	entries := []v1alpha1.EndpointEntry{
		{Endpoint: "/api/users/{id}", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/users"}}},
	}

	out, err := g.Generate(context.Background(), GenerateInput{
		AutoConfig:   ac,
		Entries:      entries,
		OperationIDs: map[string]string{},
		GatewayRef:   v1alpha1.GatewayRef{Name: "gw"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Endpoints[0].Name != "ac-get-api-users-id" {
		t.Errorf("expected ac-get-api-users-id, got %s", out.Endpoints[0].Name)
	}
}

func TestGenerator_DuplicateOperationID(t *testing.T) {
	g := NewGenerator()
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
	}
	entries := []v1alpha1.EndpointEntry{
		{Endpoint: "/v1/users", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/users"}}},
		{Endpoint: "/v2/users", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/users"}}},
	}
	opIDs := map[string]string{
		"/v1/users:GET": "listUsers",
		"/v2/users:GET": "listUsers",
	}

	out, err := g.Generate(context.Background(), GenerateInput{
		AutoConfig:   ac,
		Entries:      entries,
		OperationIDs: opIDs,
		GatewayRef:   v1alpha1.GatewayRef{Name: "gw"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Endpoints) != 1 {
		t.Errorf("expected 1 endpoint (duplicate skipped), got %d", len(out.Endpoints))
	}
	if out.SkippedOperations != 1 {
		t.Errorf("expected 1 skipped, got %d", out.SkippedOperations)
	}
	if len(out.Duplicates) != 1 || out.Duplicates[0] != "listUsers" {
		t.Errorf("expected duplicate listUsers, got %v", out.Duplicates)
	}
}

func TestGenerator_MultipleEntries(t *testing.T) {
	g := NewGenerator()
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
	}
	entries := sampleEntries()
	opIDs := sampleOperationIDs()

	out, err := g.Generate(context.Background(), GenerateInput{
		AutoConfig:   ac,
		Entries:      entries,
		OperationIDs: opIDs,
		GatewayRef:   v1alpha1.GatewayRef{Name: "gw"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Endpoints) != 5 {
		t.Errorf("expected 5 endpoints, got %d", len(out.Endpoints))
	}
	if out.SkippedOperations != 0 {
		t.Errorf("expected 0 skipped, got %d", out.SkippedOperations)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"listUsers", "listusers"},
		{"get_user_by_id", "get-user-by-id"},
		{"GetUserById", "getuserbyid"},
		{"---trim---", "trim"},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.input); got != tt.expected {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"/api/users", "api-users"},
		{"/api/users/{id}", "api-users-id"},
		{"/api/orders/{orderId}/items", "api-orders-orderid-items"},
	}
	for _, tt := range tests {
		if got := sanitizePath(tt.input); got != tt.expected {
			t.Errorf("sanitizePath(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
