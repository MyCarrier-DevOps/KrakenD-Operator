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

package controller

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"k8s.io/apimachinery/pkg/types"
)

// schemaConflictMessages returns, for each endpoint that defines a component
// schema differently from the endpoint the gateway's documentation takes it
// from, the message its Accepted condition carries. Schema names are sorted, so
// identical renders give identical messages.
func schemaConflictMessages(conflicts []renderer.SchemaConflict) map[types.NamespacedName]string {
	byEndpoint := map[types.NamespacedName][]string{}
	sorted := slices.Clone(conflicts)
	slices.SortStableFunc(sorted, func(a, b renderer.SchemaConflict) int { return cmp.Compare(a.Schema, b.Schema) })
	for _, c := range sorted {
		byEndpoint[c.Endpoint] = append(byEndpoint[c.Endpoint],
			fmt.Sprintf("%q (published from %s)", c.Schema, c.Winner))
	}
	msgs := make(map[types.NamespacedName]string, len(byEndpoint))
	for nn, schemas := range byEndpoint {
		msgs[nn] = "Served, but the gateway documentation shows another endpoint's definition of component schemas " +
			listed(schemas) + "; rename them in the OpenAPI spec to publish both"
	}
	return msgs
}
