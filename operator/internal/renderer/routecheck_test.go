/*
Copyright 2026 The KrakenD Operator Authors.

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
	"strings"
	"testing"
)

func TestRouteConflicts_ParameterClashNamesBothEndpoints(t *testing.T) {
	doc := `{"version":3,"endpoints":[
		{"endpoint":"/users/{id}","method":"GET"},
		{"endpoint":"/users/{userId}/orders","method":"GET"}]}`

	lines, err := routeConflicts([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "- at '/endpoints/1/endpoint': ") ||
		!strings.HasPrefix(lines[1], "- at '/endpoints/0/endpoint': ") {
		t.Fatalf("lines = %q, want endpoint 1 (refused) then endpoint 0 (clashes with it)", lines)
	}
	if !strings.Contains(lines[0], "conflicts with existing wildcard ':id'") {
		t.Errorf("line = %q, want gin's refusal", lines[0])
	}
}

func TestRouteConflicts_MirrorsTheRuntimeRouter(t *testing.T) {
	tests := []struct {
		name, doc string
		refused   string // substring of the first line; "" means no lines
	}{
		{"static beside parameter", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/static","method":"GET"}]}`, ""},
		{"trailing slash is distinct", `{"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/a/","method":"GET"}]}`, ""},
		{"methods have separate trees", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/{name}","method":"POST"}]}`, ""},
		{"parameter beside static prefix", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/{x}/b","method":"GET"}]}`, ""},
		{"mid-segment braces are literal", `{"endpoints":[{"endpoint":"/a/b{id}","method":"GET"}]}`, ""},
		{"suffix after a parameter", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/{id}.json","method":"GET"}]}`, "conflicts with existing wildcard"},
		{"exact duplicate", `{"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/a","method":"GET"}]}`, "handlers are already registered"},
		{"double slash duplicate", `{"endpoints":[{"endpoint":"/a//b","method":"GET"},{"endpoint":"/a/b","method":"GET"}]}`, "handlers are already registered"},
		{"unnamed wildcard", `{"endpoints":[{"endpoint":"/a/*","method":"GET"}]}`, "wildcards must be named"},
		{"custom health path", `{"extra_config":{"router":{"health_path":"/healthz"}},"endpoints":[{"endpoint":"/healthz","method":"GET"}]}`, "the gateway's own route"},
		{"health path is GET only", `{"extra_config":{"router":{"health_path":"/healthz"}},"endpoints":[{"endpoint":"/healthz","method":"POST"}]}`, ""},
		{"health disabled", `{"extra_config":{"router":{"disable_health":true}},"endpoints":[{"endpoint":"/__health","method":"GET"}]}`, ""},
		{"auto options joins methods", `{"extra_config":{"router":{"auto_options":true}},"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/{name}","method":"POST"}]}`, "conflicts with existing wildcard"},
		{"auto options registers one route per path", `{"extra_config":{"router":{"auto_options":true}},"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/a","method":"POST"}]}`, ""},
		{"auto options cleans the path first", `{"extra_config":{"router":{"auto_options":true}},"endpoints":[{"endpoint":"a","method":"GET"},{"endpoint":"/a","method":"POST"}]}`, ""},
		{"echo beside root parameter", `{"echo_endpoint":true,"endpoints":[{"endpoint":"/{x}","method":"GET"}]}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := routeConflicts([]byte(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			if tt.refused == "" {
				if len(lines) != 0 {
					t.Errorf("lines = %q, want none", lines)
				}
				return
			}
			if len(lines) == 0 || !strings.Contains(lines[0], tt.refused) {
				t.Errorf("lines = %q, want one containing %q", lines, tt.refused)
			}
		})
	}
}

func TestRouteConflicts_ARefusalOnItsOwnBlamesNoNeighbour(t *testing.T) {
	doc := `{"extra_config":{"router":{"disable_health":true}},"endpoints":[
		{"endpoint":"/ok","method":"GET"},
		{"endpoint":"/a/*","method":"GET"}]}`

	lines, err := routeConflicts([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "- at '/endpoints/1/endpoint': ") {
		t.Errorf("lines = %q, want one line blaming endpoint 1, not the unrelated /ok", lines)
	}
}
