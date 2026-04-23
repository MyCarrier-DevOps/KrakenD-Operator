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
	"encoding/json"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
)

// ExtractComponentSchemas parses the OpenAPI spec JSON and returns
// the components/schemas map with lowercased keys. The keys are
// lowercased to match the ref values produced by the CUE template.
// Returns nil when no schemas are present or the data cannot be parsed.
func ExtractComponentSchemas(specData []byte) map[string]runtime.RawExtension {
	var spec struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(specData, &spec); err != nil {
		return nil
	}
	if len(spec.Components.Schemas) == 0 {
		return nil
	}
	result := make(map[string]runtime.RawExtension, len(spec.Components.Schemas))
	for name, raw := range spec.Components.Schemas {
		result[strings.ToLower(name)] = runtime.RawExtension{Raw: raw}
	}
	return result
}
