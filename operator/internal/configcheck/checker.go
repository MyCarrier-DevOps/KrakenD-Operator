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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
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
func (c *Checker) CheckGateway(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	replace []v1alpha1.KrakenDEndpoint) (Verdict, error) {
	in, err := c.gather(ctx, gw, replace, client.UnsafeDisableDeepCopy)
	if err != nil {
		return Verdict{}, err
	}
	return c.lint(ctx, in)
}

func (c *Checker) lint(ctx context.Context, in renderer.RenderInput) (Verdict, error) {
	out, err := c.renderer.Render(in)
	if err != nil {
		return Verdict{}, fmt.Errorf("rendering config: %w", err)
	}
	return c.check(ctx, in, out, c.validator.Lint)
}

// check runs validate on out as the edition in is for, holding a slot.
func (c *Checker) check(ctx context.Context, in renderer.RenderInput, out *renderer.RenderOutput,
	validate func(context.Context, []byte, v1alpha1.Edition) error) (Verdict, error) {
	if err := c.acquire(ctx); err != nil {
		return Verdict{}, err
	}
	err := validate(ctx, out.JSON, renderer.EditionFor(in.Gateway, in.CEFallback))
	<-c.slots
	var invalid *renderer.ValidationError
	if errors.As(err, &invalid) {
		atts := renderer.Attribute(out.JSON, out.Sources, invalid.Output)
		return Verdict{Findings: findingsFrom(atts, out.JSON, in.Endpoints, invalid.Output)}, nil
	}
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{OK: true}, nil
}

// acquire takes a validation slot, giving up when ctx ends.
func (c *Checker) acquire(ctx context.Context) error {
	select {
	case c.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for a validation slot: %w", ctx.Err())
	}
}

// gather lists gw's endpoints and applies replace.
func (c *Checker) gather(ctx context.Context, gw *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint,
	opts ...client.ListOption) (renderer.RenderInput, error) {
	var list v1alpha1.KrakenDEndpointList
	opts = append(opts, client.MatchingFields{fieldindex.EndpointGateway: gw.Namespace + "/" + gw.Name})
	if err := c.reader.List(ctx, &list, opts...); err != nil {
		return renderer.RenderInput{}, fmt.Errorf("listing endpoints of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	endpoints := substitute(list.Items, replace)
	sortEndpoints(endpoints)
	return renderer.RenderInput{Gateway: gw, Endpoints: endpoints, CEFallback: ceFallback(gw)}, nil
}

// substitute returns current with each replace entry in place of the endpoint
// with the same namespace/name, or appended when there is none. A replace
// entry without entries removes that endpoint: it stands for a deletion.
func substitute(current, replace []v1alpha1.KrakenDEndpoint) []v1alpha1.KrakenDEndpoint {
	out := slices.Clone(current)
	for _, r := range replace {
		i := slices.IndexFunc(out, func(e v1alpha1.KrakenDEndpoint) bool {
			return e.Namespace == r.Namespace && e.Name == r.Name
		})
		if len(r.Spec.Endpoints) == 0 {
			if i >= 0 {
				out = slices.Delete(out, i, i+1)
			}
			continue
		}
		if i >= 0 {
			out[i] = r
		} else {
			out = append(out, r)
		}
	}
	return out
}

func sortEndpoints(endpoints []v1alpha1.KrakenDEndpoint) {
	slices.SortFunc(endpoints, func(a, b v1alpha1.KrakenDEndpoint) int {
		if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

// ceFallback reads CE fallback from gw's status. The gateway controller
// overrides it with its own in-reconcile license verdict; admission sees the
// last one recorded.
func ceFallback(gw *v1alpha1.KrakenDGateway) bool {
	return meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded)
}
