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
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
)

type stubFetcher struct {
	docs map[string][]byte
	hits map[string]int
}

func (s *stubFetcher) Fetch(_ context.Context, source FetchSource) (*FetchResult, error) {
	if s.hits == nil {
		s.hits = map[string]int{}
	}
	s.hits[source.URL]++
	body, ok := s.docs[source.URL]
	if !ok {
		return nil, fmt.Errorf("not found: %s", source.URL)
	}
	return &FetchResult{Data: body, Checksum: fmt.Sprintf("%x", sha256.Sum256(body))}, nil
}

func TestResolveExternalRefs_InlinesAndRewrites(t *testing.T) {
	main := []byte(`{
		"openapi": "3.0.0",
		"paths": {
			"/pets": {
				"get": {
					"responses": {
						"200": {
							"content": {
								"application/json": {
									"schema": {"$ref": "https://schemas.example.com/pet.json#/definitions/Pet"}
								}
							}
						}
					}
				}
			}
		}
	}`)
	petDoc := []byte(`{"definitions":{"Pet":{"type":"object","properties":{"name":{"type":"string"}}}}}`)

	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://schemas.example.com/pet.json": petDoc,
	}}
	resolved, warnings, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.json", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	var out map[string]any
	if err := json.Unmarshal(resolved, &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	raw := string(resolved)
	if !strings.Contains(raw, `"$ref":"#/components/schemas/pet_definitions_Pet"`) {
		t.Fatalf("rewritten $ref missing; got: %s", raw)
	}
	comps, _ := out["components"].(map[string]any)
	schemas, _ := comps["schemas"].(map[string]any)
	if schemas["pet_definitions_Pet"] == nil {
		t.Fatalf("inlined schema missing; schemas=%v", schemas)
	}
	if fetcher.hits["https://schemas.example.com/pet.json"] != 1 {
		t.Fatalf("expected fetch once, got %d", fetcher.hits["https://schemas.example.com/pet.json"])
	}
}

func TestResolveExternalRefs_RelativeRef(t *testing.T) {
	main := []byte(`{"paths":{"/a":{"get":{"responses":{"200":{"$ref":"./fragment.json#/Response"}}}}}}`)
	frag := []byte(`{"Response":{"description":"ok"}}`)

	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://api.example.com/v1/fragment.json": frag,
	}}
	resolved, _, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/v1/openapi.json", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if !strings.Contains(string(resolved), "fragment_Response") {
		t.Fatalf("relative ref not resolved: %s", resolved)
	}
}

func TestResolveExternalRefs_NestedRelativeRefResolvesAgainstContainingDocument(t *testing.T) {
	// A relative ref inside a fetched document is relative to that
	// document, not to the main spec.
	main := []byte(`{"paths":{"/pets":{"get":{"responses":{"200":{"content":{"application/json":{` +
		`"schema":{"$ref":"schemas/pet.json#/Pet"}}}}}}}}}`)
	pet := []byte(`{"Pet":{"type":"object","properties":{"category":{"$ref":"category.json#/Category"}}}}`)
	category := []byte(`{"Category":{"type":"string"}}`)

	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://api.example.com/v1/schemas/pet.json":      pet,
		"https://api.example.com/v1/schemas/category.json": category,
	}}
	resolved, warnings, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/v1/openapi.json", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("resolve failed: %v (fetches: %v)", err, fetcher.hits)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	wantHits := map[string]int{
		"https://api.example.com/v1/schemas/pet.json":      1,
		"https://api.example.com/v1/schemas/category.json": 1,
	}
	if !maps.Equal(fetcher.hits, wantHits) {
		t.Errorf("expected fetches %v, got %v", wantHits, fetcher.hits)
	}
	if !strings.Contains(string(resolved), `"$ref":"#/components/schemas/category_Category"`) {
		t.Errorf("nested ref not rewritten to the inlined category schema: %s", resolved)
	}
}

func TestResolveExternalRefs_InternalRefsUntouched(t *testing.T) {
	main := []byte(
		`{"components":{"schemas":{"X":{"type":"string"}}},"paths":{"/a":{"get":{"responses":{"200":{"$ref":"#/components/schemas/X"}}}}}}`,
	)
	fetcher := &stubFetcher{docs: map[string][]byte{}}
	resolved, warnings, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.json", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !strings.Contains(string(resolved), `"$ref":"#/components/schemas/X"`) {
		t.Fatalf("internal ref modified: %s", resolved)
	}
}

