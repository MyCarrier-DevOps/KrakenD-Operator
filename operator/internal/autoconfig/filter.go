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

// Package autoconfig implements the OpenAPI-to-endpoint pipeline
// described in operator architecture §16, using CUE as the
// transformation engine.
package autoconfig

import (
	"strings"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// Filter applies include/exclude rules to CUE evaluator output entries.
type Filter interface {
	Apply(
		entries []v1alpha1.EndpointEntry,
		tags map[string][]string,
		operationIDs map[string]string,
		spec v1alpha1.FilterSpec,
	) []v1alpha1.EndpointEntry
}

// NewFilter returns a Filter implementation.
func NewFilter() Filter {
	return &pathFilter{}
}

type pathFilter struct{}

func (f *pathFilter) Apply(
	entries []v1alpha1.EndpointEntry,
	tags map[string][]string,
	operationIDs map[string]string,
	spec v1alpha1.FilterSpec,
) []v1alpha1.EndpointEntry {
	if len(spec.IncludePaths) == 0 && len(spec.ExcludePaths) == 0 &&
		len(spec.IncludeTags) == 0 && len(spec.ExcludeTags) == 0 &&
		len(spec.IncludeMethods) == 0 && len(spec.ExcludeOperationIds) == 0 {
		return entries
	}

	result := make([]v1alpha1.EndpointEntry, 0, len(entries))
	for _, entry := range entries {
		key := entry.Endpoint + ":" + entry.Method
		entryTags := tags[key]
		opID := operationIDs[key]

		if !f.matchesInclude(entry, entryTags, spec) {
			continue
		}
		if f.matchesExclude(entry, entryTags, opID, spec) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func (f *pathFilter) matchesInclude(
	entry v1alpha1.EndpointEntry,
	entryTags []string,
	spec v1alpha1.FilterSpec,
) bool {
	pathIncluded := len(spec.IncludePaths) == 0 || matchesAnyPattern(entry.Endpoint, spec.IncludePaths)
	tagIncluded := len(spec.IncludeTags) == 0 || hasAnyTag(entryTags, spec.IncludeTags)
	methodIncluded := len(spec.IncludeMethods) == 0 || containsString(spec.IncludeMethods, entry.Method)
	return pathIncluded && tagIncluded && methodIncluded
}

func (f *pathFilter) matchesExclude(
	entry v1alpha1.EndpointEntry,
	entryTags []string,
	operationID string,
	spec v1alpha1.FilterSpec,
) bool {
	if matchesAnyPattern(entry.Endpoint, spec.ExcludePaths) {
		return true
	}
	if hasAnyTag(entryTags, spec.ExcludeTags) {
		return true
	}
	if operationID != "" && containsExact(spec.ExcludeOperationIds, operationID) {
		return true
	}
	return false
}

func matchesAnyPattern(path string, patterns []string) bool {
	for _, p := range patterns {
		if matchGlob(path, p) {
			return true
		}
	}
	return false
}

func matchGlob(path, pattern string) bool {
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(path, prefix+"/") || path == prefix
	}
	return path == pattern
}

func hasAnyTag(entryTags, filterTags []string) bool {
	tagSet := make(map[string]struct{}, len(entryTags))
	for _, t := range entryTags {
		tagSet[t] = struct{}{}
	}
	for _, ft := range filterTags {
		if _, ok := tagSet[ft]; ok {
			return true
		}
	}
	return false
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func containsExact(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
