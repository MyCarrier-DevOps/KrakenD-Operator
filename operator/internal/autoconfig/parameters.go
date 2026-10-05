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
// It returns specData unchanged when no parameter is a $ref. A decode error
// returns specData unchanged with the error.
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
		if dereferenceList(root, item) {
			changed = true
		}
		for _, method := range httpMethods {
			if op, ok := item[method].(map[string]any); ok && dereferenceList(root, op) {
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
// in place, reporting whether it replaced any.
func dereferenceList(root, holder map[string]any) bool {
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
// parameter object.
func resolveParameter(root map[string]any, ref string) (any, error) {
	for range maxParameterRefDepth {
		target, err := pointerLookup(root, strings.TrimPrefix(ref, "#"))
		if err != nil {
			return nil, err
		}
		next, isRef := localRef(target)
		if !isRef {
			return target, nil
		}
		ref = next
	}
	return nil, errors.New("too many chained $refs")
}
