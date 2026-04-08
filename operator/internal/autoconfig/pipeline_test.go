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

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestEmbeddedCUE_FullPipeline verifies the embedded CUE definitions produce
// EndpointEntry objects that flow through the Generator into KrakenDEndpoint CRs,
// and then through the Renderer into valid krakend.json.
func TestEmbeddedCUE_FullPipeline(t *testing.T) {
	// --- Stage 1: Load embedded CUE definitions ---
	defs, err := EmbeddedCUEDefinitions()
	if err != nil {
		t.Fatalf("loading embedded CUE defs: %v", err)
	}

	// Realistic OpenAPI spec with multiple paths, methods, parameters,
	// request bodies, and responses.
	specJSON := []byte(`{
		"openapi": "3.0.3",
		"info": {"title": "User Service", "version": "1.0.0"},
		"paths": {
			"/api/v1/users": {
				"get": {
					"operationId": "listUsers",
					"summary": "List all users",
					"description": "Returns a paginated list of users",
					"tags": ["users"],
					"parameters": [
						{"name": "limit", "in": "query", "required": false, "description": "Max results"},
						{"name": "offset", "in": "query", "required": false},
						{"name": "X-Request-ID", "in": "header"}
					],
					"responses": {
						"200": {
							"description": "Successful response",
							"content": {
								"application/json": {
									"schema": {"$ref": "#/components/schemas/UserList"}
								}
							}
						}
					}
				},
				"post": {
					"operationId": "createUser",
					"summary": "Create a user",
					"tags": ["users"],
					"requestBody": {
						"content": {
							"application/json": {
								"schema": {"$ref": "#/components/schemas/CreateUserRequest"}
							}
						}
					},
					"responses": {
						"201": {
							"description": "User created",
							"content": {
								"application/json": {
									"schema": {"$ref": "#/components/schemas/User"}
								}
							}
						}
					}
				}
			},
			"/api/v1/users/{id}": {
				"parameters": [
					{"name": "id", "in": "path", "required": true, "description": "User ID"}
				],
				"get": {
					"operationId": "getUser",
					"summary": "Get user by ID",
					"tags": ["users"],
					"responses": {
						"200": {
							"description": "User found",
							"content": {
								"application/json": {
									"schema": {"$ref": "#/components/schemas/User"}
								}
							}
						},
						"404": {"description": "User not found"}
					}
				},
				"put": {
					"operationId": "updateUser",
					"summary": "Update user",
					"tags": ["users"],
					"requestBody": {
						"content": {
							"application/json": {
								"schema": {"$ref": "#/components/schemas/UpdateUserRequest"}
							}
						}
					}
				},
				"delete": {
					"operationId": "deleteUser",
					"tags": ["users"]
				}
			},
			"/api/v1/health": {
				"get": {
					"operationId": "healthCheck",
					"tags": ["system"],
					"authorization": false
				}
			}
		}
	}`)

	// --- Stage 2: CUE Evaluation ---
	eval := NewCUEEvaluator()
	cueOutput, err := eval.Evaluate(context.Background(), CUEInput{
		SpecData:    specJSON,
		SpecFormat:  v1alpha1.SpecFormatJSON,
		DefaultDefs: defs,
		CustomDefs: map[string]string{
			"host.cue": `_defaultHost: "http://user-service.default.svc.cluster.local:8080"`,
		},
		Environment: "prod",
		ServiceName: "_spec",
	})
	if err != nil {
		t.Fatalf("CUE evaluation failed: %v", err)
	}

	// Should produce 6 endpoints: listUsers, createUser, getUser, updateUser, deleteUser, healthCheck
	if len(cueOutput.Entries) != 6 {
		t.Fatalf("expected 6 entries from CUE, got %d", len(cueOutput.Entries))
	}

	// Verify operationIDs were extracted
	if len(cueOutput.OperationIDs) != 6 {
		t.Errorf("expected 6 operationIDs, got %d: %v", len(cueOutput.OperationIDs), cueOutput.OperationIDs)
	}

	// --- Stage 3: Generator ---
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "main-gateway"},
		},
	}
	gen := NewGenerator()
	genOutput, err := gen.Generate(context.Background(), GenerateInput{
		AutoConfig:     ac,
		Entries:        cueOutput.Entries,
		OperationIDs:   cueOutput.OperationIDs,
		GatewayRefName: ac.Spec.GatewayRef.Name,
	})
	if err != nil {
		t.Fatalf("Generator failed: %v", err)
	}

	if len(genOutput.Endpoints) != 6 {
		t.Fatalf("expected 6 generated endpoints, got %d", len(genOutput.Endpoints))
	}
	if genOutput.SkippedOperations != 0 {
		t.Errorf("expected 0 skipped operations, got %d", genOutput.SkippedOperations)
	}

	// Verify generated CRs have correct metadata
	for _, ep := range genOutput.Endpoints {
		if ep.Labels["gateway.krakend.io/auto-generated"] != "true" {
			t.Errorf("endpoint %s missing auto-generated label", ep.Name)
		}
		if ep.Labels["gateway.krakend.io/autoconfig"] != "user-service" {
			t.Errorf("endpoint %s has wrong autoconfig label: %s", ep.Name, ep.Labels["gateway.krakend.io/autoconfig"])
		}
		if ep.Spec.GatewayRef.Name != "main-gateway" {
			t.Errorf("endpoint %s has wrong gatewayRef: %s", ep.Name, ep.Spec.GatewayRef.Name)
		}
	}

	// --- Stage 4: Renderer ---
	// Convert generated endpoints to the format the renderer expects
	var krakendEndpoints []v1alpha1.KrakenDEndpoint
	for _, ep := range genOutput.Endpoints {
		ep.CreationTimestamp = metav1.Now()
		krakendEndpoints = append(krakendEndpoints, *ep)
	}

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "main-gateway", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}

	r := renderer.New(renderer.Options{})
	renderOutput, err := r.Render(renderer.RenderInput{
		Gateway:   gw,
		Endpoints: krakendEndpoints,
		Policies:  map[string]*v1alpha1.KrakenDBackendPolicy{},
	})
	if err != nil {
		t.Fatalf("Renderer failed: %v", err)
	}

	// --- Stage 5: Validate the output JSON ---
	if len(renderOutput.JSON) == 0 {
		t.Fatal("renderer produced empty JSON")
	}
	if renderOutput.Checksum == "" {
		t.Fatal("renderer produced empty checksum")
	}

	// Parse the output to validate its structure
	var krakendConfig map[string]interface{}
	if err := json.Unmarshal(renderOutput.JSON, &krakendConfig); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Verify root structure
	if krakendConfig["version"] != float64(3) {
		t.Errorf("expected version 3, got %v", krakendConfig["version"])
	}

	endpoints, ok := krakendConfig["endpoints"].([]interface{})
	if !ok {
		t.Fatalf("expected endpoints array, got %T", krakendConfig["endpoints"])
	}
	if len(endpoints) != 6 {
		t.Fatalf("expected 6 endpoints in krakend.json, got %d", len(endpoints))
	}

	// Build a lookup by endpoint+method for verification
	byKey := map[string]map[string]interface{}{}
	for _, e := range endpoints {
		ep := e.(map[string]interface{})
		key := ep["endpoint"].(string) + ":" + ep["method"].(string)
		byKey[key] = ep
	}

	// Verify /api/v1/users GET
	usersGet, ok := byKey["/api/v1/users:GET"]
	if !ok {
		t.Fatal("missing /api/v1/users:GET in krakend.json")
	}

	// Check timeout rendered correctly
	if usersGet["timeout"] != "3s" {
		t.Errorf("expected timeout '3s', got %v", usersGet["timeout"])
	}

	// Check backends
	backends := usersGet["backend"].([]interface{})
	if len(backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(backends))
	}
	backend := backends[0].(map[string]interface{})
	if backend["url_pattern"] != "/api/v1/users" {
		t.Errorf("expected url_pattern /api/v1/users, got %v", backend["url_pattern"])
	}

	// Verify host from custom CUE definitions
	hosts := backend["host"].([]interface{})
	if len(hosts) != 1 || hosts[0] != "http://user-service.default.svc.cluster.local:8080" {
		t.Errorf("expected custom host, got %v", hosts)
	}

	// Check input_headers include auth headers
	headers := usersGet["input_headers"].([]interface{})
	headerSet := map[string]bool{}
	for _, h := range headers {
		headerSet[h.(string)] = true
	}
	for _, expected := range []string{"Authorization", "Content-Type", "X-MC-Api-Key", "X-Request-ID"} {
		if !headerSet[expected] {
			t.Errorf("missing header %s in krakend.json, got %v", expected, headers)
		}
	}

	// Check input_query_strings
	queryStrings := usersGet["input_query_strings"].([]interface{})
	qsSet := map[string]bool{}
	for _, qs := range queryStrings {
		qsSet[qs.(string)] = true
	}
	if !qsSet["limit"] || !qsSet["offset"] {
		t.Errorf("expected limit and offset in query strings, got %v", queryStrings)
	}

	// Check extra_config has rate limiting and OpenAPI documentation
	extraConfig, ok := usersGet["extra_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected extra_config in endpoint, got %T", usersGet["extra_config"])
	}
	if _, ok := extraConfig["qos/ratelimit/router"]; !ok {
		t.Error("expected qos/ratelimit/router in extra_config")
	}
	docConfig, ok := extraConfig["documentation/openapi"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected documentation/openapi in extra_config")
	}
	if docConfig["operation_id"] != "listUsers" {
		t.Errorf("expected operation_id listUsers, got %v", docConfig["operation_id"])
	}
	if docConfig["summary"] != "List all users" {
		t.Errorf("expected summary 'List all users', got %v", docConfig["summary"])
	}

	// Verify healthCheck endpoint has no auth headers (authorization: false)
	healthGet, ok := byKey["/api/v1/health:GET"]
	if !ok {
		t.Fatal("missing /api/v1/health:GET in krakend.json")
	}
	_, hasHeaders := healthGet["input_headers"]
	if hasHeaders {
		healthHeaders := healthGet["input_headers"].([]interface{})
		hSet := map[string]bool{}
		for _, h := range healthHeaders {
			hSet[h.(string)] = true
		}
		if hSet["Authorization"] {
			t.Error("healthCheck should not forward Authorization header (authorization: false)")
		}
	}

	// Verify /api/v1/users POST has backend with method POST
	usersPost, ok := byKey["/api/v1/users:POST"]
	if !ok {
		t.Fatal("missing /api/v1/users:POST in krakend.json")
	}
	postBackends := usersPost["backend"].([]interface{})
	postBackend := postBackends[0].(map[string]interface{})
	if postBackend["method"] != "POST" {
		t.Errorf("expected backend method POST, got %v", postBackend["method"])
	}

	// Verify DELETE endpoint exists
	if _, ok := byKey["/api/v1/users/{id}:DELETE"]; !ok {
		t.Error("missing /api/v1/users/{id}:DELETE in krakend.json")
	}

	t.Logf("Pipeline produced valid krakend.json with %d endpoints, checksum=%s", len(endpoints), renderOutput.Checksum)
}
