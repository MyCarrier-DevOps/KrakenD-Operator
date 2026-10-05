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

package webhook

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// renderChecks are the four checks of the verdict ratchet, each over a
// proposed change: the gateway with it, and without it, then the same two on
// the isolated baseline used when the gateway already fails.
type renderChecks struct {
	after, before, isoAfter, isoBefore func(context.Context) (configcheck.Verdict, error)
}

// bindCheck fixes the gateway and endpoints a check runs on.
func bindCheck(
	run func(context.Context, *v1alpha1.KrakenDGateway, []v1alpha1.KrakenDEndpoint) (configcheck.Verdict, error),
	gw *v1alpha1.KrakenDGateway, eps []v1alpha1.KrakenDEndpoint,
) func(context.Context) (configcheck.Verdict, error) {
	return func(ctx context.Context) (configcheck.Verdict, error) { return run(ctx, gw, eps) }
}

// ratchetRender rejects a change only when it turns a passing config into a
// failing one. It runs after, then before; when before fails too the failure
// is a warning (preexisting words it from before's verdict) unless the change
// fails on the isolated baseline where its own baseline passed. Without an
// isolated baseline (isoAfter nil) a preexisting failure is only a warning. A check that
// cannot run is a 500 with no warning: the request is not judged. deny builds
// the rejection from a failing verdict.
func ratchetRender(
	ctx context.Context, c renderChecks, deny func(configcheck.Verdict) error,
	preexisting func(before configcheck.Verdict) string,
) (admission.Warnings, error) {
	after, err := c.after(ctx)
	if err != nil || after.OK {
		return nil, checkErr(err)
	}
	before, err := c.before(ctx)
	if err != nil {
		return nil, checkErr(err)
	}
	if before.OK {
		return nil, deny(after)
	}
	warning := admission.Warnings{preexisting(before)}
	if c.isoAfter == nil {
		return warning, nil
	}
	isoAfter, err := c.isoAfter(ctx)
	if err != nil {
		return nil, checkErr(err)
	}
	if isoAfter.OK {
		return warning, nil
	}
	isoBefore, err := c.isoBefore(ctx)
	if err != nil {
		return nil, checkErr(err)
	}
	if isoBefore.OK {
		return nil, deny(isoAfter)
	}
	return warning, nil
}

// checkGatewayRender validates gw's config. A new gateway must render on its
// own. An update is rejected only when it turns a passing config (the root
// with its endpoints) into a failing one; when the config already fails, only
// the root alone is judged.
func checkGatewayRender(
	ctx context.Context, chk ConfigChecker, old, gw *v1alpha1.KrakenDGateway,
) (admission.Warnings, error) {
	if old == nil {
		root, err := chk.CheckIsolated(ctx, gw, nil)
		if err != nil || root.OK {
			return nil, checkErr(err)
		}
		return nil, gatewayRenderDenial(gw, root)
	}
	return ratchetRender(ctx, renderChecks{
		after:     bindCheck(chk.CheckGateway, gw, nil),
		before:    bindCheck(chk.CheckGateway, old, nil),
		isoAfter:  bindCheck(chk.CheckIsolated, gw, nil),
		isoBefore: bindCheck(chk.CheckIsolated, old, nil),
	},
		func(v configcheck.Verdict) error { return gatewayRenderDenial(gw, v) },
		func(before configcheck.Verdict) string {
			return "the gateway's config already fails validation: " + before.Summary(warningLimit)
		})
}

