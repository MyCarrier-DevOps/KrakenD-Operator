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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
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
// A reference an "examples" object holds is to an Example Object, which is
// data: its target is fetched and inlined under `components.examples`, and
// the reference is rewritten to `#/components/examples/<name>`, without
// resolving anything inside the target. Only a root $ref in the target, a
// chain to the object that holds the data, is followed. The name is the
// sanitized key, or that key with _2, _3 and so on when the spec's own
// components.examples, or another inlined example, already holds it.
//
// baseURL is the main spec's URL. A relative reference resolves against the
// URL of the document that contains it: baseURL for a ref in the main spec,
// the fetched document's URL for a ref inside an external document. When
// called without a URL, external refs are left untouched and each is returned
// as a "failed to resolve" warning. The controller does not call it for a
// ConfigMap-sourced spec; it lists those refs with ExternalRefs instead.
//
// The returned JSON is always JSON (regardless of input format). External
// documents fetched as YAML are converted to JSON before inlining.
//
// Fetched documents are cached within the call. Cycle detection prevents
// unbounded recursion when external documents reference each other.
//
// A failed fetch or decode of an external document aborts the resolution;
// its error is prefixed "resolving external $refs: ". An error decoding or
// marshaling the main spec itself is returned without that prefix.
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
	resolver.takenExamples = existingExampleNames(root)
	resolver.walk(root, baseURL)
	if resolver.fatalErr != nil {
		return nil, resolver.warnings, fmt.Errorf("resolving external $refs: %w", resolver.fatalErr)
	}

	// Inline collected external schemas under components/schemas.
	if len(resolver.inlined) > 0 {
		schemas := componentMap(root, "schemas")
		for name, body := range resolver.inlined {
			if _, exists := schemas[name]; exists {
				msg := fmt.Sprintf(
					"external ref schema name collision: %q already exists in components/schemas; skipping",
					name,
				)
				resolver.warnings = append(resolver.warnings, msg)
				continue
			}
			schemas[name] = body
		}
	}

	// Inline collected external Example Objects under components/examples.
	if len(resolver.inlinedExamples) > 0 {
		maps.Copy(componentMap(root, "examples"), resolver.inlinedExamples)
	}

	out, err := json.Marshal(root)
	if err != nil {
		return nil, resolver.warnings, fmt.Errorf("marshaling resolved spec: %w", err)
	}
	return out, resolver.warnings, nil
}

// componentMap returns root's components.<kind> map, creating it when absent.
func componentMap(root map[string]any, kind string) map[string]any {
	components, ok := root["components"].(map[string]any)
	if !ok || components == nil {
		components = map[string]any{}
		root["components"] = components
	}
	m, ok := components[kind].(map[string]any)
	if !ok || m == nil {
		m = map[string]any{}
		components[kind] = m
	}
	return m
}

// existingExampleNames returns the keys of root's components.examples.
func existingExampleNames(root map[string]any) map[string]bool {
	taken := map[string]bool{}
	components, ok := root["components"].(map[string]any)
	if !ok {
		return taken
	}
	examples, ok := components["examples"].(map[string]any)
	if !ok {
		return taken
	}
	for name := range examples {
		taken[name] = true
	}
	return taken
}

// refRole is what an external $ref's target is: a schema, which is walked for
// the refs it holds, or an Example Object, which is data.
type refRole int

const (
	schemaRole refRole = iota
	exampleRole
)

// componentsPath is where a target of this role is inlined.
func (role refRole) componentsPath() string {
	if role == exampleRole {
		return "#/components/examples/"
	}
	return "#/components/schemas/"
}

type refResolver struct {
	ctx     context.Context
	baseURL string
	fetcher Fetcher
	source  FetchSource
	docs    map[string]map[string]any // cache: absoluteURL -> parsed doc
	inlined map[string]any            // sanitized name -> schema body
	// inlinedExamples is the Example Object bodies, by name, for components/examples.
	inlinedExamples map[string]any
	// takenExamples is the names in components/examples that an Example Object
	// body cannot take: the input's own, and those already given to a body.
	takenExamples map[string]bool
	resolving     map[string]string // cycle detection: role and ref keys being resolved -> their name
	resolved      map[string]string // role and ref key -> local name for already-resolved refs
	warnings      []string
	warned        map[string]bool // warnings already recorded, so each is reported once
	fatalErr      error           // first fetch/decode failure; halts all further resolution
}

// freeExampleName returns name, or name_2, name_3, and so on, whichever is the
// first not in components/examples yet, and takes it. An Example Object body
// never replaces an entry the spec already has, or takes the name a reference
// in the spec points at.
func (r *refResolver) freeExampleName(name string) string {
	free := name
	for n := 2; r.takenExamples[free]; n++ {
		free = fmt.Sprintf("%s_%d", name, n)
	}
	r.takenExamples[free] = true
	return free
}

