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
	"encoding/json"
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/validation/field"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// reservedPathPattern matches the paths KrakenD reserves for its own
// endpoints (lura's invalidPattern, less what the CRD pattern covers).
var reservedPathPattern = regexp.MustCompile(`/__(debug|echo|health)(/.*)?$`)

// validateEntries applies the entry rules KrakenD enforces that admission can
// decide from the entry and its gateway, to the entries at positions changed.
func validateEntries(ep *v1alpha1.KrakenDEndpoint, changed []int, gw *v1alpha1.KrakenDGateway) field.ErrorList {
	var errs field.ErrorList
	health := healthPath(gw)
	for _, i := range changed {
		e := ep.Spec.Endpoints[i]
		p := field.NewPath("spec", "endpoints").Index(i)
		if reservedPathPattern.MatchString(e.Endpoint) {
			errs = append(errs, field.Invalid(p.Child("endpoint"), e.Endpoint,
				"paths under /__debug, /__echo and /__health are reserved by KrakenD"))
		}
		if e.Method == "GET" && health != "" && renderer.ConflictKey(e.Endpoint) == renderer.ConflictKey(health) {
			errs = append(errs, field.Invalid(p.Child("endpoint"), e.Endpoint,
				fmt.Sprintf("GET %s is the gateway's health endpoint (spec.config.router.healthPath)", health)))
		}
	}
	return errs
}

// healthPath returns the path the gateway serves its health endpoint on, or
// "" when it is disabled. A raw router block in spec.config.extraConfig
// replaces the typed one, as the renderer merges them.
func healthPath(gw *v1alpha1.KrakenDGateway) string {
	path := "/__health"
	if r := gw.Spec.Config.Router; r != nil && r.HealthPath != "" {
		path = r.HealthPath
	}
	if gw.Spec.Config.ExtraConfig == nil {
		return path
	}
	var raw struct {
		Router *struct {
			HealthPath    string `json:"health_path"`
			DisableHealth bool   `json:"disable_health"`
		} `json:"router"`
	}
	if json.Unmarshal(gw.Spec.Config.ExtraConfig.Raw, &raw) != nil || raw.Router == nil {
		return path
	}
	if raw.Router.DisableHealth {
		return ""
	}
	if raw.Router.HealthPath != "" {
		return raw.Router.HealthPath
	}
	return "/__health"
}
