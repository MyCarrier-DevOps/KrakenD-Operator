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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// ValidatorVersion is the KrakenD minor version of the krakend binary the
// operator image pins (Dockerfile KRAKEND_IMAGE). Admission and the gateway
// controller validate every gateway with it, whatever its spec.version.
const ValidatorVersion = "2.13"

// Checker judges a gateway's root, its endpoints and policies on their own, a
// group of them, and its whole render. Every validation holds one of a fixed
// number of slots shared by all callers in the pod.
type Checker struct {
	reader    client.Reader
	renderer  renderer.Renderer
	validator renderer.Validator
	slots     chan struct{}
	tracer    trace.Tracer
}

// New returns a Checker that reads endpoints and policies through reader and
// runs at most slots validations at a time. Each check is a span of tracer;
// a nil tracer records none.
func New(reader client.Reader, r renderer.Renderer, v renderer.Validator, slots int, tracer trace.Tracer) *Checker {
	return &Checker{
		reader: reader, renderer: r, validator: v, slots: make(chan struct{}, max(slots, 1)), tracer: tracer,
	}
}

// Gather returns the render input the gateway controller publishes gw from:
// the endpoints that reference gw, with replace substituted or added by
// namespace/name, sorted by namespace/name; the policies they reference; and
// CE fallback as gw's status records it. PluginConfigMaps are left unset: they
// do not reach the validated config. Dragonfly is left unset too, so a Redis
// block that only a Dragonfly address would add to the controller's render is
// not part of what is checked here.
func (c *Checker) Gather(ctx context.Context, gw *v1alpha1.KrakenDGateway,
	replace []v1alpha1.KrakenDEndpoint) (_ renderer.RenderInput, retErr error) {
	ctx, span := c.start(ctx, "configcheck.Gather", gw)
	defer func() { tracing.End(span, retErr) }()
	return c.gather(ctx, gw, replace)
}

// CheckRendered validates out, rendered from in, with the full check the
// gateway controller publishes behind (Validator.Validate: krakend check -t -n
// after the route check), answering from memo when it already judged the same
// content.
func (c *Checker) CheckRendered(ctx context.Context, in renderer.RenderInput,
	out *renderer.RenderOutput, memo Memo) (v Verdict, retErr error) {
	ctx, span := c.start(ctx, "configcheck.CheckRendered", in.Gateway)
	defer func() { endCheck(span, v.OK, retErr) }()
	return c.remembered(ctx, in, out, modeValidate, c.validator.Validate, memo)
}

// SameConfig reports whether gw and old, two versions of one gateway, render
// the same config for the same edition from the same endpoints and policies.
// It renders in process: no validation slot is held and nothing is executed.
func (c *Checker) SameConfig(ctx context.Context, old, gw *v1alpha1.KrakenDGateway) (_ bool, retErr error) {
	ctx, span := c.start(ctx, "configcheck.SameConfig", gw)
	defer func() { tracing.End(span, retErr) }()
	// Nothing read here leaves the Checker and the renderer never mutates its
	// inputs, so the cache's objects can be used without copying them.
	in, err := c.gather(ctx, gw, nil, client.UnsafeDisableDeepCopy)
	if err != nil {
		return false, err
	}
	out, err := c.renderer.Render(in)
	if err != nil {
		return false, fmt.Errorf("rendering config: %w", err)
	}
	before := in
	before.Gateway, before.CEFallback = old, CEFallback(old)
	oldOut, err := c.renderer.Render(before)
	if err != nil {
		return false, fmt.Errorf("rendering config: %w", err)
	}
	return bytes.Equal(out.JSON, oldOut.JSON) &&
		renderer.EditionFor(gw, in.CEFallback) == renderer.EditionFor(old, before.CEFallback), nil
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

// run runs validate on out as the edition in is for, holding a slot. It
// returns the validator's rejection, or nil when it accepts out.
func (c *Checker) run(ctx context.Context, in renderer.RenderInput, out *renderer.RenderOutput,
	validate func(context.Context, []byte, v1alpha1.Edition) error) (*renderer.ValidationError, error) {
	edition := renderer.EditionFor(in.Gateway, in.CEFallback)
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	// The slot is freed even if validate panics (the manager recovers
	// panics).
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

// acquire takes a validation slot, giving up when ctx ends. The wait is a
// span of its own, so a check queued behind others shows as such.
func (c *Checker) acquire(ctx context.Context) (retErr error) {
	_, span := tracing.Start(ctx, c.tracer, "configcheck.slot")
	defer func() { tracing.End(span, retErr) }()
	select {
	case c.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for a validation slot: %w", ctx.Err())
	}
}

// gather lists gw's endpoints, applies replace, and gathers their policies.
func (c *Checker) gather(ctx context.Context, gw *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint,
	opts ...client.ListOption) (renderer.RenderInput, error) {
	var list v1alpha1.KrakenDEndpointList
	opts = append(opts, client.MatchingFields{fieldindex.EndpointGateway: gw.Namespace + "/" + gw.Name})
	if err := c.reader.List(ctx, &list, opts...); err != nil {
		return renderer.RenderInput{}, fmt.Errorf("listing endpoints of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	return c.inputFor(ctx, gw, substitute(list.Items, replace), nil, CEFallback(gw))
}

// inputFor is the render input of gw with endpoints as its only endpoints,
// sorted in place by namespace/name, and the policies they reference, with
// override (when not nil) in place of the stored policy of the same
// namespace/name.
func (c *Checker) inputFor(ctx context.Context, gw *v1alpha1.KrakenDGateway, endpoints []v1alpha1.KrakenDEndpoint,
	override *v1alpha1.KrakenDBackendPolicy, fallback bool) (renderer.RenderInput, error) {
	sortEndpoints(endpoints)
	policies, err := c.policiesFor(ctx, endpoints)
	if err != nil {
		return renderer.RenderInput{}, err
	}
	if override != nil {
		policies[policyKey(override)] = override
	}
	return renderer.RenderInput{Gateway: gw, Endpoints: endpoints, Policies: policies, CEFallback: fallback}, nil
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

// CEFallback reads CE fallback from gw's status, for an EE gateway only: the
// gateway controller never reports it for a CE gateway, whose condition may
// be a stale one from before an edition switch. The controller overrides it
// with its own in-reconcile license verdict; admission sees the last one
// recorded.
func CEFallback(gw *v1alpha1.KrakenDGateway) bool {
	return gw.Spec.Edition == v1alpha1.EditionEE &&
		meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded)
}

// start starts the span of a check of gw.
func (c *Checker) start(ctx context.Context, name string, gw *v1alpha1.KrakenDGateway) (context.Context, trace.Span) {
	return tracing.Start(ctx, c.tracer, name, trace.WithAttributes(tracing.Object("KrakenDGateway", gw)...))
}

// endCheck records whether the check passed and ends its span. A rejected
// config is a verdict, not an error: the span's status stays unset.
func endCheck(span trace.Span, ok bool, err error) {
	if err == nil {
		span.SetAttributes(attribute.Bool("configcheck.ok", ok))
	}
	tracing.End(span, err)
}