var sanitizeNameRE = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// fatalRefError marks a resolveExternal error that must abort the whole
// resolution: a failed fetch or decode of an external document. Deterministic
// per-ref issues (pointer not found, cycles, name collisions) are returned as
// plain errors and are recorded as warnings instead, without aborting.
type fatalRefError struct {
	err error
}

func (e *fatalRefError) Error() string { return e.err.Error() }
func (e *fatalRefError) Unwrap() error { return e.err }

// walk recursively scans node, an object of the document at base, replacing
// every external $ref with a local one. Once a fatal error (a failed fetch or
// decode of an external document) has been recorded, walk stops descending so
// no further refs are resolved and no further documents are fetched.
// An example payload, the value of an object's "example" or "examples" field,
// is data: walk neither descends into it nor fetches from it, and resolves
// only the Example Object references an "examples" object holds, in the
// example role (see resolveExternal). The members
// of a name-keyed map (nameKeyedMaps) are objects whatever they are named.
func (r *refResolver) walk(node any, base string) {
	if r.fatalErr != nil {
		return
	}
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			r.warnLocalRef(ref, base)
		}
		if ref, ok := v["$ref"].(string); ok && ref != "" && !strings.HasPrefix(ref, "#") {
			if localName, err := r.resolveExternal(ref, base, schemaRole); err == nil {
				v["$ref"] = schemaRole.componentsPath() + localName
			} else {
				var fatal *fatalRefError
				if errors.As(err, &fatal) {
					r.fatalErr = fatal.err
				} else {
					r.warnings = append(r.warnings,
						fmt.Sprintf("failed to resolve external $ref %q: %v", ref, err))
				}
			}
			return
		}
		// Sorted keys make the walk order, and so the first failing ref
		// and its error, deterministic.
		for _, k := range slices.Sorted(maps.Keys(v)) {
			members, isMap := nameKeyedMembers(k, v[k])
			payload, exampleRefs := examplePayload(k, v[k])
			switch {
			case isMap:
				for _, member := range members {
					r.walk(member, base)
				}
			case payload:
				for _, ref := range exampleRefs {
					r.resolveExampleRef(ref, base)
				}
			default:
				r.walk(v[k], base)
			}
			if r.fatalErr != nil {
				return
			}
		}
	case []any:
		for _, child := range v {
			r.walk(child, base)
			if r.fatalErr != nil {
				return
			}
		}
	}
}

// resolveExampleRef resolves entry, an Example Object reference held by an
// "examples" object, and rewrites it to point into components/examples.
func (r *refResolver) resolveExampleRef(entry map[string]any, base string) {
	if r.fatalErr != nil {
		return
	}
	ref, ok := entry["$ref"].(string)
	r.warnLocalRef(ref, base)
	if !ok || ref == "" || strings.HasPrefix(ref, "#") {
		return
	}
	if localName, err := r.resolveExternal(ref, base, exampleRole); err == nil {
		entry["$ref"] = exampleRole.componentsPath() + localName
	} else {
		var fatal *fatalRefError
		if errors.As(err, &fatal) {
			r.fatalErr = fatal.err
		} else {
			r.warnings = append(r.warnings,
				fmt.Sprintf("failed to resolve external $ref %q: %v", ref, err))
		}
	}
}

// warnLocalRef warns that a "#/" ref in a fetched document is resolved against
// the main spec once the document's subtree is inlined into it.
func (r *refResolver) warnLocalRef(ref, base string) {
	if strings.HasPrefix(ref, "#") && base != r.baseURL {
		r.warnOnce(fmt.Sprintf(
			"$ref %q in %s is resolved against the main spec after inlining, not against %s",
			ref, base, base))
	}
}

// warnOnce records msg as a warning unless it was already recorded.
func (r *refResolver) warnOnce(msg string) {
	if r.warned[msg] {
		return
	}
	if r.warned == nil {
		r.warned = map[string]bool{}
	}
	r.warned[msg] = true
	r.warnings = append(r.warnings, msg)
}