func TestResolveExternalRefs_FetchFailureIsFatal(t *testing.T) {
	main := []byte(`{"paths":{"/a":{"get":{"responses":{"200":{"$ref":"https://missing.example/x.json#/A"}}}}}}`)
	fetcher := &stubFetcher{docs: map[string][]byte{}}
	_, _, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.json", fetcher, FetchSource{})
	if err == nil {
		t.Fatalf("expected a fatal error for failed external $ref fetch")
	}
	if !strings.HasPrefix(err.Error(), "resolving external $refs: ") {
		t.Errorf("expected the error to be marked as an external $ref failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "https://missing.example/x.json") {
		t.Fatalf("expected error to mention the failing URL, got: %v", err)
	}
}

func TestResolveExternalRefs_FetchFailureStopsFurtherResolution(t *testing.T) {
	// Two external refs to two different documents, reached in a
	// deterministic (array) order. The first document's fetch fails.
	main := []byte(`{
		"paths": {
			"/a": {
				"get": {
					"responses": {
						"200": {
							"content": {
								"application/json": {
									"schema": {
										"anyOf": [
											{"$ref": "https://missing.example/one.json#/A"},
											{"$ref": "https://schemas.example.com/two.json#/B"}
										]
									}
								}
							}
						}
					}
				}
			}
		}
	}`)
	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://schemas.example.com/two.json": []byte(`{"B":{"type":"string"}}`),
	}}
	_, _, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.json", fetcher, FetchSource{})
	if err == nil {
		t.Fatal("expected a fatal error for failed external $ref fetch")
	}
	if !strings.Contains(err.Error(), "https://missing.example/one.json") {
		t.Fatalf("expected error to mention the failing URL, got: %v", err)
	}
	var totalHits int
	for _, n := range fetcher.hits {
		totalHits += n
	}
	if totalHits != 1 {
		t.Fatalf("expected the fetcher to be called exactly once, got %d calls: %v", totalHits, fetcher.hits)
	}
	if fetcher.hits["https://schemas.example.com/two.json"] != 0 {
		t.Fatalf("expected the second document not to be fetched after the first failure, got %v", fetcher.hits)
	}
}

func TestResolveExternalRefs_FirstFailingRefIsDeterministic(t *testing.T) {
	// Failing refs under sibling map keys: the walk visits keys in sorted
	// order, so the fatal error always names the sorted-first key's URL
	// rather than whichever Go's map iteration happens to reach first.
	main := []byte(`{"components":{"schemas":{` +
		`"Hotel":{"$ref":"https://h.example/h.json#/X"},` +
		`"Alpha":{"$ref":"https://a.example/a.json#/X"},` +
		`"Golf":{"$ref":"https://g.example/g.json#/X"},` +
		`"Bravo":{"$ref":"https://b.example/b.json#/X"},` +
		`"Foxtrot":{"$ref":"https://f.example/f.json#/X"},` +
		`"Charlie":{"$ref":"https://c.example/c.json#/X"},` +
		`"Echo":{"$ref":"https://e.example/e.json#/X"},` +
		`"Delta":{"$ref":"https://d.example/d.json#/X"}` +
		`}}}`)
	for range 20 {
		_, _, err := ResolveExternalRefs(context.Background(), main,
			"https://api.example.com/openapi.json", &stubFetcher{}, FetchSource{})
		if err == nil || !strings.Contains(err.Error(), "https://a.example/a.json") {
			t.Fatalf("expected the error to name the sorted-first ref's URL https://a.example/a.json, got: %v", err)
		}
	}
}

