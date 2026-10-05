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

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

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
	after, err := chk.CheckGateway(ctx, gw, nil)
	if err != nil || after.OK {
		return nil, checkErr(err)
	}
	before, err := chk.CheckGateway(ctx, old, nil)
	if err != nil {
		return nil, checkErr(err)
	}
	if before.OK {
		return nil, gatewayRenderDenial(gw, after)
	}
	preexisting := admission.Warnings{"the gateway's config already fails validation: " +
		before.Summary(warningLimit)}
	rootAfter, err := chk.CheckIsolated(ctx, gw, nil)
	if err != nil || rootAfter.OK {
		return preexisting, checkErr(err)
	}
	rootBefore, err := chk.CheckIsolated(ctx, old, nil)
	if err != nil {
		return nil, checkErr(err)
	}
	if rootBefore.OK {
		return nil, gatewayRenderDenial(gw, rootAfter)
	}
	return preexisting, nil
}

// gatewayRenderDenial rejects gw: gateway-root findings on spec.config, the
// endpoints the change breaks on spec. The renderer builds the root from
// spec.config (timeout, extraConfig, router), so that is where a user looks.
func gatewayRenderDenial(gw *v1alpha1.KrakenDGateway, verdict configcheck.Verdict) error {
	var errs field.ErrorList
	var endpoints configcheck.Verdict
	for _, f := range verdict.Findings {
		if f.Endpoint.Name == "" {
			errs = append(errs, field.Invalid(field.NewPath("spec", "config"), field.OmitValueType{}, f.Message))
			continue
		}
		endpoints.Findings = append(endpoints.Findings, f)
	}
	if len(endpoints.Findings) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec"), field.OmitValueType{},
			"with this change these endpoints fail krakend check: "+endpoints.Summary(warningLimit)))
	}
	return invalid("KrakenDGateway", gw.Name, errs)
}

// versionWarning warns, when spec.version is set or changed, that gw runs a
// KrakenD minor version other than the one admission and the controller
// validate with.
func versionWarning(_, _ *v1alpha1.KrakenDGateway) admission.Warnings {
	return nil
}
