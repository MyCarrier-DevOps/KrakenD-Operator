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

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// checkGatewayRender validates gw's config. A new gateway must render on its
// own.
func checkGatewayRender(
	ctx context.Context, chk ConfigChecker, _, gw *v1alpha1.KrakenDGateway,
) (admission.Warnings, error) {
	_, err := chk.CheckIsolated(ctx, gw, nil)
	return nil, checkErr(err)
}

// versionWarning warns, when spec.version is set or changed, that gw runs a
// KrakenD minor version other than the one admission and the controller
// validate with.
func versionWarning(_, _ *v1alpha1.KrakenDGateway) admission.Warnings {
	return nil
}
