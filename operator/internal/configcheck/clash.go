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


package configcheck

import (
	"k8s.io/apimachinery/pkg/types"

	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// RouteConflicts is what a render of a gateway leaves out.
type RouteConflicts struct {
	Lost   map[types.NamespacedName][]renderer.EntryConflict
	Capped bool
}

// Clash is an entry the router cannot serve next to another endpoint's.
type Clash struct {
	Loser            types.NamespacedName
	Method, Endpoint string
	Winner           types.NamespacedName
	Detail           string
}

// NewClashes returns the router clashes of after that before does not have.
func NewClashes(_, _ RouteConflicts, _ map[types.NamespacedName]bool) []Clash {
	return nil
}
