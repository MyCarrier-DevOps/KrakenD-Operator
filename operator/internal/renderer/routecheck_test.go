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
