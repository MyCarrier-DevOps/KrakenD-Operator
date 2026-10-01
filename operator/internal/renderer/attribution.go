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

package renderer

import "k8s.io/apimachinery/pkg/types"

// Attribution ties one krakend check finding to the rendered endpoint entry
// it names. Index is the entry's position in the rendered "endpoints" array,
// or -1 when the finding names no endpoint (root settings, plugins); Endpoint
// is then the zero value. Endpoint is also zero when Index is past the
// Sources it was attributed against.
type Attribution struct {
	Endpoint types.NamespacedName
	Index    int
	Message  string
}

// Attribute maps krakend check output back to the KrakenDEndpoints that
// produced the entries it names.
func Attribute(_ []byte, _ []types.NamespacedName, _ string) []Attribution { return nil }
