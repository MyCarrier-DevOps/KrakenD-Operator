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

package main

import (
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	licenseutil "github.com/mycarrier-devops/krakend-operator/internal/util/license"
	webhooksetup "github.com/mycarrier-devops/krakend-operator/internal/webhook"
)

// validation is everything that holds the pod's one config checker.
type validation struct {
	Checker    *configcheck.Checker
	Gateway    *controller.KrakenDGatewayReconciler
	Validators webhooksetup.Validators
}

// wireValidation builds the pod's one config checker and the parts that use
// it. The checker's slots bound concurrent krakend executions across the
// gateway controller and the admission webhooks, so they must share it.
func wireValidation(mgr ctrl.Manager, r renderer.Renderer, v renderer.Validator) validation {
	checker := configcheck.New(mgr.GetClient(), r, v, configCheckSlots)
	return validation{
		Checker: checker,
		Gateway: &controller.KrakenDGatewayReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			Recorder:      mgr.GetEventRecorderFor("krakendgateway-controller"),
			Renderer:      r,
			Checker:       checker,
			Clock:         clock.RealClock{},
			APIReader:     mgr.GetAPIReader(),
			LicenseParser: licenseutil.NewX509LicenseParser(),
		},
		Validators: webhooksetup.NewValidators(mgr.GetClient(), checker),
	}
}
