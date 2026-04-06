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

package renderer

import (
	"sort"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

// endpointKey uniquely identifies a KrakenD endpoint by path and method.
type endpointKey struct {
	Endpoint string
	Method   string
}

// flatEndpoint pairs an EndpointEntry with its source KrakenDEndpoint metadata.
type flatEndpoint struct {
	Entry  v1alpha1.EndpointEntry
	Source types.NamespacedName
	// CreationTimestamp is used for conflict resolution (oldest wins).
	CreationTimestampUnix int64
}

// flattenEndpoints flattens all KrakenDEndpoint specs into individual entries,
// detects conflicts (same path+method from different CRs), and returns the
// deduplicated list plus sets of conflicted and invalid endpoints.
func flattenEndpoints(
	endpoints []v1alpha1.KrakenDEndpoint,
	policies map[string]*v1alpha1.KrakenDBackendPolicy,
) (flat []flatEndpoint, conflicted, invalid map[types.NamespacedName]struct{}) {
	conflicted = make(map[types.NamespacedName]struct{})
	invalid = make(map[types.NamespacedName]struct{})

	// Group entries by (endpoint, method) to detect conflicts
	type entryGroup struct {
		entries []flatEndpoint
	}
	groups := make(map[endpointKey]*entryGroup)

	for i := range endpoints {
		ep := &endpoints[i]
		nn := types.NamespacedName{Name: ep.Name, Namespace: ep.Namespace}

		// Validate policy references
		hasInvalidPolicy := false
		for _, entry := range ep.Spec.Endpoints {
			for _, be := range entry.Backends {
				if be.PolicyRef != nil {
					if _, ok := policies[be.PolicyRef.Name]; !ok {
						hasInvalidPolicy = true
						break
					}
				}
			}
			if hasInvalidPolicy {
				break
			}
		}
		if hasInvalidPolicy {
			invalid[nn] = struct{}{}
			continue
		}

		for _, entry := range ep.Spec.Endpoints {
			key := endpointKey{Endpoint: entry.Endpoint, Method: entry.Method}
			if groups[key] == nil {
				groups[key] = &entryGroup{}
			}
			groups[key].entries = append(groups[key].entries, flatEndpoint{
				Entry:                 entry,
				Source:                nn,
				CreationTimestampUnix: ep.CreationTimestamp.Unix(),
			})
		}
	}

	// Resolve conflicts: for each group with entries from multiple CRs,
	// keep the oldest CR's entry and mark the rest as conflicted.

	for _, group := range groups {
		if len(group.entries) <= 1 {
			flat = append(flat, group.entries...)
			continue
		}

		// Sort by creation timestamp (oldest first), then by name for determinism
		sort.Slice(group.entries, func(i, j int) bool {
			if group.entries[i].CreationTimestampUnix != group.entries[j].CreationTimestampUnix {
				return group.entries[i].CreationTimestampUnix < group.entries[j].CreationTimestampUnix
			}
			return group.entries[i].Source.String() < group.entries[j].Source.String()
		})

		// Keep the winner (oldest), mark the rest as conflicted
		flat = append(flat, group.entries[0])
		for _, loser := range group.entries[1:] {
			conflicted[loser.Source] = struct{}{}
		}
	}

	// Sort result by endpoint path then method for deterministic output
	sort.Slice(flat, func(i, j int) bool {
		if flat[i].Entry.Endpoint != flat[j].Entry.Endpoint {
			return flat[i].Entry.Endpoint < flat[j].Entry.Endpoint
		}
		return flat[i].Entry.Method < flat[j].Entry.Method
	})

	return flat, conflicted, invalid
}

// buildEndpointJSON converts a flat endpoint entry to its KrakenD JSON representation.
func buildEndpointJSON(
	entry v1alpha1.EndpointEntry,
	policies map[string]*v1alpha1.KrakenDBackendPolicy,
) map[string]any {
	ep := map[string]any{
		"endpoint": entry.Endpoint,
		"method":   entry.Method,
	}

	if entry.Timeout != nil {
		ep["timeout"] = entry.Timeout.Duration.String()
	}
	if entry.CacheTTL != nil {
		ep["cache_ttl"] = entry.CacheTTL.Duration.String()
	}
	if entry.OutputEncoding != "" {
		ep["output_encoding"] = entry.OutputEncoding
	}
	if entry.ConcurrentCalls != nil {
		ep["concurrent_calls"] = *entry.ConcurrentCalls
	}
	if len(entry.InputHeaders) > 0 {
		sorted := make([]string, len(entry.InputHeaders))
		copy(sorted, entry.InputHeaders)
		sort.Strings(sorted)
		ep["input_headers"] = sorted
	}
	if len(entry.InputQueryStrings) > 0 {
		sorted := make([]string, len(entry.InputQueryStrings))
		copy(sorted, entry.InputQueryStrings)
		sort.Strings(sorted)
		ep["input_query_strings"] = sorted
	}

	// Endpoint-level extra_config
	if entry.ExtraConfig != nil && entry.ExtraConfig.Raw != nil {
		ec := buildEndpointExtraConfig(entry.ExtraConfig.Raw)
		if ec != nil {
			ep["extra_config"] = ec
		}
	}

	// Backends
	var backends []any
	for _, be := range entry.Backends {
		b := buildBackendJSON(be, policies)
		backends = append(backends, b)
	}
	ep["backend"] = backends

	return ep
}

// buildBackendJSON converts a BackendSpec to its KrakenD JSON representation.
func buildBackendJSON(
	backend v1alpha1.BackendSpec,
	policies map[string]*v1alpha1.KrakenDBackendPolicy,
) map[string]any {
	b := map[string]any{
		"url_pattern": backend.URLPattern,
	}

	if len(backend.Host) > 0 {
		sorted := make([]string, len(backend.Host))
		copy(sorted, backend.Host)
		sort.Strings(sorted)
		b["host"] = sorted
	}
	if backend.Method != "" {
		b["method"] = backend.Method
	}
	if backend.Encoding != "" {
		b["encoding"] = backend.Encoding
	}
	if len(backend.Allow) > 0 {
		sorted := make([]string, len(backend.Allow))
		copy(sorted, backend.Allow)
		sort.Strings(sorted)
		b["allow"] = sorted
	}
	if len(backend.Mapping) > 0 {
		b["mapping"] = backend.Mapping
	}

	ec := buildBackendExtraConfig(backend, policies)
	if ec != nil {
		b["extra_config"] = ec
	}

	return b
}
