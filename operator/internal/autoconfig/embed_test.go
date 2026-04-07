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
)

func TestEmbeddedCUEDefinitions_ReturnsFiles(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("expected at least one embedded CUE definition file")
	}
	if _, ok := defs["defaults.cue"]; !ok {
		t.Error("expected defaults.cue in embedded definitions")
	}
}

func TestEmbeddedCUEDefinitions_EvaluatesWithSpec(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading embedded defs: %v", err)
	}

	specJSON := []byte(`{
		"paths": {
			"/api/users": {
				"get": {
					"operationId": "listUsers",
					"tags": ["users"],
					"parameters": [
						{"name": "limit", "in": "query"},
						{"name": "X-Request-ID", "in": "header"}
					]
				},
				"post": {
					"operationId": "createUser",
					"tags": ["users"]
				}
			},
			"/api/health": {
				"get": {
					"operationId": "healthCheck"
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
		t.Fatalf("CUE evaluation failed: %v", err)
	}

	if len(out.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(out.Entries))
	}

	// Verify entries by building a lookup map
	byKey := map[string]v1alpha1.EndpointEntry{}
	for _, e := range out.Entries {
		byKey[e.Endpoint+":"+e.Method] = e
	}

	// Check /api/users GET
	usersGet, ok := byKey["/api/users:GET"]
	if !ok {
		t.Fatal("missing /api/users:GET entry")
	}
	if len(usersGet.Backends) != 1 {
		t.Errorf("expected 1 backend, got %d", len(usersGet.Backends))
	}
	if usersGet.Backends[0].URLPattern != "/api/users" {
		t.Errorf("expected urlPattern /api/users, got %s", usersGet.Backends[0].URLPattern)
	}
	if usersGet.Backends[0].Method != "GET" {
		t.Errorf("expected backend method GET, got %s", usersGet.Backends[0].Method)
	}

	// Check timeout is set
	if usersGet.Timeout == nil || usersGet.Timeout.Duration != 3*time.Second {
		t.Errorf("expected timeout 3s, got %v", usersGet.Timeout)
	}

	// Check query string params extracted
	hasLimit := false
	for _, qs := range usersGet.InputQueryStrings {
		if qs == "limit" {
			hasLimit = true
		}
	}
	if !hasLimit {
		t.Errorf("expected 'limit' in inputQueryStrings, got %v", usersGet.InputQueryStrings)
	}

	// Check header params include auth headers, Content-Type, and X-Request-ID
	headerSet := map[string]bool{}
	for _, h := range usersGet.InputHeaders {
		headerSet[h] = true
	}
	for _, expected := range []string{"Authorization", "X-MC-Api-Key", "Content-Type", "X-Request-ID"} {
		if !headerSet[expected] {
			t.Errorf("expected %s in inputHeaders, got %v", expected, usersGet.InputHeaders)
		}
	}

	// Check extraConfig contains rate limiting and documentation
	if usersGet.ExtraConfig == nil || usersGet.ExtraConfig.Raw == nil {
		t.Fatal("expected extraConfig to be set")
	}
	var ec map[string]interface{}
	if err := json.Unmarshal(usersGet.ExtraConfig.Raw, &ec); err != nil {
		t.Fatalf("unmarshalling extraConfig: %v", err)
	}
	if _, ok := ec["qos/ratelimit/router"]; !ok {
		t.Error("expected qos/ratelimit/router in extraConfig")
	}
	if docCfg, ok := ec["documentation/openapi"]; ok {
		doc := docCfg.(map[string]interface{})
		if opID, ok := doc["operation_id"]; ok {
			if opID != "listUsers" {
				t.Errorf("expected operation_id listUsers, got %v", opID)
			}
		}
	} else {
		t.Error("expected documentation/openapi in extraConfig")
	}

	// Check operationId extraction
	if opID, ok := out.OperationIDs["/api/users:GET"]; !ok || opID != "listUsers" {
		t.Errorf("expected operationId listUsers, got %v", out.OperationIDs)
	}

	// Check tags extraction
	if tags, ok := out.Tags["/api/users:GET"]; !ok || len(tags) != 1 || tags[0] != "users" {
		t.Errorf("expected tags [users], got %v", out.Tags["/api/users:GET"])
	}

	// Check /api/health GET has no query strings
	healthGet := byKey["/api/health:GET"]
	if len(healthGet.InputQueryStrings) != 0 {
		t.Errorf("expected no query strings for healthCheck, got %v", healthGet.InputQueryStrings)
	}
}

func TestEmbeddedCUEDefinitions_EnvironmentInjection(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading embedded defs: %v", err)
	}

	specJSON := []byte(`{"paths": {"/test": {"get": {"operationId": "test"}}}}`)

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		Environment: "prod",
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("CUE evaluation with environment injection failed: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
}

func TestEmbeddedCUEDefinitions_SkipsNonMethodKeys(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading embedded defs: %v", err)
	}

	// OpenAPI path item with path-level parameters alongside a method
	specJSON := []byte(`{
		"paths": {
			"/api/items/{id}": {
				"parameters": [
					{"name": "id", "in": "path", "required": true}
				],
				"get": {
					"operationId": "getItem",
					"parameters": [
						{"name": "fields", "in": "query"}
					]
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
		t.Fatalf("CUE evaluation failed: %v", err)
	}

	// Should produce exactly 1 entry (GET), not a second for "parameters"
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
	if out.Entries[0].Method != "GET" {
		t.Errorf("expected method GET, got %s", out.Entries[0].Method)
	}

	// The path-level "id" parameter should NOT appear in query strings
	// (it's "in": "path"), but the operation-level "fields" should
	hasFields := false
	for _, qs := range out.Entries[0].InputQueryStrings {
		if qs == "fields" {
			hasFields = true
		}
		if qs == "id" {
			t.Error("path parameter 'id' should not appear in inputQueryStrings")
		}
	}
	if !hasFields {
		t.Errorf("expected 'fields' in inputQueryStrings, got %v", out.Entries[0].InputQueryStrings)
	}
}

func TestEmbeddedCUEDefinitions_CustomDefsOverride(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading embedded defs: %v", err)
	}

	// Custom CUE that overrides _defaultHost
	customDefs := map[string]string{
		"custom.cue": `_defaultHost: "http://my-service.svc.cluster.local"`,
	}

	specJSON := []byte(`{"paths": {"/api/test": {"get": {"operationId": "test"}}}}`)

	eval := NewCUEEvaluator()
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		CustomDefs:  customDefs,
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("CUE evaluation with custom defs failed: %v", err)
	}
	if len(out.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out.Entries))
	}
	if out.Entries[0].Backends[0].Host[0] != "http://my-service.svc.cluster.local" {
		t.Errorf("expected custom host, got %s", out.Entries[0].Backends[0].Host[0])
	}
}

func TestEmbeddedCUEDefinitions_URLTransformHostMapping(t *testing.T) {
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading embedded defs: %v", err)
	}

	specJSON := []byte(`{
		"paths": {
			"/api/users": {
				"get": {"operationId": "listUsers", "tags": ["users"]},
				"post": {"operationId": "createUser", "tags": ["users"]}
			}
		}
	}`)

	eval := NewCUEEvaluator()

	// The embedded CUE defaults _defaultHost to "http://localhost".
	// URLTransform.HostMapping replaces it with a Kubernetes service URL.
	out, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		ServiceName: "_spec",
		URLTransform: &v1alpha1.URLTransformSpec{
			HostMapping: []v1alpha1.HostMappingEntry{
				{From: "http://localhost", To: "http://user-api.production.svc.cluster.local:8080"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CUE evaluation with URLTransform failed: %v", err)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out.Entries))
	}

	// All backends should have the mapped host
	for _, entry := range out.Entries {
		for _, be := range entry.Backends {
			if be.Host[0] != "http://user-api.production.svc.cluster.local:8080" {
				t.Errorf("endpoint %s:%s backend host not mapped: got %s",
					entry.Endpoint, entry.Method, be.Host[0])
			}
		}
	}
}
