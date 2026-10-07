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
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// decision is what the config stage concludes about the newest render.
type decision struct {
	// output is the render to apply: the gathered endpoints less excluded.
	// It is nil when failure is set.
	output *renderer.RenderOutput
	// excluded are the endpoints that fail validation on their own, with why.
	excluded map[types.NamespacedName]configcheck.EndpointVerdict
	// judged says every endpoint was checked on its own, or passed as part of
	// the whole render, so one missing from excluded passes.
	judged bool
	// failure is set when nothing can be applied: the gateway root fails on
	// its own, or the endpoints fail only together.
	failure *gatewayFailure
}

// gatewayFailure is ConfigValid's verdict on a render none of which can be
// applied. It blames no endpoint.
type gatewayFailure struct {
	reason, message string
}

// combinedFailureMessage is ConfigValid's message when every endpoint passes
// on its own but the gateway's config fails with them together. It quotes
// nothing: that output quotes the endpoints' values, which the gateway's
// readers may not be allowed to read, so it goes to the operator log.
const combinedFailureMessage = "Every endpoint passes krakend check on its own, but the gateway's config fails " +
	"it with them together, so the last applied config keeps serving. The check's output quotes the endpoints' " +
	"values and is only in the operator log (\"the gateway's config fails krakend check only together\")."

// decide judges the newest render, full, which is not the applied config:
//  1. the gateway root on its own: when it fails, nothing more is judged and
//     no endpoint is blamed;
//  2. the render as a whole, with the full check;
//  3. when that fails, each endpoint on its own (judgeEndpoints). Those that
//     fail are excluded and the rest is applied. When none fails, the
//     endpoints fail only together, which is the gateway's failure;
//  4. the render without the excluded endpoints, with the full check, unless
//     it is the applied config: when it still fails, the endpoints fail only
//     together.
//
// Every check answers from the gateway's verdict memo when it already judged
// the same content. An error means a check could not run: nothing is
// decided, and the verdicts judged before it are kept for the next pass.
func (r *KrakenDGatewayReconciler) decide(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, in renderer.RenderInput,
	full *renderer.RenderOutput, edition v1alpha1.Edition,
) (d decision, err error) {
	key := client.ObjectKeyFromObject(gw)
	pass := r.verdicts.begin(key)
	defer func() { r.verdicts.end(key, pass, err != nil) }()
	counted := countedPass{pass}

	root, err := r.Checker.CheckRoot(ctx, configcheck.Root{Gateway: gw, CEFallback: in.CEFallback}, counted)
	if err != nil {
		return decision{}, err
	}
	if !root.OK {
		return decision{failure: rootFailure(root)}, nil
	}
	whole, err := r.Checker.CheckRendered(ctx, in, full, counted)
	if err != nil {
		return decision{}, err
	}
	if whole.OK {
		return decision{output: full, judged: true}, nil
	}
	excluded, err := r.judgeEndpoints(ctx, gw, in, in.Endpoints, counted)
	if err != nil {
		return decision{}, err
	}
	if len(excluded) == 0 {
		logCombinedFailure(ctx, whole)
		return decision{judged: true, failure: combinedFailure()}, nil
	}
	rest := in
	rest.Endpoints = without(in.Endpoints, excluded)
	out, err := r.Renderer.Render(rest)
	if err != nil {
		return decision{}, fmt.Errorf("rendering config: %w", err)
	}
	if !isApplied(gw, out, edition) {
		var safety configcheck.Verdict
		if safety, err = r.Checker.CheckRendered(ctx, rest, out, counted); err != nil {
			return decision{}, err
		}
		if !safety.OK {
			logCombinedFailure(ctx, safety)
			return decision{excluded: excluded, judged: true, failure: combinedFailure()}, nil
		}
	}
	return decision{output: out, excluded: excluded, judged: true}, nil
}

// judgeEndpoints checks each of suspects, endpoints of in, on its own
// (configcheck.CheckEndpoint) and returns those that fail, with why. An
// endpoint whose policy is missing is not judged: the render already leaves
// it out.
func (r *KrakenDGatewayReconciler) judgeEndpoints(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, in renderer.RenderInput,
	suspects []v1alpha1.KrakenDEndpoint, memo configcheck.Memo,
) (map[types.NamespacedName]configcheck.EndpointVerdict, error) {
	excluded := map[types.NamespacedName]configcheck.EndpointVerdict{}
	for i := range suspects {
		ep := &suspects[i]
		v, err := r.Checker.CheckEndpoint(ctx, configcheck.EndpointUnit{
			Gateway: gw, Endpoint: ep, Policies: in.Policies, CEFallback: in.CEFallback,
		}, memo)
		if err != nil {
			return nil, err
		}
		if !v.OK {
			excluded[client.ObjectKeyFromObject(ep)] = v
		}
	}
	return excluded, nil
}

// without returns endpoints less those in excluded.
func without(endpoints []v1alpha1.KrakenDEndpoint,
	excluded map[types.NamespacedName]configcheck.EndpointVerdict) []v1alpha1.KrakenDEndpoint {
	return slices.DeleteFunc(slices.Clone(endpoints), func(ep v1alpha1.KrakenDEndpoint) bool {
		_, ok := excluded[client.ObjectKeyFromObject(&ep)]
		return ok
	})
}

// rootFailure is ConfigValid's verdict when the gateway root fails on its
// own. The root is the gateway's, so its output is shown.
func rootFailure(v configcheck.Verdict) *gatewayFailure {
	return &gatewayFailure{reason: v1alpha1.ReasonGatewayRootInvalid,
		message: "The gateway root (the gateway with no endpoint) fails krakend check, so no endpoint was " +
			"judged and the last applied config keeps serving:\n" + strings.TrimSpace(v.Output)}
}

// combinedFailure is ConfigValid's verdict when the endpoints fail only
// together.
func combinedFailure() *gatewayFailure {
	return &gatewayFailure{reason: v1alpha1.ReasonCombinedConfigInvalid, message: combinedFailureMessage}
}

// logCombinedFailure logs the output of a full check that fails only with
// several endpoints together.
func logCombinedFailure(ctx context.Context, v configcheck.Verdict) {
	logf.FromContext(ctx).Info("the gateway's config fails krakend check only together", "output", v.Output)
}
