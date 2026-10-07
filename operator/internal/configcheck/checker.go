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
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// ValidatorVersion is the KrakenD minor version of the krakend binary the
// operator image pins (Dockerfile KRAKEND_IMAGE). Admission and the gateway
// controller validate every gateway with it, whatever its spec.version.
const ValidatorVersion = "2.13"

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

// Gather returns the render input the gateway controller publishes gw from:
// the endpoints that reference gw, with replace substituted or added by
// namespace/name, sorted by namespace/name; the policies they reference; and
// CE fallback as gw's status records it. PluginConfigMaps are left unset: they
// do not reach the validated config. Dragonfly is left unset too, so a Redis
// block that only a Dragonfly address would add to the controller's render is
// not part of what is checked here.
func (c *Checker) Gather(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	replace []v1alpha1.KrakenDEndpoint) (renderer.RenderInput, error) {
	return c.gather(ctx, gw, replace, nil)
}

// CheckRendered validates out, rendered from in, with the full check the
// gateway controller publishes behind (Validator.Validate: krakend check -t -n
// after the route check), answering from memo when it already judged the same
// content.
func (c *Checker) CheckRendered(ctx context.Context, in renderer.RenderInput,
	out *renderer.RenderOutput, memo Memo) (Verdict, error) {
	return c.remembered(ctx, in, out, modeValidate, c.validator.Validate, memo)
}

// CheckGateway lints gw's config: its current endpoints with replace
// substituted or added by namespace/name. A replace entry without
// spec.endpoints means that endpoint is removed: it renders nothing.
func (c *Checker) CheckGateway(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	replace []v1alpha1.KrakenDEndpoint) (Verdict, error) {
	return c.lintGathered(ctx, gw, replace, nil)
}

// CheckIsolated lints gw's root config with eps as its only endpoints.
func (c *Checker) CheckIsolated(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	eps []v1alpha1.KrakenDEndpoint) (Verdict, error) {
	endpoints := slices.Clone(eps)
	sortEndpoints(endpoints)
	policies, err := c.policiesFor(ctx, endpoints)
	if err != nil {
		return Verdict{}, err
	}
	return c.lint(ctx, renderer.RenderInput{
		Gateway: gw, Endpoints: endpoints, Policies: policies, CEFallback: ceFallback(gw),
	})
}

// SameConfig reports whether gw and old, two versions of one gateway, render
// the same config for the same edition from the same endpoints and policies.
// It renders in process: no validation slot is held and nothing is executed.
func (c *Checker) SameConfig(ctx context.Context, old, gw *v1alpha1.KrakenDGateway) (bool, error) {
	// Nothing read here leaves the Checker and the renderer never mutates its
	// inputs, so the cache's objects can be used without copying them.
	in, err := c.gather(ctx, gw, nil, nil, client.UnsafeDisableDeepCopy)
	if err != nil {
		return false, err
	}
	out, err := c.renderer.Render(in)
	if err != nil {
		return false, fmt.Errorf("rendering config: %w", err)
	}
	before := in
	before.Gateway, before.CEFallback = old, ceFallback(old)
	oldOut, err := c.renderer.Render(before)
	if err != nil {
		return false, fmt.Errorf("rendering config: %w", err)
	}
	return bytes.Equal(out.JSON, oldOut.JSON) &&
		renderer.EditionFor(gw, in.CEFallback) == renderer.EditionFor(old, before.CEFallback), nil
}

// CheckGatewayPolicy lints gw's config with policy in place of the stored
// policy of the same namespace/name.
func (c *Checker) CheckGatewayPolicy(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	policy *v1alpha1.KrakenDBackendPolicy) (Verdict, error) {
	return c.lintGathered(ctx, gw, nil, policy)
}

// policyAlone is the render input that checks policy on its own: one
// synthetic endpoint on a default CE gateway, whose only backend references
// policy.
func policyAlone(policy *v1alpha1.KrakenDBackendPolicy) renderer.RenderInput {
	const name = "policy-lint"
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: policy.Namespace},
		Spec:       v1alpha1.KrakenDGatewaySpec{Edition: v1alpha1.EditionCE},
	}
	ep := v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: policy.Namespace},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: name},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/" + name, Method: "GET",
				Backends: []v1alpha1.BackendSpec{{
					Host: []string{"http://" + name}, URLPattern: "/",
					PolicyRef: &v1alpha1.PolicyRef{Name: policy.Name},
				}},
			}},
		},
	}
	return renderer.RenderInput{
		Gateway:   gw,
		Endpoints: []v1alpha1.KrakenDEndpoint{ep},
		Policies:  map[string]*v1alpha1.KrakenDBackendPolicy{policyKey(policy): policy},
	}
}

