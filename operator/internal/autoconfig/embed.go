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

import "embed"

//go:embed cue/*.cue
var embeddedCUEFS embed.FS

// EmbeddedCUEDefinitions returns the operator's built-in default CUE
// definitions as a map of filename to content. These are used when the
// krakend-cue-definitions ConfigMap does not exist in the namespace.
func EmbeddedCUEDefinitions() (map[string]string, error) {
	entries, err := embeddedCUEFS.ReadDir("cue")
	if err != nil {
		return nil, err
	}
	defs := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := embeddedCUEFS.ReadFile("cue/" + e.Name())
		if err != nil {
			return nil, err
		}
		defs[e.Name()] = string(data)
	}
	return defs, nil
}
