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

package autoconfig

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestDereferenceParameters_InlinesLocalRefs(t *testing.T) {
	spec := []byte(`{"paths":{"/pets":{
		"parameters":[{"$ref":"#/components/parameters/Tenant"}],
		"get":{"operationId":"listPets","parameters":[{"$ref":"#/components/parameters/Limit"},
			{"name":"X-Trace","in":"header"}],"responses":{"200":{"description":"OK"}}}}},
		"components":{"parameters":{
			"Limit":{"name":"limit","in":"query"},
			"Tenant":{"$ref":"#/components/parameters/TenantHeader"},
			"TenantHeader":{"name":"X-Tenant","in":"header"}}}}`)

	out, warnings, err := DereferenceParameters(spec)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("DereferenceParameters: err=%v warnings=%v", err, warnings)
	}
	entries := evaluateEmbedded(t, string(out)).Entries
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if !slices.Equal(entry.InputQueryStrings, []string{"limit"}) {
		t.Errorf("inputQueryStrings = %v, want [limit]", entry.InputQueryStrings)
	}
	for _, h := range []string{"X-Trace", "X-Tenant"} {
		if !slices.Contains(entry.InputHeaders, h) {
			t.Errorf("inputHeaders %v: missing %s", entry.InputHeaders, h)
		}
	}
}

func TestDereferenceParameters_ReportsUnresolvableRef(t *testing.T) {
	spec := []byte(`{"paths":{"/pets":{"get":{"parameters":[{"$ref":"#/components/parameters/Nope"}]}}}}`)

	out, warnings, err := DereferenceParameters(spec)
	if err != nil {
		t.Fatalf("DereferenceParameters: %v", err)
	}
	if !bytes.Equal(out, spec) {
		t.Errorf("expected the spec unchanged, got %s", out)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"#/components/parameters/Nope" in GET /pets`) {
		t.Errorf("warnings = %q", warnings)
	}
}
