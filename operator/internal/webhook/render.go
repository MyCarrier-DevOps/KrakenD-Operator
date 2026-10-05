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
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
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
// fails on the isolated baseline where its own baseline passed. A check that
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
	return admission.Warnings{fmt.Sprintf("spec.version %s: configs are validated with KrakenD %s; "+
		"the checks may not match what %s accepts", gw.Spec.Version, configcheck.ValidatorVersion, gw.Spec.Version)}
}