func TestResolveExternalRefs_NestedFetchFailureIsFatalAndStopsFurtherResolution(t *testing.T) {
	// The first document fetches fine, but a ref inside it cannot be
	// fetched: that failure is the resolve's fatal error, and nothing after
	// it is fetched — neither the fetched document's next ref nor the main
	// spec's next ref.
	main := []byte(`{"paths":{"/a":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{` +
		`"anyOf":[{"$ref":"schemas/pet.json#/Pet"},{"$ref":"https://schemas.example.com/two.json#/B"}]` +
		`}}}}}}}}}`)
	pet := []byte(`{"Pet":{"anyOf":[{"$ref":"missing.json#/M"},{"$ref":"tag.json#/Tag"}]}}`)

	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://api.example.com/v1/schemas/pet.json": pet,
		"https://api.example.com/v1/schemas/tag.json": []byte(`{"Tag":{"type":"string"}}`),
		"https://schemas.example.com/two.json":        []byte(`{"B":{"type":"string"}}`),
	}}
	_, _, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/v1/openapi.json", fetcher, FetchSource{})
	if err == nil {
		t.Fatal("expected a fatal error for the failed nested $ref fetch")
	}
	if !strings.Contains(err.Error(), "https://api.example.com/v1/schemas/missing.json") {
		t.Fatalf("expected error to mention the failing nested URL, got: %v", err)
	}
	wantHits := map[string]int{
		"https://api.example.com/v1/schemas/pet.json":     1,
		"https://api.example.com/v1/schemas/missing.json": 1,
	}
	if !maps.Equal(fetcher.hits, wantHits) {
		t.Errorf("expected fetches %v, got %v", wantHits, fetcher.hits)
	}
}

func TestResolveExternalRefs_PointerNotFoundIsWarningNotError(t *testing.T) {
	// Pin existing behavior: a deterministic issue in an otherwise
	// successfully-fetched document (a JSON pointer that doesn't exist) stays
	// a warning, not a fatal error.
	main := []byte(`{"paths":{"/a":{"get":{"responses":{"200":{"$ref":"https://schemas.example.com/doc.json#/Missing"}}}}}}`)
	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://schemas.example.com/doc.json": []byte(`{"Present":{"type":"string"}}`),
	}}
	_, warnings, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.json", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("pointer-not-found must not fail the resolve: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected a warning for the missing pointer")
	}
}

func TestResolveExternalRefs_YAMLInput(t *testing.T) {
	main := []byte(`openapi: 3.0.0
paths:
  /a:
    get:
      responses:
        "200":
          $ref: "https://schemas.example.com/x.yaml#/R"
`)
	other := []byte("R:\n  description: ok\n")
	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://schemas.example.com/x.yaml": other,
	}}
	resolved, warnings, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.yaml", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !strings.Contains(string(resolved), "x_R") {
		t.Fatalf("YAML external ref not inlined: %s", resolved)
	}
}

func TestSplitRef(t *testing.T) {
	cases := []struct {
		ref, wantURL, wantFrag string
	}{
		{"#/components/schemas/X", "", "/components/schemas/X"},
		{"other.json#/X", "other.json", "/X"},
		{"other.json", "other.json", ""},
	}
	for _, tc := range cases {
		gotURL, gotFrag := splitRef(tc.ref)
		if gotURL != tc.wantURL || gotFrag != tc.wantFrag {
			t.Errorf("splitRef(%q) = (%q,%q), want (%q,%q)",
				tc.ref, gotURL, gotFrag, tc.wantURL, tc.wantFrag)
		}
	}
}

func TestPointerLookup(t *testing.T) {
	doc := map[string]any{
		"a": map[string]any{
			"b/c": "value",
		},
	}
	got, err := pointerLookup(doc, "/a/b~1c")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	if got != "value" {
		t.Fatalf("expected value, got %v", got)
	}
}

func TestPointerLookup_ArrayIndex(t *testing.T) {
	doc := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "integer"},
		},
	}
	got, err := pointerLookup(doc, "/oneOf/1")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", got)
	}
	if m["type"] != "integer" {
		t.Fatalf("expected integer, got %v", m["type"])
	}
}

func TestPointerLookup_ArrayOutOfRange(t *testing.T) {
	doc := map[string]any{
		"items": []any{"a"},
	}
	_, err := pointerLookup(doc, "/items/5")
	if err == nil {
		t.Fatal("expected error for out-of-range index")
	}
}

func TestDeepCloneJSON(t *testing.T) {
	original := map[string]any{
		"a": map[string]any{
			"b": []any{"x", "y"},
		},
		"c": float64(42),
	}
	cloned := deepCloneJSON(original)
	clonedMap := cloned.(map[string]any)
	// Mutate the clone; original must be untouched.
	clonedMap["a"].(map[string]any)["b"] = "replaced"
	if original["a"].(map[string]any)["b"].([]any)[0] != "x" {
		t.Fatal("deepCloneJSON did not produce an independent copy")
	}
}