// gatewayRenderDenial rejects gw: gateway-root findings on spec.config, the
// endpoints the change breaks on spec. The renderer builds the root from
// spec.config (timeout, extraConfig, router), so that is where a user looks.
// The first maxEntryCauses root findings are causes of their own, cut to the
// warning limit; the rest go on spec.config as a bounded summary.
func gatewayRenderDenial(gw *v1alpha1.KrakenDGateway, verdict configcheck.Verdict) error {
	var errs field.ErrorList
	var root, endpoints configcheck.Verdict
	for _, f := range verdict.Findings {
		switch {
		case f.Endpoint.Name != "":
			endpoints.Findings = append(endpoints.Findings, f)
		case len(errs) < maxEntryCauses:
			errs = append(errs, field.Invalid(field.NewPath("spec", "config"), field.OmitValueType{},
				truncate(f.Message, warningLimit)))
		default:
			root.Findings = append(root.Findings, f)
		}
	}
	if len(root.Findings) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "config"), field.OmitValueType{},
			"more gateway config failures: "+root.Summary(warningLimit)))
	}
	if len(endpoints.Findings) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec"), field.OmitValueType{},
			"with this change these endpoints fail krakend check: "+endpoints.Summary(warningLimit)))
	}
	return invalid("KrakenDGateway", gw.Name, errs)
}

// checkPolicyRender validates policy on its own and in every gateway that
// renders it. It rejects a request only for a pass-to-fail change: a policy
// that already failed alone (old) is judged by its gateways, and a gateway
// already failing without the change gets a warning instead.
func checkPolicyRender(
	ctx context.Context, c client.Reader, chk ConfigChecker, old, policy *v1alpha1.KrakenDBackendPolicy,
) (admission.Warnings, error) {
	alone, err := chk.LintPolicy(ctx, policy)
	if err != nil {
		return nil, checkErr(err)
	}
	if !alone.OK {
		oldFailed := false
		if old != nil {
			oldAlone, err := chk.LintPolicy(ctx, old)
			if err != nil {
				return nil, checkErr(err)
			}
			oldFailed = !oldAlone.OK
		}
		if !oldFailed {
			return nil, invalid("KrakenDBackendPolicy", policy.Name, field.ErrorList{field.Invalid(
				field.NewPath("spec"), field.OmitValueType{}, "fails krakend check on its own: "+messages(alone))})
		}
	}
	gateways, err := gatewaysUsing(ctx, c, policy)
	if err != nil {
		return nil, unavailable(err)
	}
	// krakend check accepts Enterprise-only namespaces, and KrakenD CE then
	// ignores them silently; only a new or changed raw is judged.
	var drops []renderer.CEDrop
	if old == nil || !equality.Semantic.DeepEqual(old.Spec.Raw, policy.Spec.Raw) {
		drops = eeOnlyNamespacesIn(policy.Spec.Raw, renderer.LevelBackend)
	}
	var errs field.ErrorList
	var warnings admission.Warnings
	omitted := 0
	// cause records a gateway the policy cannot go to; past maxEntryCauses the
	// rest are only counted.
	cause := func(e *field.Error) {
		if len(errs) < maxEntryCauses {
			errs = append(errs, e)
			return
		}
		omitted++
	}
	for i := range gateways {
		gw := &gateways[i]
		if gw.Spec.Edition == v1alpha1.EditionCE && len(drops) > 0 {
			cause(field.Invalid(field.NewPath("spec", "raw"), describeDrops(drops),
				fmt.Sprintf("Enterprise-only extra_config: gateway %s/%s runs CE, which ignores it silently",
					gw.Namespace, gw.Name)))
			continue
		}
		w, err := ratchetRender(ctx, renderChecks{
			after:  bindPolicyCheck(chk.CheckGatewayPolicy, gw, policy),
			before: bindCheck(chk.CheckGateway, gw, nil),
		},
			func(after configcheck.Verdict) error {
				cause(field.Invalid(field.NewPath("spec"), field.OmitValueType{},
					fmt.Sprintf("breaks gateway %s/%s: %s", gw.Namespace, gw.Name, after.Summary(warningLimit))))
				return errPolicyBreaksGateway
			},
			func(before configcheck.Verdict) string {
				return fmt.Sprintf("gateway %s/%s already fails validation: %s",
					gw.Namespace, gw.Name, before.Summary(warningLimit))
			})
		if err != nil && !errors.Is(err, errPolicyBreaksGateway) {
			return nil, err
		}
		warnings = append(warnings, w...)
	}
	if omitted > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec"), field.OmitValueType{},
			fmt.Sprintf("the policy also fails %d more gateways", omitted)))
	}
	return warnings, invalid("KrakenDBackendPolicy", policy.Name, errs)
}

