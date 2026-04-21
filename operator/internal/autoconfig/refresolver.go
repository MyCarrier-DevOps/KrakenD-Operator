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
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// ResolveExternalRefs walks the OpenAPI spec and, for every `$ref` that
// points to an external document (i.e. not starting with `#`), fetches the
// referenced document, extracts the referenced fragment, and inlines it
// into `components.schemas` of the main spec under a sanitized key. The
// original $ref is rewritten to `#/components/schemas/<sanitized-name>`.
//
// baseURL is used to resolve relative references. When the source is a
// ConfigMap (no URL), external refs are left untouched and a warning is
// returned via the errors slice.
//
// The returned JSON is always JSON (regardless of input format). External
// documents fetched as YAML are converted to JSON before inlining.
//
// Fetched documents are cached within the call. Cycle detection prevents
// unbounded recursion when external documents reference each other.
func ResolveExternalRefs(
	ctx context.Context,
	specData []byte,
	baseURL string,
	fetcher Fetcher,
	source FetchSource,
) (resolvedJSON []byte, warnings []string, err error) {
	root, err := decodeSpec(specData)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding spec: %w", err)
	}

	resolver := &refResolver{
		ctx:     ctx,
		baseURL: baseURL,
		fetcher: fetcher,
		source:  source,
		docs:    map[string]map[string]any{},
	}
	resolver.walk(root, "")

	// Inline collected external schemas under components/schemas.
	if len(resolver.inlined) > 0 {
		components, ok := root["components"].(map[string]any)
		if !ok || components == nil {
			components = map[string]any{}
			root["components"] = components
		}
		schemas, ok := components["schemas"].(map[string]any)
		if !ok || schemas == nil {
			schemas = map[string]any{}
			components["schemas"] = schemas
		}
		for name, body := range resolver.inlined {
			if _, exists := schemas[name]; !exists {
				schemas[name] = body
			}
		}
	}

	out, err := json.Marshal(root)
	if err != nil {
		return nil, resolver.warnings, fmt.Errorf("marshaling resolved spec: %w", err)
	}
	return out, resolver.warnings, nil
}

type refResolver struct {
	ctx       context.Context
	baseURL   string
	fetcher   Fetcher
	source    FetchSource
	docs      map[string]map[string]any // cache: absoluteURL -> parsed doc
	inlined   map[string]any            // sanitized name -> schema body
	resolving map[string]bool           // cycle detection: ref keys currently being resolved
	warnings  []string
}

var sanitizeNameRE = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// walk recursively scans m, replacing every external $ref with a local one.
func (r *refResolver) walk(node any, pointer string) {
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok && ref != "" && !strings.HasPrefix(ref, "#") {
			if localName, err := r.resolveExternal(ref); err == nil {
				v["$ref"] = "#/components/schemas/" + localName
			} else {
				r.warnings = append(r.warnings,
					fmt.Sprintf("failed to resolve external $ref %q: %v", ref, err))
			}
			return
		}
		for k, child := range v {
			r.walk(child, pointer+"/"+k)
		}
	case []any:
		for _, child := range v {
			r.walk(child, pointer)
		}
	}
}

// resolveExternal fetches the referenced document (caching), extracts the
// referenced fragment, inlines it into the components/schemas map of the
// root doc, and returns the sanitized local name used for the new $ref.
func (r *refResolver) resolveExternal(ref string) (string, error) {
	docURL, fragment := splitRef(ref)
	absolute, err := r.absolutize(docURL)
	if err != nil {
		return "", err
	}

	doc, ok := r.docs[absolute]
	if !ok {
		if r.fetcher == nil || r.baseURL == "" {
			return "", fmt.Errorf("external refs require an http(s) source")
		}
		child := r.source
		child.URL = absolute
		child.ConfigMapRef = nil
		fetched, err := r.fetcher.Fetch(r.ctx, child)
		if err != nil {
			return "", fmt.Errorf("fetching %s: %w", absolute, err)
		}
		parsed, err := decodeSpec(fetched.Data)
		if err != nil {
			return "", fmt.Errorf("decoding %s: %w", absolute, err)
		}
		doc = parsed
		r.docs[absolute] = doc
	}

	target, err := pointerLookup(doc, fragment)
	if err != nil {
		return "", err
	}

	name := sanitizeRefName(absolute, fragment)

	// Cycle detection: if we are already resolving this ref, short-circuit.
	refKey := absolute + "#" + fragment
	if r.resolving[refKey] {
		// Already in-flight — return the name so callers get a valid local ref.
		if r.inlined == nil {
			r.inlined = map[string]any{}
		}
		if _, exists := r.inlined[name]; !exists {
			r.inlined[name] = target
		}
		return name, nil
	}
	if r.resolving == nil {
		r.resolving = map[string]bool{}
	}
	r.resolving[refKey] = true
	defer delete(r.resolving, refKey)

	// Deep-clone the target before walking so the cached document is not mutated.
	target = deepCloneJSON(target)

	// Walk the cloned node so nested external refs are resolved.
	r.walk(target, "")

	if r.inlined == nil {
		r.inlined = map[string]any{}
	}
	r.inlined[name] = target
	return name, nil
}