func TestResolveExternalRefs_CycleDetection(t *testing.T) {
	// Doc A references Doc B's schema, and Doc B references Doc A's schema
	// back — a genuine mutual cycle that must not infinite loop.
	docA := `{"openapi":"3.0.0","info":{"title":"A","version":"1"},"components":{"schemas":{"AType":{"type":"object","properties":{"child":{"$ref":"b.yaml#/components/schemas/BType"}}}}},"paths":{"/a":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"b.yaml#/components/schemas/BType"}}}}}}}}}`
	docB := `{"components":{"schemas":{"BType":{"type":"object","properties":{"parent":{"$ref":"a.yaml#/components/schemas/AType"}}}}}}`

	fetcher := &stubFetcher{docs: map[string][]byte{
		"https://example.com/b.yaml": []byte(docB),
		"https://example.com/a.yaml": []byte(docA),
	}}

	resolved, warnings, err := ResolveExternalRefs(
		context.Background(),
		[]byte(docA),
		"https://example.com/a.yaml",
		fetcher,
		FetchSource{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The cycle should produce a warning, not a panic/infinite recursion.
	if len(warnings) == 0 {
		t.Error("expected at least one cycle-detection warning")
	}
	hasCycleWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "cycle detected") {
			hasCycleWarning = true
			break
		}
	}
	if !hasCycleWarning {
		t.Errorf("expected a 'cycle detected' warning, got: %v", warnings)
	}
	// The returned document must still be valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal(resolved, &parsed); err != nil {
		t.Fatalf("resolved document is not valid JSON: %v", err)
	}
	// The inlined BType schema should have its self-ref rewritten to a local ref.
	schemas, _ := parsed["components"].(map[string]any)["schemas"].(map[string]any)
	found := false
	for k := range schemas {
		if strings.Contains(k, "BType") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected BType inlined into components/schemas, got keys: %v", schemas)
	}
}

func TestSanitizeRefName_NoCollision(t *testing.T) {
	// Two refs from the same doc with the same leaf but different paths
	// must produce distinct names.
	n1 := sanitizeRefName("https://example.com/doc.yaml", "/definitions/Pet")
	n2 := sanitizeRefName("https://example.com/doc.yaml", "/components/schemas/Pet")
	if n1 == n2 {
		t.Fatalf("expected distinct names, both got %q", n1)
	}
	// Verify full fragment path is embedded.
	if !strings.Contains(n1, "definitions_Pet") {
		t.Errorf("expected fragment path in name, got %q", n1)
	}
	if !strings.Contains(n2, "components_schemas_Pet") {
		t.Errorf("expected fragment path in name, got %q", n2)
	}
}

func TestSanitizeRefName_EmptyFragment(t *testing.T) {
	name := sanitizeRefName("https://example.com/common.yaml", "")
	if name == "" || name == "external_ref" {
		t.Fatalf("expected non-empty sanitized name derived from doc basename, got %q", name)
	}
	if !strings.Contains(name, "common") {
		t.Errorf("expected doc basename in name, got %q", name)
	}
}

func TestResolveExternalRefs_ReportsLocalRefInFetchedDocument(t *testing.T) {
	main := []byte(`{"paths":{"/a":{"get":{"responses":{"200":{"content":{"application/json":` +
		`{"schema":{"$ref":"common.json#/Pet"}}}}}}}}}`)
	common := []byte(`{"Pet":{"properties":{"owner":{"$ref":"#/Owner"}}},"Owner":{"type":"string"}}`)
	fetcher := &stubFetcher{docs: map[string][]byte{"https://api.example.com/common.json": common}}

	_, warnings, err := ResolveExternalRefs(context.Background(), main,
		"https://api.example.com/openapi.json", fetcher, FetchSource{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := `$ref "#/Owner" in https://api.example.com/common.json is resolved against the main spec`
	if len(warnings) != 1 || !strings.Contains(warnings[0], want) {
		t.Errorf("warnings = %q, want one containing %q", warnings, want)
	}
}
