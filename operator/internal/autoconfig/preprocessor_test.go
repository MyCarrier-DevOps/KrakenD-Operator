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
	"encoding/json"
	"strings"
	"testing"
)

func TestStripServers_RootAndOverrides(t *testing.T) {
	input := []byte(`{
		"openapi": "3.0.0",
		"info": {"title": "t", "version": "1"},
		"servers": [{"url": "https://upstream.example.com"}],
		"paths": {
			"/things": {
				"servers": [{"url": "https://path-override.example.com"}],
				"get": {
					"servers": [{"url": "https://op-override.example.com"}],
					"responses": {"200": {"description": "ok"}}
				}
			}
		},
		"components": {"schemas": {"X": {"type": "object"}}}
	}`)

	out, err := StripServers(input)
	if err != nil {
		t.Fatalf("StripServers returned error: %v", err)
	}

	if strings.Contains(string(out), "upstream.example.com") ||
		strings.Contains(string(out), "path-override.example.com") ||
		strings.Contains(string(out), "op-override.example.com") {
		t.Fatalf("expected all server URLs to be stripped, got: %s", string(out))
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if _, present := decoded["servers"]; present {
		t.Fatalf("root servers key should be removed")
	}
	if _, present := decoded["info"]; !present {
		t.Fatalf("info should be preserved")
	}
	if _, present := decoded["components"]; !present {
		t.Fatalf("components should be preserved")
	}

	paths := decoded["paths"].(map[string]any)
	pathItem := paths["/things"].(map[string]any)
	if _, present := pathItem["servers"]; present {
		t.Fatalf("path-item servers should be removed")
	}
	op := pathItem["get"].(map[string]any)
	if _, present := op["servers"]; present {
		t.Fatalf("operation servers should be removed")
	}
	if _, present := op["responses"]; !present {
		t.Fatalf("operation responses should be preserved")
	}
}

func TestStripServers_NoServers(t *testing.T) {
	input := []byte(`{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{}}`)
	out, err := StripServers(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), `"openapi":"3.0.0"`) {
		t.Fatalf("expected spec to round-trip, got: %s", string(out))
	}
}

func TestStripServers_YAMLInput(t *testing.T) {
	input := []byte("openapi: 3.0.0\ninfo:\n  title: t\n  version: '1'\nservers:\n  - url: https://upstream\npaths: {}\n")
	out, err := StripServers(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(out), "upstream") {
		t.Fatalf("expected servers stripped, got: %s", string(out))
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}

func TestStripServers_InvalidInputReturnsOriginal(t *testing.T) {
	input := []byte("not-json-not-yaml: : :")
	out, err := StripServers(input)
	if err == nil {
		t.Fatalf("expected error for invalid input")
	}
	if string(out) != string(input) {
		t.Fatalf("expected original input returned on error")
	}
}
