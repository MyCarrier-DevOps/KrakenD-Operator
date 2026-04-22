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
	"fmt"
)

// httpMethods lists the OpenAPI 3.x operation keys within a path-item.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// StripServers removes the OpenAPI `servers` field from the root document
// and from any path-item or operation overrides. The KrakenD gateway is
// the externally-visible server, so upstream service URLs must not leak
// into the generated configuration or documentation.
//
// The function accepts JSON or YAML and always returns canonical JSON.
// If the document cannot be decoded it is returned unchanged with the
// decode error so the caller can choose to log-and-continue.
func StripServers(specData []byte) ([]byte, error) {
	root, err := decodeSpec(specData)
	if err != nil {
		return specData, fmt.Errorf("decoding spec: %w", err)
	}

	delete(root, "servers")

	if paths, ok := root["paths"].(map[string]any); ok {
		for _, item := range paths {
			pathItem, ok := item.(map[string]any)
			if !ok {
				continue
			}
			delete(pathItem, "servers")
			for _, method := range httpMethods {
				opVal, ok := pathItem[method]
				if !ok {
					continue
				}
				op, ok := opVal.(map[string]any)
				if !ok {
					continue
				}
				delete(op, "servers")
			}
		}
	}

	out, err := json.Marshal(root)
	if err != nil {
		return specData, fmt.Errorf("re-encoding spec: %w", err)
	}
	return out, nil
}
