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
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
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

func TestDereferenceParameters_UnresolvableRefStillFailsTheOperation(t *testing.T) {
	spec := []byte(`{"paths":{"/ok":{"get":{"operationId":"ok","responses":{"200":{"description":"OK"}}}},` +
		`"/bad":{"get":{"operationId":"bad","parameters":[{"$ref":"#/components/parameters/Nope"}],` +
		`"responses":{"200":{"description":"OK"}}}}}}`)
	out, _, err := DereferenceParameters(spec)
	if err != nil {
		t.Fatalf("DereferenceParameters: %v", err)
	}
	got := evaluateEmbedded(t, string(out))
	if len(got.Entries) != 1 || len(got.Failed) != 1 || got.Failed[0].OperationID != "bad" {
		t.Errorf("entries=%d failed=%+v, want 1 entry and operation bad failed", len(got.Entries), got.Failed)
	}
}

func TestDereferenceParameters_RefToPathsDoesNotMultiplyTheSpec(t *testing.T) {
	var ops []string
	for i := range 16 {
		ops = append(ops, fmt.Sprintf(`"/p%d":{"get":{"operationId":"op%d","parameters":[{"$ref":"#/paths"}],`+
			`"responses":{"200":{"description":"OK"}}}}`, i, i))
	}
	spec := []byte(`{"paths":{` + strings.Join(ops, ",") + `}}`)

	out, warnings, err := DereferenceParameters(spec)
	if err != nil {
		t.Fatalf("DereferenceParameters: %v", err)
	}
	if len(out) > 2*len(spec) {
		t.Fatalf("output grew from %d to %d bytes", len(spec), len(out))
	}
	if len(warnings) != 16 {
		t.Errorf("got %d warnings, want 16: %q", len(warnings), warnings)
	}
	if failed := evaluateEmbedded(t, string(out)).Failed; len(failed) != 16 {
		t.Errorf("got %d failed operations, want 16", len(failed))
	}
}

func TestDereferenceParameters_WarnsOnRefsToNonParameters(t *testing.T) {
	for name, target := range map[string]string{
		"info section":      `#/info`,
		"external chain":    `#/components/parameters/Remote`,
		"schema":            `#/components/schemas/Pet`,
		"named but no 'in'": `#/components/parameters/NoIn`,
	} {
		t.Run(name, func(t *testing.T) {
			spec := []byte(`{"info":{"name":"x","in":"y"},"paths":{"/a":{"get":{"parameters":[{"$ref":"` + target + `"}]}}},` +
				`"components":{"schemas":{"Pet":{"type":"object"}},"parameters":{` +
				`"Remote":{"$ref":"https://example.com/p.json#/Limit"},"NoIn":{"name":"n"}}}}`)
			out, warnings, err := DereferenceParameters(spec)
			if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], target) {
				t.Errorf("warnings = %q, err = %v", warnings, err)
			}
			if !bytes.Equal(out, spec) {
				t.Errorf("expected the spec unchanged, got %s", out)
			}
		})
	}
}

func TestDereferenceParameters_ResolvesParameterUnderComponentsSchemas(t *testing.T) {
	spec := []byte(`{"paths":{"/a":{"get":{"operationId":"a","parameters":[` +
		`{"$ref":"#/components/schemas/common_components_parameters_Limit"}],` +
		`"responses":{"200":{"description":"OK"}}}}},` +
		`"components":{"schemas":{"common_components_parameters_Limit":{"name":"limit","in":"query"}}}}`)
	out, warnings, err := DereferenceParameters(spec)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("DereferenceParameters: err=%v warnings=%v", err, warnings)
	}
	entries := evaluateEmbedded(t, string(out)).Entries
	if len(entries) != 1 || !slices.Equal(entries[0].InputQueryStrings, []string{"limit"}) {
		t.Errorf("entries = %+v, want one forwarding [limit]", entries)
	}
}

func TestDereferenceParameters_RejectsExpansionBeyondTheBodyLimit(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	var ops []string
	for i := range 12 {
		ops = append(ops, fmt.Sprintf(`"/p%d":{"get":{"parameters":[{"$ref":"#/components/parameters/Big"}]}}`, i))
	}
	spec := []byte(`{"paths":{` + strings.Join(ops, ",") + `},"components":{"parameters":{` +
		`"Big":{"name":"big","in":"query","description":"` + big + `"}}}}`)

	out, _, err := DereferenceParameters(spec)
	if err == nil {
		t.Fatalf("expected an error, got %d bytes of output", len(out))
	}
	if !bytes.Equal(out, spec) {
		t.Errorf("expected the input back on error, got %d bytes", len(out))
	}
}

func TestDereferenceParameters_PathLevelWarningNamesEveryOperation(t *testing.T) {
	spec := []byte(`{"paths":{"/pets":{"parameters":[{"$ref":"#/components/parameters/Nope"}],` +
		`"get":{"operationId":"listPets"}}}}`)
	_, warnings, err := DereferenceParameters(spec)
	if err != nil || len(warnings) != 1 ||
		!strings.Contains(warnings[0], `"#/components/parameters/Nope" in /pets`) ||
		!strings.Contains(warnings[0], "every operation on /pets") {
		t.Errorf("warnings = %q, err = %v", warnings, err)
	}
}

// resolveAndDereference runs the controller's spec preparation over a main
// spec and the documents its refs fetch, and returns the evaluated entries.
func resolveAndDereference(t *testing.T, main string, docs map[string]string) []v1alpha1.EndpointEntry {
	t.Helper()
	fetched := map[string][]byte{}
	for url, body := range docs {
		fetched[url] = []byte(body)
	}
	resolved, _, err := ResolveExternalRefs(context.Background(), []byte(main),
		"https://api.example.com/openapi.json", &stubFetcher{docs: fetched}, FetchSource{})
	if err != nil {
		t.Fatalf("ResolveExternalRefs: %v", err)
	}
	out, _, err := DereferenceParameters(resolved)
	if err != nil {
		t.Fatalf("DereferenceParameters: %v", err)
	}
	return evaluateEmbedded(t, string(out)).Entries
}

// An alias inside a fetched document names a component of that document, not
// of the main spec, whatever the main spec holds under the same name.
func TestDereferenceParameters_AliasInFetchedDocumentResolvesInThatDocument(t *testing.T) {
	main := `{"paths":{"/pets":{"get":{"operationId":"listPets","parameters":[
		{"$ref":"common.json#/components/parameters/Limit"}],"responses":{"200":{"description":"OK"}}}}},
		"components":{"parameters":{"PageLimit":{"name":"X-Page","in":"header"}}}}`
	common := `{"components":{"parameters":{
		"Limit":{"$ref":"#/components/parameters/PageLimit"},
		"PageLimit":{"name":"limit","in":"query"}}}}`

	entries := resolveAndDereference(t, main, map[string]string{"https://api.example.com/common.json": common})

	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if !slices.Equal(entries[0].InputQueryStrings, []string{"limit"}) {
		t.Errorf("inputQueryStrings = %v, want [limit]", entries[0].InputQueryStrings)
	}
	if slices.Contains(entries[0].InputHeaders, "X-Page") {
		t.Errorf("inputHeaders %v: forwards the main spec's X-Page", entries[0].InputHeaders)
	}
}
