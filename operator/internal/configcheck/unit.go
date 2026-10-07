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

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// Memo remembers verdicts by the content key of what was checked: the
// checksum of the rendered config, the edition it is validated as, and the
// check (lint or the full check). A verdict depends on nothing else, so a
// remembered one stands in for a run. The implementation decides how long it
// keeps one. A nil Memo remembers nothing.
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
	endpoints := slices.Clone(g.Endpoints)
	sortEndpoints(endpoints)
	policies, err := c.policiesFor(ctx, endpoints)
	if err != nil {
		return Verdict{}, err
	}
	if g.Override != nil {
		policies[policyKey(g.Override)] = g.Override
	}
	return c.lintInput(ctx, renderer.RenderInput{
		Gateway: g.Gateway, Endpoints: endpoints, Policies: policies, CEFallback: g.CEFallback,
	}, memo)
}

// CheckPolicy lints policy on its own: one synthetic endpoint on a default CE
// gateway, whose only backend references policy (policyAlone).
func (c *Checker) CheckPolicy(ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy,
	memo Memo) (Verdict, error) {
	return c.lintInput(ctx, policyAlone(policy), memo)
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
		if kept, ok := memo.Lookup(key); ok {
			return verdictFor(kept.OK, kept.Rejection, in, out), nil
		}
	}
	rejection, err := c.run(ctx, in, out, validate)
	if err != nil {
		return Verdict{}, err
	}
	if memo != nil {
		memo.Store(key, Verdict{OK: rejection == nil, Rejection: bounded(rejection)})
	}
	return verdictFor(rejection == nil, rejection, in, out), nil
}

// verdictFor is the verdict on out, rendered from in, for a check that
// accepted it (ok) or rejected it with rejection. It reads a bounded copy of
// the rejection, so a fresh run and a hit return the same text.
func verdictFor(ok bool, rejection *renderer.ValidationError, in renderer.RenderInput,
	out *renderer.RenderOutput) Verdict {
	if ok {
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
	return &cut
}
