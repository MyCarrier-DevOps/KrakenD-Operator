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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// Memo remembers what a check judged about a content, by its key: the
// checksum of the rendered config, the edition it is validated as, and the
// check (lint or the full check). It keeps the content's judgement only: OK,
// or the rejection's Output bounded to maxStoredOutput, with its Stage.
// Masked depends on the input a config was rendered from, which the content
// does not fix, so it is rebuilt on every call from that call's own render.
// What a Memo receives is a partial verdict, and it is never returned as is.
// The implementation decides how long it keeps one. A nil Memo remembers
// nothing.
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
// in for the policy of its namespace/name. Dragonfly is the Dragonfly state
// the gateway controller detected (Root.Dragonfly); nil renders none.
type EndpointUnit struct {
	Gateway    *v1alpha1.KrakenDGateway
	Endpoint   *v1alpha1.KrakenDEndpoint
	Policies   map[string]*v1alpha1.KrakenDBackendPolicy
	Override   *v1alpha1.KrakenDBackendPolicy
	CEFallback bool
	Dragonfly  *renderer.DragonflyState
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
// endpoint's own rejection cut to limit bytes, or the policies at fault by
// name, never their content. It is "" for an OK verdict.
func (v EndpointVerdict) Message(limit int) string {
	names := make([]string, len(v.Policies))
	for i, p := range v.Policies {
		names[i] = p.String()
	}
	switch {
	case v.OK:
		return ""
	case v.Reason == v1alpha1.ReasonPolicyInvalid && v.PoliciesFailAlone:
		return fmt.Sprintf("references KrakenDBackendPolicy %s, which fails krakend check on its own",
			strings.Join(names, ", "))
	case v.Reason == v1alpha1.ReasonPolicyInvalid:
		return fmt.Sprintf("fails krakend check only together with KrakenDBackendPolicy %s of another namespace, "+
			"whose content is not shown", strings.Join(names, ", "))
	}
	return "fails krakend check on its own: " + Verdict{Output: v.Output}.Excerpt(limit)
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
func (c *Checker) CheckRoot(ctx context.Context, r Root, memo Memo) (v Verdict, retErr error) {
	ctx, span := c.start(ctx, "configcheck.CheckRoot", r.Gateway)
	defer func() { endCheck(span, v.OK, retErr) }()
	return c.lintInput(ctx, renderer.RenderInput{
		Gateway: r.Gateway, CEFallback: r.CEFallback, Dragonfly: r.Dragonfly,
	}, memo)
}

// CheckGroup lints g.
func (c *Checker) CheckGroup(ctx context.Context, g Group, memo Memo) (v Verdict, retErr error) {
	ctx, span := c.start(ctx, "configcheck.CheckGroup", g.Gateway)
	defer func() { endCheck(span, v.OK, retErr) }()
	in, err := c.inputFor(ctx, g.Gateway, slices.Clone(g.Endpoints), g.Override, g.CEFallback)
	if err != nil {
		return Verdict{}, err
	}
	return c.lintInput(ctx, in, memo)
}

// CheckPolicy lints policy on its own: one synthetic endpoint on a default CE
// gateway, whose only backend references policy (policyAlone).
func (c *Checker) CheckPolicy(ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy,
	memo Memo) (v Verdict, retErr error) {
	ctx, span := tracing.Start(ctx, c.tracer, "configcheck.CheckPolicy",
		trace.WithAttributes(tracing.Object("KrakenDBackendPolicy", policy)...))
	defer func() { endCheck(span, v.OK, retErr) }()
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
func (c *Checker) CheckEndpoint(ctx context.Context, u EndpointUnit, memo Memo) (v EndpointVerdict, retErr error) {
	ctx, span := c.start(ctx, "configcheck.CheckEndpoint", u.Gateway)
	span.SetAttributes(attribute.String("configcheck.endpoint", client.ObjectKeyFromObject(u.Endpoint).String()))
	defer func() { endCheck(span, v.OK, retErr) }()
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
		Policies: policies, CEFallback: u.CEFallback, Dragonfly: u.Dragonfly,
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
	if own.OK {
		return EndpointVerdict{Reason: v1alpha1.ReasonPolicyInvalid, Policies: policyNames(foreign)}, nil
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
// the content decides: acceptance, or the bounded output and stage of the
// rejection. Masked is not part of it, so a hit shares nothing with the
// verdict it returned before.
func (c *Checker) remembered(ctx context.Context, in renderer.RenderInput, out *renderer.RenderOutput, mode string,
	validate func(context.Context, []byte, v1alpha1.Edition) error, memo Memo) (Verdict, error) {
	key := contentKey(out, renderer.EditionFor(in.Gateway, in.CEFallback), mode)
	if memo != nil {
		if kept, ok := memo.Lookup(key); ok && judged(kept) {
			return Verdict{OK: kept.OK, Output: kept.Output, Stage: kept.Stage}, nil
		}
	}
	rejection, err := c.run(ctx, in, out, validate)
	if err != nil {
		return Verdict{}, err
	}
	verdict := verdictFor(rejection)
	if memo != nil {
		memo.Store(key, verdict)
	}
	return verdict, nil
}

// unitPolicies returns the policies u's endpoint references: u.Policies, or
// those read through the reader, with u.Override in place of the policy of
// its namespace/name. The caller's map is never changed.
func (c *Checker) unitPolicies(ctx context.Context, u EndpointUnit) (map[string]*v1alpha1.KrakenDBackendPolicy, error) {
	policies := u.Policies
	if policies == nil {
		read, err := c.policiesFor(ctx, []v1alpha1.KrakenDEndpoint{*u.Endpoint})
		if err != nil {
			return nil, err
		}
		policies = read
	}
	if u.Override == nil {
		return policies, nil
	}
	policies = maps.Clone(policies)
	policies[policyKey(u.Override)] = u.Override
	return policies, nil
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
		out[key] = EmptyPolicy(p.Namespace, p.Name)
	}
	return out
}

// EmptyPolicy returns a policy with the identity namespace/name and no
// content: what stands for a policy whose content is not rendered. It must
// stay present, because a render leaves out every entry of an endpoint one of
// whose policies is missing.
func EmptyPolicy(namespace, name string) *v1alpha1.KrakenDBackendPolicy {
	return &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// policyNames names the policies of keys ("namespace/name").
func policyNames(keys []string) []types.NamespacedName {
	names := make([]types.NamespacedName, len(keys))
	for i, key := range keys {
		ns, name, _ := strings.Cut(key, "/")
		names[i] = types.NamespacedName{Namespace: ns, Name: name}
	}
	return names
}

// judged reports whether a memo entry holds a judgement: exactly one of an
// acceptance and a rejection. An entry that is neither, or both, is not
// trusted: it is a miss.
func judged(kept Verdict) bool {
	rejected := kept.Output != "" || kept.Stage != renderer.StageUnknown
	return kept.OK != rejected
}

// verdictFor is the verdict for a check that accepted its config (nil
// rejection) or rejected it with rejection. It keeps at most maxStoredOutput
// of the output, so a fresh run and a memo hit return the same text.
func verdictFor(rejection *renderer.ValidationError) Verdict {
	if rejection == nil {
		return Verdict{OK: true}
	}
	return Verdict{Output: TruncateEllipsis(rejection.Output, maxStoredOutput), Stage: rejection.Stage}
}
