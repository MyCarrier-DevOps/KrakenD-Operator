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

// ErrParameterRefsTooLarge reports that inlining parameter $refs would grow
// the spec past the body size limit.
var ErrParameterRefsTooLarge = errors.New("dereferenced parameters exceed the spec size limit")

// DereferenceParameters replaces every local $ref ("#/...") in a path item's
// or operation's parameters with the parameter object it points to, so the
// CUE definitions see each parameter's name and location: they decide the
// query strings and headers an endpoint forwards. Only targets under
// #/components/ are followed, which is where ResolveExternalRefs rewrites the
// external refs it resolves; a ref into any other section is not a parameter.
//
// It returns specData unchanged when no parameter is a $ref. A ref it cannot
// resolve is left in place and reported in warnings, in path and method
// order; the operation that uses it then fails CUE evaluation instead of
// silently forwarding nothing. A decode error returns specData unchanged
// with the error, and so does a result that would, with the input, exceed
// maxBodyBytes (ErrParameterRefsTooLarge).
func DereferenceParameters(specData []byte) (out []byte, warnings []string, err error) {
	root, err := decodeSpec(specData)
	if err != nil {
		return specData, nil, fmt.Errorf("decoding spec: %w", err)
	}
	paths, ok := root["paths"].(map[string]any)
	if !ok {
		return specData, nil, nil
	}
	d := &dereferencer{root: root, remaining: maxBodyBytes - len(specData), sizes: map[string]int{}}
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		if err := d.list(item, path+" (used by every operation on "+path+")"); err != nil {
			return specData, nil, err
		}
		for _, method := range httpMethods {
			if op, ok := item[method].(map[string]any); ok {
				if err := d.list(op, strings.ToUpper(method)+" "+path); err != nil {
					return specData, nil, err
				}
			}
		}
	}
	if !d.changed {
		return specData, d.warnings, nil
	}
	out, err = json.Marshal(root)
	if err != nil {
		return specData, d.warnings, fmt.Errorf("re-encoding spec: %w", err)
	}
	return out, d.warnings, nil
}

// dereferencer inlines parameter $refs into root. It keeps the byte budget
// left for inlined parameters and the encoded size of each target it has
// measured, so a target used by many operations is measured once.
type dereferencer struct {
	root      map[string]any
	warnings  []string
	changed   bool
	remaining int
	sizes     map[string]int
}

// list dereferences the local $refs in holder's "parameters" list in place.
// where names holder in warnings: the path (and that it applies to every
// operation) for a path item, "METHOD /path" for an operation.
func (d *dereferencer) list(holder map[string]any, where string) error {
	params, ok := holder["parameters"].([]any)
	if !ok {
		return nil
	}
	for i, p := range params {
		ref, ok := localRef(p)
		if !ok {
			continue
		}
		target, err := resolveParameter(d.root, ref)
		if err != nil {
			d.warnings = append(d.warnings,
				fmt.Sprintf("parameter $ref %q in %s cannot be resolved: %v", ref, where, err))
			continue
		}
		size, measured := d.sizes[ref]
		if !measured {
			encoded, err := json.Marshal(target)
			if err != nil {
				return fmt.Errorf("measuring parameter %q: %w", ref, err)
			}
			size = len(encoded)
			d.sizes[ref] = size
		}
		if d.remaining -= size; d.remaining < 0 {
			return ErrParameterRefsTooLarge
		}
		params[i] = deepCloneJSON(target)
		d.changed = true
	}
	return nil
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