func (r *refResolver) absolutize(docURL string) (string, error) {
	if docURL == "" {
		return r.baseURL, nil
	}
	base, err := url.Parse(r.baseURL)
	if err != nil {
		return "", fmt.Errorf("parsing baseURL: %w", err)
	}
	ref, err := url.Parse(docURL)
	if err != nil {
		return "", fmt.Errorf("parsing ref url %q: %w", docURL, err)
	}
	return base.ResolveReference(ref).String(), nil
}

// splitRef splits a $ref into its URI portion (possibly empty) and its
// JSON pointer fragment (possibly empty, without the leading #).
func splitRef(ref string) (docURL, fragment string) {
	idx := strings.Index(ref, "#")
	if idx < 0 {
		return ref, ""
	}
	return ref[:idx], ref[idx+1:]
}

// pointerLookup walks a JSON pointer (RFC 6901) and returns the referenced node.
// Supports both object keys and numeric array indices.
func pointerLookup(doc map[string]any, pointer string) (any, error) {
	if pointer == "" {
		return doc, nil
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	var current any = doc
	for _, part := range parts {
		decoded := strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch next := current.(type) {
		case map[string]any:
			val, ok := next[decoded]
			if !ok {
				return nil, fmt.Errorf("pointer segment %q not found", decoded)
			}
			current = val
		case []any:
			idx, err := strconv.Atoi(decoded)
			if err != nil {
				return nil, fmt.Errorf("non-numeric array index %q", decoded)
			}
			if idx < 0 || idx >= len(next) {
				return nil, fmt.Errorf("array index %d out of range (len %d)", idx, len(next))
			}
			current = next[idx]
		default:
			return nil, fmt.Errorf("pointer segment %q applied to scalar", decoded)
		}
	}
	return current, nil
}

// sanitizeRefName produces a deterministic local schema name from an
// absolute URL + fragment. Uses the final segment of the fragment when
// available (e.g. #/components/schemas/Pet → "Pet"), prefixed with the
// basename of the document for disambiguation.
func sanitizeRefName(absoluteURL, fragment string) string {
	var leaf string
	if fragment != "" {
		parts := strings.Split(fragment, "/")
		leaf = parts[len(parts)-1]
	}

	u, err := url.Parse(absoluteURL)
	var docBase string
	if err == nil {
		path := strings.TrimSuffix(u.Path, "/")
		segs := strings.Split(path, "/")
		if len(segs) > 0 {
			docBase = segs[len(segs)-1]
			if idx := strings.LastIndex(docBase, "."); idx > 0 {
				docBase = docBase[:idx]
			}
		}
	}

	name := strings.Trim(docBase+"_"+leaf, "_")
	if name == "" {
		name = "external_ref"
	}
	return sanitizeNameRE.ReplaceAllString(name, "_")
}

// deepCloneJSON returns a deep copy of a JSON-compatible value (maps, slices, scalars).
func deepCloneJSON(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			out[k] = deepCloneJSON(child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = deepCloneJSON(child)
		}
		return out
	default:
		return val // scalars are immutable
	}
}

// decodeSpec accepts a JSON or YAML document and returns it as a generic map.
func decodeSpec(data []byte) (map[string]any, error) {
	trimmed := strings.TrimLeftFunc(string(data), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\ufeff'
	})
	if strings.HasPrefix(trimmed, "{") {
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	// Fall back to YAML (also parses JSON).
	asJSON, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(asJSON, &out); err != nil {
		return nil, err
	}
	return out, nil
}