// describeDrops lists what a CE render drops: a namespace, with the keys CE
// does not honor when it honors the rest of the block.
func describeDrops(drops []renderer.CEDrop) string {
	parts := make([]string, 0, len(drops))
	for _, d := range drops {
		if len(d.Keys) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%s)", d.Namespace, strings.Join(d.Keys, ", ")))
			continue
		}
		parts = append(parts, d.Namespace)
	}
	return strings.Join(parts, ", ")
}

// errPolicyBreaksGateway tells checkPolicyRender's loop that a gateway's
// ratchet denied the policy; the cause itself is already collected.
var errPolicyBreaksGateway = errors.New("policy breaks gateway")

// bindPolicyCheck fixes the gateway and the policy a check runs on.
func bindPolicyCheck(
	run func(context.Context, *v1alpha1.KrakenDGateway, *v1alpha1.KrakenDBackendPolicy) (configcheck.Verdict, error),
	gw *v1alpha1.KrakenDGateway, policy *v1alpha1.KrakenDBackendPolicy,
) func(context.Context) (configcheck.Verdict, error) {
	return func(ctx context.Context) (configcheck.Verdict, error) { return run(ctx, gw, policy) }
}

// messages joins a verdict's messages without their locations: a policy's
// lint findings point into a synthetic endpoint the user never wrote.
func messages(v configcheck.Verdict) string {
	parts := make([]string, 0, len(v.Findings))
	for _, f := range v.Findings {
		parts = append(parts, f.Message)
	}
	return truncate(strings.Join(parts, "; "), warningLimit)
}

// gatewaysUsing returns the gateways of the endpoints that reference policy,
// each once, sorted by namespace/name. Gateways that no longer exist are
// skipped.
func gatewaysUsing(
	ctx context.Context, c client.Reader, policy *v1alpha1.KrakenDBackendPolicy,
) ([]v1alpha1.KrakenDGateway, error) {
	var eps v1alpha1.KrakenDEndpointList
	if err := c.List(ctx, &eps, client.UnsafeDisableDeepCopy,
		client.MatchingFields{fieldindex.EndpointPolicy: policy.Namespace + "/" + policy.Name}); err != nil {
		return nil, fmt.Errorf("listing endpoints that reference policy %s/%s: %w", policy.Namespace, policy.Name, err)
	}
	keys := map[types.NamespacedName]struct{}{}
	for i := range eps.Items {
		ep := &eps.Items[i]
		keys[types.NamespacedName{
			Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace), Name: ep.Spec.GatewayRef.Name,
		}] = struct{}{}
	}
	sorted := make([]types.NamespacedName, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	gateways := make([]v1alpha1.KrakenDGateway, 0, len(sorted))
	for _, key := range sorted {
		var gw v1alpha1.KrakenDGateway
		if err := c.Get(ctx, key, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("getting gateway %s: %w", key, err)
		}
		gateways = append(gateways, gw)
	}
	return gateways, nil
}

// versionEchoLimit bounds, in bytes, the spec.version a warning quotes: the
// CRD does not bound it.
const versionEchoLimit = 64

// versionWarning warns, when spec.version is set or changed, that gw runs a
// KrakenD minor version other than the one admission and the controller
// validate with: their checks may not match what that version accepts.
func versionWarning(gw, old *v1alpha1.KrakenDGateway) admission.Warnings {
	if old != nil && old.Spec.Version == gw.Spec.Version {
		return nil
	}
	parts := strings.SplitN(strings.TrimPrefix(gw.Spec.Version, "v"), ".", 3)
	if len(parts) >= 2 && parts[0]+"."+parts[1] == configcheck.ValidatorVersion {
		return nil
	}
	version := truncate(gw.Spec.Version, versionEchoLimit)
	return admission.Warnings{fmt.Sprintf("spec.version %s: configs are validated with KrakenD %s; "+
		"the checks may not match what %s accepts", version, configcheck.ValidatorVersion, version)}
}
