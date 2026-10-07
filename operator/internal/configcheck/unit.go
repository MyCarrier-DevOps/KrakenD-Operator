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
	"fmt"
	"maps"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// Memo remembers what a check judged about a content, by its key: the
// checksum of the rendered config, the edition it is validated as, and the
// check (lint or the full check). It keeps the content's judgement only: OK,
// or the rejection bounded to maxStoredOutput with its refusals by index.
// Findings, refusals, endpoint names and Masked depend on the input a config
// was rendered from, which the content does not fix, so they are rebuilt on
// every call from that call's own input and render. What a Memo receives is a
// partial verdict, and it is never returned as is. The implementation decides
// how long it keeps one. A nil Memo remembers nothing.
type Memo interface {
	Lookup(key string) (Verdict, bool)
	Store(key string, v Verdict)
}

// maxStoredOutput bounds the rejection output a verdict keeps, so a memo of
// many verdicts stays small. No message shows more than a few KiB of it.
const maxStoredOutput = 16 << 10

// The checks a content key names.
const (
	modeLint     = "lint"
	modeValidate = "validate"
)

// contentKey identifies the check of out, as edition, in mode.
func contentKey(out *renderer.RenderOutput, edition v1alpha1.Edition, mode string) string {
	return out.Checksum + "/" + string(edition) + "/" + mode
}

