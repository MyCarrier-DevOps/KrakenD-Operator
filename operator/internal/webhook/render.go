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
// own.
func checkGatewayRender(
	ctx context.Context, chk ConfigChecker, old, gw *v1alpha1.KrakenDGateway,
) (admission.Warnings, error) {
	if old != nil {
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
		return nil, nil
	}
	root, err := chk.CheckIsolated(ctx, gw, nil)
	if err != nil || root.OK {
		return nil, checkErr(err)
	}
	return nil, gatewayRenderDenial(gw, root)
}

// gatewayRenderDenial rejects gw: gateway-root findings on spec.config. The
// renderer builds the root from spec.config (timeout, extraConfig, router), so
// that is where a user looks.
func gatewayRenderDenial(gw *v1alpha1.KrakenDGateway, verdict configcheck.Verdict) error {
	var errs field.ErrorList
	for _, f := range verdict.Findings {
		errs = append(errs, field.Invalid(field.NewPath("spec", "config"), field.OmitValueType{}, f.Message))
	}
	return invalid("KrakenDGateway", gw.Name, errs)
}

// versionWarning warns, when spec.version is set or changed, that gw runs a
// KrakenD minor version other than the one admission and the controller
// validate with.
func versionWarning(_, _ *v1alpha1.KrakenDGateway) admission.Warnings {
	return nil
}
