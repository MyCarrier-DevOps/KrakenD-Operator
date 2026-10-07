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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// combinedFailureMessage is ConfigValid's message when the endpoints that pass
// on their own fail the gateway's config together. An endpoint that fails on
// its own may already have been excluded, so it does not say every endpoint
// passes. It quotes nothing: that output quotes the endpoints' values, which
// the gateway's readers may not be allowed to read, so it goes to the
// operator log.
const combinedFailureMessage = "The endpoints that pass krakend check on their own fail it together, so the last " +
	"applied config keeps serving; any endpoint that fails on its own is excluded first. The check's output " +
	"quotes the endpoints' values and is only in the operator log " +
	"(\"the gateway's config fails krakend check only together\")."

// decide judges the newest render, full, in the order that blames each
// object only for its own content:
//  1. when full is the applied config, it passed when it was applied, so
//     only the endpoints that lost an entry in it are judged, each on its
//     own: a check of full says nothing about the entries it left out. When
//     none fails, full is kept as it is;
//  2. the gateway root on its own: when it fails, nothing more is judged and
//     no endpoint is blamed;
//  3. the render as a whole, with the full check;
//  4. each endpoint the whole check does not vouch for, on its own
//     (judgeEndpoints): every endpoint when it failed, otherwise those that
//     lost an entry in full. Those that fail are excluded, and the render
//     without them is checked once more with the full check. When that still
//     fails, or the whole check failed and nothing was excluded, the
//     endpoints fail only together, which is the gateway's failure.
//
// Every check answers from the gateway's verdict memo when it already judged
// the same content, so a gateway whose inputs did not change runs none. The
// checks of one object on its own are counted in
// config_validation_failures_total (countedPass); the whole render's are not.
// An error means a check could not run: nothing is decided, and the verdicts
// judged before it are kept for the next pass.
func (r *KrakenDGatewayReconciler) decide(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, in renderer.RenderInput,
	full *renderer.RenderOutput, edition v1alpha1.Edition,
) (d decision, err error) {
	key := client.ObjectKeyFromObject(gw)
	pass := r.verdicts.begin(key)
	defer func() { r.verdicts.end(key, pass, err != nil) }()
	counted := countedPass{pass}

	// A check of a render says nothing of the endpoints it left out of it.
	rendered := configcheck.Verdict{OK: true, Masked: configcheck.MaskedEndpoints(full)}
	masked := suspectsOf(in.Endpoints, rendered)
	if isApplied(gw, full, edition) {
		var excluded map[types.NamespacedName]configcheck.EndpointVerdict
		if excluded, err = r.judgeEndpoints(ctx, gw, in, masked, counted); err != nil {
			return decision{}, err
		}
		if len(excluded) == 0 {
			return decision{output: full, judged: true}, nil
		}
	}
	root, err := r.Checker.CheckRoot(ctx, configcheck.Root{
		Gateway: gw, CEFallback: in.CEFallback, Dragonfly: in.Dragonfly,
	}, counted)
	if err != nil {
		return decision{}, err
	}
	if !root.OK {
		return decision{failure: rootFailure(root)}, nil
	}
	whole, err := r.Checker.CheckRendered(ctx, in, full, pass)
	if err != nil {
		return decision{}, err
	}
	whole.Masked = rendered.Masked
	excluded, err := r.judgeEndpoints(ctx, gw, in, suspectsOf(in.Endpoints, whole), counted)
	if err != nil {
		return decision{}, err
	}
	if len(excluded) == 0 {
		if whole.OK {
			return decision{output: full, judged: true}, nil
		}
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
		if safety, err = r.Checker.CheckRendered(ctx, rest, out, pass); err != nil {
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
// (configcheck.CheckEndpoint) and returns those that fail, with why. Only a
// failing verdict is returned: an excluded endpoint's condition takes its
// reason from the verdict, and an OK one has none, which the API server
// rejects. An endpoint whose policy is missing is not judged: the render
// already leaves it out.
func (r *KrakenDGatewayReconciler) judgeEndpoints(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, in renderer.RenderInput,
	suspects []v1alpha1.KrakenDEndpoint, memo configcheck.Memo,
) (map[types.NamespacedName]configcheck.EndpointVerdict, error) {
	excluded := map[types.NamespacedName]configcheck.EndpointVerdict{}
	for i := range suspects {
		ep := &suspects[i]
		v, err := r.Checker.CheckEndpoint(ctx, configcheck.EndpointUnit{
			Gateway: gw, Endpoint: ep, Policies: in.Policies, CEFallback: in.CEFallback, Dragonfly: in.Dragonfly,
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

// suspectsOf returns the endpoints that verdict, the check of them as a group,
// did not judge (Verdict.Suspect), in their order in endpoints.
func suspectsOf(endpoints []v1alpha1.KrakenDEndpoint, verdict configcheck.Verdict) []v1alpha1.KrakenDEndpoint {
	return slices.DeleteFunc(slices.Clone(endpoints), func(ep v1alpha1.KrakenDEndpoint) bool {
		return !verdict.Suspect(client.ObjectKeyFromObject(&ep))
	})
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

// maxExcludedNamed is how many excluded endpoints EndpointsExcluded names.
const maxExcludedNamed = 10

// reportExclusions sets the gateway's EndpointsExcluded condition and its
// excluded-endpoints gauge from the endpoints' Accepted verdicts after this
// pass: decided holds what this pass set (nil: removed), and an endpoint it
// did not decide keeps its stored verdict. Both therefore follow the
// endpoints on every pass, including one that applies nothing, and after an
// operator restart. applied says the pass's render is the gateway's applied
// config; otherwise the message says the endpoints are left out when the
// gateway next applies one. A Warning event marks the condition appearing or its
// message changing.
func (r *KrakenDGatewayReconciler) reportExclusions(gw *v1alpha1.KrakenDGateway,
	endpoints []v1alpha1.KrakenDEndpoint, decided map[types.NamespacedName]*metav1.Condition, applied bool) {
	var names []string
	counts := map[string]int{}
	for i := range endpoints {
		key := client.ObjectKeyFromObject(&endpoints[i])
		cond, ok := decided[key]
		if !ok {
			cond = meta.FindStatusCondition(endpoints[i].Status.Conditions, v1alpha1.ConditionAccepted)
		}
		if isExclusion(cond) {
			names = append(names, key.String())
			counts[cond.Reason]++
		}
	}
	recordExcludedEndpoints(gw, counts)
	if len(names) == 0 {
		meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionEndpointsExcluded)
		return
	}
	slices.Sort(names)
	listed, more := names, ""
	if len(names) > maxExcludedNamed {
		listed, more = names[:maxExcludedNamed], fmt.Sprintf(" (+%d more)", len(names)-maxExcludedNamed)
	}
	served := "are not served"
	if !applied {
		served = "will not be served when the gateway next applies its config"
	}
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionEndpointsExcluded,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonInvalidEndpointsExcluded,
		Message: fmt.Sprintf("%d KrakenDEndpoint(s) fail validation and %s: %s%s",
			len(names), served, strings.Join(listed, ", "), more),
	}
	prev := meta.FindStatusCondition(gw.Status.Conditions, cond.Type).DeepCopy()
	meta.SetStatusCondition(&gw.Status.Conditions, cond)
	if prev == nil || prev.Status != metav1.ConditionTrue || prev.Message != cond.Message {
		r.Recorder.Event(gw, corev1.EventTypeWarning, cond.Reason, cond.Message)
	}
}
