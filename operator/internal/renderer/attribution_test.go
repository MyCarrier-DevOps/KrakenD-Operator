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

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestAttribute_MapsKrakendCheckOutputToSources(t *testing.T) {
	src := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: "ns", Name: name} }
	cases := []struct {
		name      string
		rendered  string
		output    string
		wantIndex []int
	}{
		{
			name:     "lint pointers",
			rendered: `{"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/b","method":"GET"},{"endpoint":"/c","method":"GET"}]}`,
			output: "Parsing configuration file: /tmp/krakend-config-1.json\n" +
				"ERROR linting the configuration file:\tjsonschema validation failed with 'file:///etc/krakend/schema.json#'\n" +
				"- at '/endpoints/1/backend/0/extra_config/qos~1circuit-breaker/interval': got string, want integer\n" +
				"- at '/endpoints/2/extra_config': additional properties 'totally/unknown' not allowed\n",
			wantIndex: []int{1, 2},
		},
		{
			name:      "method and gin-style path",
			rendered:  `{"endpoints":[{"endpoint":"/ok","method":"GET"},{"endpoint":"/a/{id}","method":"GET"}]}`,
			output:    "ERROR parsing the configuration file:\t'/tmp/k.json': undefined output param 'other'! endpoint: GET /a/:id, backend: 0. input: [id], output: [other]\n",
			wantIndex: []int{1},
		},
		{
			name:      "method and brace path",
			rendered:  `{"endpoints":[{"endpoint":"/ok/{p}","method":"GET"},{"endpoint":"/__debug/{x}","method":"POST"}]}`,
			output:    "ERROR parsing the configuration file:\t'/tmp/k.json': ignoring the 'POST /__debug/{x}' endpoint, since it is invalid!!!\n",
			wantIndex: []int{1},
		},
		{
			name:      "route conflict names both parties",
			rendered:  `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/{name}","method":"GET"}]}`,
			output:    "ERROR testing the configuration file:\t':name' in new path '/a/:name' conflicts with existing wildcard ':id' in existing prefix '/a/:id'\n",
			wantIndex: []int{0, 1},
		},
		{
			name:      "not attributable",
			rendered:  `{"endpoints":[{"endpoint":"/ok","method":"GET"}]}`,
			output:    "ERROR parsing the configuration file:\t'/tmp/k.json': decoding failed due to the following error(s):\n\n'timeout' time: unknown unit\n",
			wantIndex: []int{-1, -1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources := []types.NamespacedName{src("e0"), src("e1"), src("e2")}
			got := Attribute([]byte(tc.rendered), sources, tc.output)
			var indices []int
			for _, a := range got {
				indices = append(indices, a.Index)
				wantSource := types.NamespacedName{}
				if a.Index >= 0 {
					wantSource = sources[a.Index]
				}
				if a.Endpoint != wantSource {
					t.Errorf("finding %q attributed to %s, want %s", a.Message, a.Endpoint, wantSource)
				}
				if a.Message == "" {
					t.Error("every attribution must carry its output line")
				}
			}
			slices.Sort(indices)
			if !slices.Equal(indices, tc.wantIndex) {
				t.Errorf("attributed indices = %v, want %v", indices, tc.wantIndex)
			}
		})
	}
}

func TestAttribute_IndexBeyondSourcesIsUnnamed(t *testing.T) {
	got := Attribute([]byte(`{"endpoints":[{"endpoint":"/a","method":"GET"}]}`), nil,
		"- at '/endpoints/0/endpoint': bad\n")
	if len(got) != 1 || got[0].Index != 0 || got[0].Endpoint != (types.NamespacedName{}) {
		t.Errorf("Attribute = %+v, want one finding at index 0 with no source", got)
	}
}

func TestAttribute_RouterErrorsRespectMethods(t *testing.T) {
	cases := []struct {
		name      string
		rendered  string
		output    string
		wantIndex []int
	}{
		{
			name: "conflict ignores another method on the same path",
			rendered: `{"endpoints":[{"endpoint":"/users/{id}","method":"GET"},` +
				`{"endpoint":"/users/{id}","method":"PUT"},{"endpoint":"/users/{userId}","method":"GET"}]}`,
			output: "ERROR testing the configuration file:\t':userId' in new path '/users/:userId' " +
				"conflicts with existing wildcard ':id' in existing prefix '/users/:id'\n",
			wantIndex: []int{0, 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources := []types.NamespacedName{{Name: "a"}, {Name: "b"}, {Name: "c"}}
			var indices []int
			for _, a := range Attribute([]byte(tc.rendered), sources, tc.output) {
				indices = append(indices, a.Index)
			}
			slices.Sort(indices)
			if !slices.Equal(indices, tc.wantIndex) {
				t.Errorf("blamed indices = %v, want %v", indices, tc.wantIndex)
			}
		})
	}
}