// LintPolicy lints policy on its own (policyAlone). It catches a bad policy
// before anything references it. Its findings name no endpoint.
func (c *Checker) LintPolicy(ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy) (Verdict, error) {
	verdict, err := c.lint(ctx, policyAlone(policy))
	// The synthetic endpoint is not something the caller created: its
	// findings read as policy-level.
	for i := range verdict.Findings {
		verdict.Findings[i].Endpoint, verdict.Findings[i].Index = types.NamespacedName{}, -1
	}
	return verdict, err
}

// lintGathered lints what gather returns for gw, replace and override.
func (c *Checker) lintGathered(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	replace []v1alpha1.KrakenDEndpoint, override *v1alpha1.KrakenDBackendPolicy) (Verdict, error) {
	// Nothing read here leaves the Checker and the renderer never mutates its
	// inputs, so the cache's objects, and the caller's override, can be used
	// without copying them.
	in, err := c.gather(ctx, gw, replace, override, client.UnsafeDisableDeepCopy)
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
	rejection, err := c.run(ctx, in, out, validate)
	if err != nil {
		return Verdict{}, err
	}
	if rejection != nil {
		return Rejected(rejection, in, out), nil
	}
	return Verdict{OK: true}, nil
}

// run runs validate on out as the edition in is for, holding a slot. It
// returns the validator's rejection, or nil when it accepts out.
func (c *Checker) run(ctx context.Context, in renderer.RenderInput, out *renderer.RenderOutput,
	validate func(context.Context, []byte, v1alpha1.Edition) error) (*renderer.ValidationError, error) {
	edition := renderer.EditionFor(in.Gateway, in.CEFallback)
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	// The slot is freed even if validate panics (the manager recovers
	// panics), and before attribution, which needs no slot.
	err := func() error {
		defer func() { <-c.slots }()
		return validate(ctx, out.JSON, edition)
	}()
	var invalid *renderer.ValidationError
	if errors.As(err, &invalid) {
		return invalid, nil
	}
	return nil, err
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

// gather lists gw's endpoints, applies replace, and gathers their policies,
// with override (when not nil) in place of the stored policy of the same
// namespace/name.
func (c *Checker) gather(ctx context.Context, gw *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint,
	override *v1alpha1.KrakenDBackendPolicy, opts ...client.ListOption) (renderer.RenderInput, error) {
	var list v1alpha1.KrakenDEndpointList
	opts = append(opts, client.MatchingFields{fieldindex.EndpointGateway: gw.Namespace + "/" + gw.Name})
	if err := c.reader.List(ctx, &list, opts...); err != nil {
		return renderer.RenderInput{}, fmt.Errorf("listing endpoints of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	endpoints := substitute(list.Items, replace)
	sortEndpoints(endpoints)
	policies, err := c.policiesFor(ctx, endpoints)
	if err != nil {
		return renderer.RenderInput{}, err
	}
	if override != nil {
		policies[policyKey(override)] = override
	}
	return renderer.RenderInput{Gateway: gw, Endpoints: endpoints, Policies: policies, CEFallback: ceFallback(gw)}, nil
}

// policiesFor fetches every policy the endpoints reference, keyed by
// PolicyRef.PolicyKey. A missing policy is left out: the renderer then marks
// the endpoint invalid.
func (c *Checker) policiesFor(ctx context.Context,
	endpoints []v1alpha1.KrakenDEndpoint) (map[string]*v1alpha1.KrakenDBackendPolicy, error) {
	policies := make(map[string]*v1alpha1.KrakenDBackendPolicy)
	for i := range endpoints {
		for _, key := range fieldindex.EndpointPolicyKeys(&endpoints[i]) {
			if _, ok := policies[key]; ok {
				continue
			}
			ns, name, _ := strings.Cut(key, "/")
			var policy v1alpha1.KrakenDBackendPolicy
			if err := c.reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &policy); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("getting policy %s: %w", key, err)
			}
			policies[key] = &policy
		}
	}
	return policies, nil
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

// policyKey is the key policy is gathered under: the one a PolicyRef to it
// resolves to.
func policyKey(policy *v1alpha1.KrakenDBackendPolicy) string {
	return (&v1alpha1.PolicyRef{Name: policy.Name}).PolicyKey(policy.Namespace)
}

func sortEndpoints(endpoints []v1alpha1.KrakenDEndpoint) {
	slices.SortFunc(endpoints, func(a, b v1alpha1.KrakenDEndpoint) int {
		if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

// ceFallback reads CE fallback from gw's status, for an EE gateway only: the
// gateway controller never reports it for a CE gateway, whose condition may
// be a stale one from before an edition switch. The controller overrides it
// with its own in-reconcile license verdict; admission sees the last one
// recorded.
func ceFallback(gw *v1alpha1.KrakenDGateway) bool {
	return gw.Spec.Edition == v1alpha1.EditionEE &&
		meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded)
}
