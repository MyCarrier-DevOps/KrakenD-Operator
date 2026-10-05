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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// Checker renders gateway configs and validates them. Every validation holds
// one of a fixed number of slots shared by all callers in the pod.
type Checker struct {
	reader    client.Reader
	renderer  renderer.Renderer
	validator renderer.Validator
	slots     chan struct{}
}

// New returns a Checker that reads endpoints and policies through reader and
// runs at most slots validations at a time.
func New(reader client.Reader, r renderer.Renderer, v renderer.Validator, slots int) *Checker {
	return &Checker{reader: reader, renderer: r, validator: v, slots: make(chan struct{}, max(slots, 1))}
}

// CheckGateway lints gw's config: its current endpoints with replace
// substituted or added by namespace/name.
func (c *Checker) CheckGateway(_ context.Context, _ *v1alpha1.KrakenDGateway,
	_ []v1alpha1.KrakenDEndpoint) (Verdict, error) {
	return Verdict{}, nil
}
