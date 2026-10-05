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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// maxParameterRefDepth bounds a chain of parameter $refs that point at other
// $refs, so a cycle cannot recurse forever.
const maxParameterRefDepth = 8

// DereferenceParameters replaces every local $ref ("#/...") in a path item's
// or operation's parameters with the parameter object it points to, so the
// CUE definitions see each parameter's name and location: they decide the
// query strings and headers an endpoint forwards. External refs are left for
// ResolveExternalRefs, which rewrites them to local ones before this runs.
//
// It returns specData unchanged when no parameter is a $ref. A ref it cannot
// resolve is left in place and reported in warnings, in path and method
// order; the operation that uses it then fails CUE evaluation instead of
// silently forwarding nothing. A decode error returns specData unchanged
// with the error.
func DereferenceParameters(specData []byte) (out []byte, warnings []string, err error) {
	root, err := decodeSpec(specData)
	if err != nil {
		return specData, nil, fmt.Errorf("decoding spec: %w", err)
	}
	paths, ok := root["paths"].(map[string]any)
	if !ok {
		return specData, nil, nil
	}
	changed := false
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		if dereferenceList(root, item, path, &warnings) {
			changed = true
		}
		for _, method := range httpMethods {
			if op, ok := item[method].(map[string]any); ok &&
				dereferenceList(root, op, strings.ToUpper(method)+" "+path, &warnings) {
				changed = true
			}
		}
	}
	if !changed {
		return specData, warnings, nil
	}
	out, err = json.Marshal(root)
	if err != nil {
		return specData, warnings, fmt.Errorf("re-encoding spec: %w", err)
	}
	return out, warnings, nil
}

// dereferenceList dereferences the local $refs in holder's "parameters" list
// in place, reporting whether it replaced any. where names holder in
// warnings: the path for a path item, "METHOD /path" for an operation.
func dereferenceList(root, holder map[string]any, where string, warnings *[]string) bool {
	params, ok := holder["parameters"].([]any)
	if !ok {
		return false
	}
	changed := false
	for i, p := range params {
		ref, ok := localRef(p)
		if !ok {
			continue
		}
		target, err := resolveParameter(root, ref)
		if err != nil {
			*warnings = append(*warnings,
				fmt.Sprintf("parameter $ref %q in %s cannot be resolved: %v", ref, where, err))
			continue
		}
		params[i] = deepCloneJSON(target)
		changed = true
	}
	return changed
}

// localRef returns the "#/..." $ref of a reference object.
func localRef(node any) (string, bool) {
	obj, ok := node.(map[string]any)
	if !ok {
		return "", false
	}
	ref, ok := obj["$ref"].(string)
	return ref, ok && strings.HasPrefix(ref, "#")
}

// resolveParameter follows ref, and any $ref the target is itself, to a
// parameter object. Only targets under #/components/ resolve (external refs
// are rewritten into it), so a ref cannot pull in the paths being rewritten;
// the final target must have a string name and in.
func resolveParameter(root map[string]any, ref string) (map[string]any, error) {
	for range maxParameterRefDepth {
		if !strings.HasPrefix(ref, "#/components/") {
			return nil, errors.New("only local #/components/ targets are dereferenced")
		}
		target, err := pointerLookup(root, strings.TrimPrefix(ref, "#"))
		if err != nil {
			return nil, err
		}
		obj, _ := target.(map[string]any)
		if next, isRef := obj["$ref"].(string); isRef {
			ref = next
			continue
		}
		_, hasName := obj["name"].(string)
		_, hasIn := obj["in"].(string)
		if !hasName || !hasIn {
			return nil, errors.New("target is not a parameter object")
		}
		return obj, nil
	}
	return nil, fmt.Errorf("more than %d chained $refs", maxParameterRefDepth)
}