// resolveExternal fetches the document ref (found in the document at base)
// points to (caching), extracts the referenced fragment, inlines it into the
// root doc, and returns the local name used for the new $ref. In the schema
// role the fragment is walked and inlined under components/schemas, named by
// sanitizeRefName. In the example role it is data: cloned, not walked except
// for a root $ref chain, and inlined under components/examples at a free name.
func (r *refResolver) resolveExternal(ref, base string, role refRole) (string, error) {
	docURL, fragment := splitRef(ref)
	absolute, err := absolutize(docURL, base)
	if err != nil {
		return "", err
	}

	// Fast path: if this exact ref was already fully resolved, return the
	// cached name without re-walking or emitting false collision warnings.
	// Both caches are keyed by role too: a fragment reached as a schema and as
	// an Example Object yields two bodies, one walked and one not.
	refKey := absolute + "#" + fragment
	cacheKey := fmt.Sprintf("%d %s", role, refKey)
	if name, ok := r.resolved[cacheKey]; ok {
		return name, nil
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
			return "", &fatalRefError{fmt.Errorf("fetching %s: %w", RedactURL(absolute), err)}
		}
		parsed, err := decodeSpec(fetched.Data)
		if err != nil {
			return "", &fatalRefError{fmt.Errorf("decoding %s: %w", absolute, err)}
		}
		doc = parsed
		r.docs[absolute] = doc
	}

	target, err := pointerLookup(doc, fragment)
	if err != nil {
		return "", err
	}

	// Cycle detection: if we are already resolving this ref, short-circuit
	// with the name the outer (first) call chose. Do NOT write to r.inlined
	// here — the outer call will store the properly-walked clone after its
	// r.walk completes.
	if name, busy := r.resolving[cacheKey]; busy {
		r.warnings = append(r.warnings, fmt.Sprintf("cycle detected for %s, skipping recursive resolution", refKey))
		return name, nil
	}
	name := sanitizeRefName(absolute, fragment)
	if role == exampleRole {
		name = r.freeExampleName(name)
	}
	if r.resolving == nil {
		r.resolving = map[string]string{}
	}
	r.resolving[cacheKey] = name
	defer delete(r.resolving, cacheKey)

	// Deep-clone the target before walking so the cached document is not mutated.
	target = deepCloneJSON(target)

	// Walk the cloned schema so nested external refs are resolved, relative to
	// the document they appear in. An Example Object is data and is not walked.
	if role == schemaRole {
		r.walk(target, absolute)
	} else if root, ok := target.(map[string]any); ok {
		// The one thing followed in an Example Object is a root $ref, a chain
		// to the object that holds the data. Its siblings are not walked.
		r.resolveExampleRef(root, absolute)
	}

	if r.resolved == nil {
		r.resolved = map[string]string{}
	}
	if role == exampleRole {
		if r.inlinedExamples == nil {
			r.inlinedExamples = map[string]any{}
		}
		r.inlinedExamples[name] = target
		r.resolved[cacheKey] = name
		return name, nil
	}
	if r.inlined == nil {
		r.inlined = map[string]any{}
	}
	if _, exists := r.inlined[name]; exists {
		r.warnings = append(r.warnings, fmt.Sprintf(
			"external ref name collision: %q produced by multiple sources; keeping first",
			name,
		))
	} else {
		r.inlined[name] = target
	}
	r.resolved[cacheKey] = name
	return name, nil
}

// absolutize resolves docURL against base, the URL of the document the
// reference appears in.
func absolutize(docURL, base string) (string, error) {
	if docURL == "" {
		return base, nil
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parsing base URL %q: %w", base, err)
	}
	ref, err := url.Parse(docURL)
	if err != nil {
		return "", fmt.Errorf("parsing ref url %q: %w", docURL, err)
	}
	return baseURL.ResolveReference(ref).String(), nil
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
// absolute URL + fragment. All non-empty segments of the fragment path
// are joined with underscores (e.g. #/components/schemas/Pet →
// "components_schemas_Pet"), prefixed with the basename of the document
// for disambiguation. Using the full path avoids collisions when
// different fragment paths share the same leaf name.
func sanitizeRefName(absoluteURL, fragment string) string {
	var fragPart string
	if fragment != "" {
		parts := strings.Split(strings.TrimPrefix(fragment, "/"), "/")
		var nonEmpty []string
		for _, p := range parts {
			if p != "" {
				nonEmpty = append(nonEmpty, p)
			}
		}
		fragPart = strings.Join(nonEmpty, "_")
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

	name := strings.Trim(docBase+"_"+fragPart, "_")
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
// It tries json.Unmarshal first and only falls back to YAML conversion on
// failure, avoiding an extra string allocation for the common JSON case.
func decodeSpec(data []byte) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal(data, &out); err == nil {
		return out, nil
	}
	// Fall back to YAML (also parses JSON).
	asJSON, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(asJSON, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ExternalRefs returns the distinct $refs in specData that point outside the
// document (those not starting with "#"), sorted. Only a URL-sourced spec has
// a base to resolve them against, so in a ConfigMap-sourced spec they stay
// unresolved. $refs inside example payloads are data, not references, and are
// skipped, apart from an examples entry that is itself a $ref. The members of
// a name-keyed map are objects whatever they are named, so an external ref
// below a schema or response called "example" is reported.
func ExternalRefs(specData []byte) ([]string, error) {
	root, err := decodeSpec(specData)
	if err != nil {
		return nil, fmt.Errorf("decoding spec: %w", err)
	}
	refs := map[string]struct{}{}
	walkJSON(root, func(key string, value any) bool {
		if payload, own := examplePayload(key, value); payload {
			for _, entry := range own {
				if ref, ok := entry["$ref"].(string); ok && isExternalRef(ref) {
					refs[ref] = struct{}{}
				}
			}
			return false
		}
		if s, ok := value.(string); ok && key == "$ref" && isExternalRef(s) {
			refs[s] = struct{}{}
		}
		return true
	})
	return slices.Sorted(maps.Keys(refs)), nil
}

// isExternalRef reports whether ref points outside the document: it is not
// empty and does not start with "#".
func isExternalRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "#")
}
