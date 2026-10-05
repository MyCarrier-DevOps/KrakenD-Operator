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
	"context"
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

func TestDereferenceParameters_LeavesSpecWithoutRefsByteIdentical(t *testing.T) {
	spec := []byte(`{"paths": {"/pets": {"get": {"parameters": [{"name": "limit", "in": "query"}]}}}}`)
	out, warnings, err := DereferenceParameters(spec)
	if err != nil || len(warnings) != 0 || !bytes.Equal(out, spec) {
		t.Errorf("got %s, %v, %v; want the input unchanged", out, warnings, err)
	}
}

func TestDereferenceParameters_ReportsRefCycle(t *testing.T) {
	spec := []byte(`{"paths":{"/a":{"get":{"parameters":[{"$ref":"#/components/parameters/A"}]}}},
		"components":{"parameters":{"A":{"$ref":"#/components/parameters/B"},"B":{"$ref":"#/components/parameters/A"}}}}`)
	_, warnings, err := DereferenceParameters(spec)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "chained $refs") {
		t.Errorf("warnings = %q, err = %v", warnings, err)
	}
}

func TestDereferenceParameters_ResolvesRefRewrittenFromExternalDocument(t *testing.T) {
	main := []byte(`{"paths":{"/pets":{"get":{"operationId":"listPets",` +
		`"parameters":[{"$ref":"common.json#/components/parameters/Limit"}],` +
		`"responses":{"200":{"description":"OK"}}}}}}`)
	common := []byte(`{"components":{"parameters":{"Limit":{"name":"limit","in":"query"}}}}`)
	fetcher := &stubFetcher{docs: map[string][]byte{"https://api.example.com/common.json": common}}
	resolved, _, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.json", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	out, warnings, err := DereferenceParameters(resolved)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("DereferenceParameters: err=%v warnings=%v", err, warnings)
	}
	entries := evaluateEmbedded(t, string(out)).Entries
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if qs := entries[0].InputQueryStrings; !slices.Equal(qs, []string{"limit"}) {
		t.Errorf("inputQueryStrings = %v, want [limit]", qs)
	}
}

func TestDereferenceParameters_IsDeterministic(t *testing.T) {
	var paths []string
	for _, p := range []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g", "/h"} {
		paths = append(paths, `"`+p+`":{"get":{"parameters":[{"$ref":"#/components/parameters/Limit"},`+
			`{"$ref":"#/components/parameters/Missing"}]}}`)
	}
	spec := []byte(`{"paths":{` + strings.Join(paths, ",") + `},` +
		`"components":{"parameters":{"Limit":{"name":"limit","in":"query","schema":{"type":"integer"}}}}}`)

	firstOut, firstWarnings, err := DereferenceParameters(spec)
	if err != nil {
		t.Fatalf("DereferenceParameters: %v", err)
	}
	if !slices.IsSorted(firstWarnings) || len(firstWarnings) != 8 {
		t.Errorf("warnings = %q, want 8 in path order", firstWarnings)
	}
	for range 20 {
		out, warnings, err := DereferenceParameters(spec)
		if err != nil || !bytes.Equal(out, firstOut) || !slices.Equal(warnings, firstWarnings) {
			t.Fatalf("a repeat pass differs: err=%v\nout=%s\nwant=%s\nwarnings=%q", err, out, firstOut, warnings)
		}
	}
	again, _, err := DereferenceParameters(firstOut)
	if err != nil || !bytes.Equal(again, firstOut) {
		t.Errorf("a second pass over the output changed it: err=%v", err)
	}
}