// MaskedEndpoints returns the endpoints that lost an entry in out, sorted by
// namespace/name: a check of out says nothing about the entries it left out.
func MaskedEndpoints(out *renderer.RenderOutput) []types.NamespacedName {
	names := slices.Collect(maps.Keys(out.EntryConflicts))
	slices.SortFunc(names, func(a, b types.NamespacedName) int {
		return cmp.Or(strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	return names
}

// Group is a gateway root with Endpoints as its only endpoints, as the
// edition CEFallback makes it. The policies they reference are read through
// the Checker's reader, with Override, when not nil, in place of the stored
// policy of its namespace/name.
type Group struct {
	Gateway    *v1alpha1.KrakenDGateway
	Endpoints  []v1alpha1.KrakenDEndpoint
	Override   *v1alpha1.KrakenDBackendPolicy
	CEFallback bool
}

// EndpointUnit is one KrakenDEndpoint to judge on its own: the gateway root
// with Endpoint as its only endpoint and the policies it references, as the
// edition CEFallback makes it. Policies, when not nil, are those policies by
// PolicyRef.PolicyKey, as a caller that gathered them holds them; otherwise
// they are read through the Checker's reader. Override, when not nil, stands
// in for the policy of its namespace/name.
type EndpointUnit struct {
	Gateway    *v1alpha1.KrakenDGateway
	Endpoint   *v1alpha1.KrakenDEndpoint
	Policies   map[string]*v1alpha1.KrakenDBackendPolicy
	Override   *v1alpha1.KrakenDBackendPolicy
	CEFallback bool
}

// EndpointVerdict is the verdict on one KrakenDEndpoint judged on its own.
type EndpointVerdict struct {
	OK bool
	// Reason is v1alpha1.ReasonEndpointInvalid or
	// v1alpha1.ReasonPolicyInvalid when OK is false.
	Reason string
	// Policies are the policies a PolicyInvalid verdict is about.
	Policies []types.NamespacedName
	// PoliciesFailAlone says those policies fail krakend check on their own;
	// otherwise the endpoint fails only together with them.
	PoliciesFailAlone bool
	// Output is an EndpointInvalid verdict's rejection: what the check of the
	// gateway root with the endpoint printed, with every policy of another
	// namespace rendered empty.
	Output string
}

// Message says why v is not OK in words its endpoint's owner may read: the
// policies at fault by name, never their content.
func (v EndpointVerdict) Message(int) string {
	names := make([]string, len(v.Policies))
	for i, p := range v.Policies {
		names[i] = p.String()
	}
	if v.Reason == v1alpha1.ReasonPolicyInvalid && v.PoliciesFailAlone {
		return fmt.Sprintf("references KrakenDBackendPolicy %s, which fails krakend check on its own",
			strings.Join(names, ", "))
	}
	return ""
}

// Root is a gateway's root on its own: what Gateway renders with no endpoint,
// as the edition CEFallback makes it. Dragonfly is the Dragonfly state the
// gateway controller detected, which sets the Redis address; nil renders
// none, as admission renders.
type Root struct {
	Gateway    *v1alpha1.KrakenDGateway
	CEFallback bool
	Dragonfly  *renderer.DragonflyState
}

// CheckRoot lints r on its own.
func (c *Checker) CheckRoot(ctx context.Context, r Root, memo Memo) (Verdict, error) {
	return c.lintInput(ctx, renderer.RenderInput{
		Gateway: r.Gateway, CEFallback: r.CEFallback, Dragonfly: r.Dragonfly,
	}, memo)
}

// CheckGroup lints g.
func (c *Checker) CheckGroup(ctx context.Context, g Group, memo Memo) (Verdict, error) {
	in, err := c.inputFor(ctx, g.Gateway, slices.Clone(g.Endpoints), g.Override, g.CEFallback)
	if err != nil {
		return Verdict{}, err
	}
	return c.lintInput(ctx, in, memo)
}

// CheckPolicy lints policy on its own: one synthetic endpoint on a default CE
// gateway, whose only backend references policy (policyAlone).
func (c *Checker) CheckPolicy(ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy,
	memo Memo) (Verdict, error) {
	return c.lintInput(ctx, policyAlone(policy), memo)
}

// CheckEndpoint judges u.Endpoint on its own, in the order that blames each
// object only for its own content:
//  1. each policy it references, alone (CheckPolicy): one that fails makes
//     the verdict PolicyInvalid, naming it;
//  2. the gateway root with the endpoint and its policies (lint);
//  3. when that fails and the endpoint references a policy of another
//     namespace, the same check with those policies rendered empty, because
//     the endpoint's owner may not read them. Still failing is the
//     endpoint's own fault (EndpointInvalid, quoting this check); passing
//     means it fails only together with those policies (PolicyInvalid).
//
// An endpoint that references a policy that does not exist is not judged:
// no render includes it, and the endpoint controller reports the missing
// policy. Its verdict is OK. Every check answers from memo when it already
// judged the same content.
func (c *Checker) CheckEndpoint(ctx context.Context, u EndpointUnit, memo Memo) (EndpointVerdict, error) {
	policies, err := c.unitPolicies(ctx, u)
	if err != nil {
		return EndpointVerdict{}, err
	}
	keys := fieldindex.EndpointPolicyKeys(u.Endpoint)
	var failing []types.NamespacedName
	for _, key := range keys {
		policy, ok := policies[key]
		if !ok {
			return EndpointVerdict{OK: true}, nil
		}
		alone, err := c.CheckPolicy(ctx, policy, memo)
		if err != nil {
			return EndpointVerdict{}, err
		}
		if !alone.OK {
			failing = append(failing, client.ObjectKeyFromObject(policy))
		}
	}
	if len(failing) > 0 {
		return EndpointVerdict{Reason: v1alpha1.ReasonPolicyInvalid, Policies: failing, PoliciesFailAlone: true}, nil
	}
	in := renderer.RenderInput{
		Gateway: u.Gateway, Endpoints: []v1alpha1.KrakenDEndpoint{*u.Endpoint},
		Policies: policies, CEFallback: u.CEFallback,
	}
	whole, err := c.lintInput(ctx, in, memo)
	if err != nil || whole.OK {
		return EndpointVerdict{OK: whole.OK}, err
	}
	foreign := foreignPolicies(u.Endpoint.Namespace, keys)
	if len(foreign) == 0 {
		return EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: whole.Output}, nil
	}
	in.Policies = emptied(policies, foreign)
	own, err := c.lintInput(ctx, in, memo)
	if err != nil {
		return EndpointVerdict{}, err
	}
	return EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: own.Output}, nil
}

// lintInput renders in and lints the render, answering from memo when it
// already judged the same content. The verdict's Masked comes from this
// render, never from the memo.
func (c *Checker) lintInput(ctx context.Context, in renderer.RenderInput, memo Memo) (Verdict, error) {
	out, err := c.renderer.Render(in)
	if err != nil {
		return Verdict{}, fmt.Errorf("rendering config: %w", err)
	}
	v, err := c.remembered(ctx, in, out, modeLint, c.validator.Lint, memo)
	if err != nil {
		return Verdict{}, err
	}
	v.Masked = MaskedEndpoints(out)
	return v, nil
}

// remembered runs validate on out, rendered from in, unless memo already
// holds the verdict on out's content in mode, and stores a fresh one in memo.
// An error is never stored: the check did not judge. The memo holds only what
// the content decides (acceptance, and the bounded rejection with its refusals
// by index); the findings, refusals and their endpoint names are rebuilt from
// in and out on every call, so a hit never names another input's endpoints
// and shares nothing with the verdict it returned before.
func (c *Checker) remembered(ctx context.Context, in renderer.RenderInput, out *renderer.RenderOutput, mode string,
	validate func(context.Context, []byte, v1alpha1.Edition) error, memo Memo) (Verdict, error) {
	key := contentKey(out, renderer.EditionFor(in.Gateway, in.CEFallback), mode)
	if memo != nil {
		// An entry that is neither OK nor a rejection judged nothing: a miss.
		if kept, ok := memo.Lookup(key); ok && (kept.OK || kept.Rejection != nil) {
			return verdictFor(kept.Rejection, in, out), nil
		}
	}
	rejection, err := c.run(ctx, in, out, validate)
	if err != nil {
		return Verdict{}, err
	}
	if memo != nil {
		memo.Store(key, Verdict{OK: rejection == nil, Rejection: bounded(rejection)})
	}
	return verdictFor(rejection, in, out), nil
}

// unitPolicies returns the policies u's endpoint references, read through the
// reader.
func (c *Checker) unitPolicies(ctx context.Context, u EndpointUnit) (map[string]*v1alpha1.KrakenDBackendPolicy, error) {
	return c.policiesFor(ctx, []v1alpha1.KrakenDEndpoint{*u.Endpoint})
}

// foreignPolicies returns the keys ("namespace/name") of the policies outside
// namespace.
func foreignPolicies(namespace string, keys []string) []string {
	var foreign []string
	for _, key := range keys {
		if ns, _, _ := strings.Cut(key, "/"); ns != namespace {
			foreign = append(foreign, key)
		}
	}
	return foreign
}

// emptied returns policies with each of keys replaced by a policy of the
// same namespace/name and no content. The policy must stay present: a render
// leaves out every entry of an endpoint one of whose policies is missing.
func emptied(policies map[string]*v1alpha1.KrakenDBackendPolicy,
	keys []string) map[string]*v1alpha1.KrakenDBackendPolicy {
	out := maps.Clone(policies)
	for _, key := range keys {
		p := policies[key]
		out[key] = &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
	}
	return out
}

// verdictFor is the verdict on out, rendered from in, for a check that
// accepted it (nil rejection) or rejected it with rejection. It reads a
// bounded copy of the rejection, so a fresh run and a hit return the same
// text.
func verdictFor(rejection *renderer.ValidationError, in renderer.RenderInput,
	out *renderer.RenderOutput) Verdict {
	if rejection == nil {
		return Verdict{OK: true}
	}
	return Rejected(bounded(rejection), in, out)
}

// bounded returns a copy of rejection that shares nothing with it and keeps
// at most maxStoredOutput of its output, nil for a nil rejection.
func bounded(rejection *renderer.ValidationError) *renderer.ValidationError {
	if rejection == nil {
		return nil
	}
	cut := *rejection
	cut.Output = TruncateEllipsis(rejection.Output, maxStoredOutput)
	cut.Refusals = slices.Clone(rejection.Refusals)
	for i := range cut.Refusals {
		cut.Refusals[i].Message = TruncateEllipsis(cut.Refusals[i].Message, maxStoredOutput)
		cut.Refusals[i].Indices = slices.Clone(cut.Refusals[i].Indices)
	}
	return &cut
}
