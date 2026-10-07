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
	"fmt"
	"slices"

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
// as the edition CEFallback makes it.
type Root struct {
	Gateway    *v1alpha1.KrakenDGateway
	CEFallback bool
}

// CheckRoot lints r on its own.
func (c *Checker) CheckRoot(ctx context.Context, r Root, memo Memo) (Verdict, error) {
	return c.lintInput(ctx, renderer.RenderInput{Gateway: r.Gateway, CEFallback: r.CEFallback}, memo)
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
// already judged the same content.
func (c *Checker) lintInput(ctx context.Context, in renderer.RenderInput, memo Memo) (Verdict, error) {
	out, err := c.renderer.Render(in)
	if err != nil {
		return Verdict{}, fmt.Errorf("rendering config: %w", err)
	}
	return c.remembered(ctx, in, out, modeLint, c.validator.Lint, memo)
}

// remembered runs validate on out, rendered from in, unless memo already
// holds the verdict on out's content in mode, and stores a fresh verdict in
// memo. An error is never stored: the check did not judge.
func (c *Checker) remembered(ctx context.Context, in renderer.RenderInput, out *renderer.RenderOutput, mode string,
	validate func(context.Context, []byte, v1alpha1.Edition) error, memo Memo) (Verdict, error) {
	key := contentKey(out, renderer.EditionFor(in.Gateway, in.CEFallback), mode)
	if memo != nil {
		if v, ok := memo.Lookup(key); ok {
			return v, nil
		}
	}
	v, err := c.check(ctx, in, out, validate)
	v.Output = TruncateEllipsis(v.Output, maxStoredOutput)
	if err == nil && memo != nil {
		memo.Store(key, v)
	}
	return v, err
}
