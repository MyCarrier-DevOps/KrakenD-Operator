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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// defaultHealthPath is where KrakenD serves its health endpoint unless told otherwise.
const defaultHealthPath = "/__health"

// reservedPathPattern matches the paths KrakenD reserves for its own
// endpoints (lura's invalidPattern, less what the CRD pattern covers).
var reservedPathPattern = regexp.MustCompile(`/__(debug|echo|health)(/.*)?$`)

// urlPlaceholderPattern matches a "{name}" placeholder in a backend
// urlPattern (lura's simpleURLKeysPattern).
var urlPlaceholderPattern = regexp.MustCompile(`\{([\w\-.:/]+)\}`)

// sequentialParamPattern matches placeholders KrakenD fills from earlier
// backend responses or the JWT instead of the path (lura's
// sequentialParamsPattern).
var sequentialParamPattern = regexp.MustCompile(`^(resp\d+_.+)?(JWT\.([\w\-.:/]+))?$`)

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
				fmt.Sprintf("GET %s is the gateway's health endpoint (spec.config.router)", health)))
		}
		if gw.Spec.Edition == v1alpha1.EditionCE && strings.HasSuffix(e.Endpoint, "/*") {
			errs = append(errs, field.Invalid(p.Child("endpoint"), e.Endpoint,
				"unnamed wildcards (/*) are an Enterprise feature; the gateway runs CE"))
		}
		if gw.Spec.Edition == v1alpha1.EditionCE {
			errs = append(errs, validateEENamespaces(p, e)...)
		}
		errs = append(errs, validatePlaceholders(p, e)...)
	}
	return errs
}

// validateEENamespaces rejects the Enterprise-only namespaces in the
// extraConfig of an entry of a CE gateway and of the entry's backends.
func validateEENamespaces(p *field.Path, e v1alpha1.EndpointEntry) field.ErrorList {
	errs := ceIgnores(p.Child("extraConfig"), eeOnlyNamespacesIn(e.ExtraConfig, renderer.LevelEndpoint))
	for j, be := range e.Backends {
		errs = append(errs, ceIgnores(p.Child("backends").Index(j).Child("extraConfig"),
			eeOnlyNamespacesIn(be.ExtraConfig, renderer.LevelBackend))...)
	}
	return errs
}

// eeOnlyNamespacesIn returns the namespaces of the extra_config raw that only
// KrakenD Enterprise implements at level, in name order. raw that is not a
// JSON object has none; the render check reports it.
func eeOnlyNamespacesIn(raw *runtime.RawExtension, level renderer.NamespaceLevel) []string {
	if raw == nil || raw.Raw == nil {
		return nil
	}
	var ec map[string]json.RawMessage
	if json.Unmarshal(raw.Raw, &ec) != nil {
		return nil
	}
	var found []string
	for _, ns := range renderer.EEOnlyNamespaces(level) {
		if _, ok := ec[ns]; ok {
			found = append(found, ns)
		}
	}
	return found
}

// ceIgnores reports each Enterprise-only namespace in names on p. KrakenD CE
// accepts such a namespace in krakend check and then ignores it, so an entry
// that asks for API-key authentication would be served without any.
func ceIgnores(p *field.Path, names []string) field.ErrorList {
	var errs field.ErrorList
	for _, ns := range names {
		errs = append(errs, field.Invalid(p, ns,
			"Enterprise-only extra_config namespace: the gateway runs CE, which ignores it silently"))
	}
	return errs
}

// validatePlaceholders rejects backend urlPattern placeholders that are
// neither a parameter of the endpoint path nor filled by KrakenD itself.
func validatePlaceholders(p *field.Path, e v1alpha1.EndpointEntry) field.ErrorList {
	var errs field.ErrorList
	params := renderer.PathParams(e.Endpoint)
	for j, be := range e.Backends {
		for _, m := range urlPlaceholderPattern.FindAllStringSubmatch(be.URLPattern, -1) {
			if sequentialParamPattern.MatchString(m[1]) || slices.Contains(params, m[1]) {
				continue
			}
			errs = append(errs, field.Invalid(p.Child("backends").Index(j).Child("urlPattern"), be.URLPattern,
				fmt.Sprintf("placeholder {%s} is not a parameter of the endpoint path %s", m[1], e.Endpoint)))
		}
	}
	return errs
}

// routerOptions are the "router" extra_config keys that decide where the
// health endpoint is served; the route check reads the same ones.
type routerOptions struct {
	HealthPath    string `json:"health_path"`
	DisableHealth bool   `json:"disable_health"`
	AutoOptions   bool   `json:"auto_options"`
}

// healthPath returns the path the gateway serves its health endpoint on, or
// "" when it is disabled. It follows the route check: a raw router block in
// spec.config.extraConfig replaces the typed one, and a block that does not
// decode reads as the defaults.
func healthPath(gw *v1alpha1.KrakenDGateway) string {
	if block, ok := rawRouterBlock(gw); ok {
		var opts routerOptions
		if json.Unmarshal(block, &opts) != nil {
			opts = routerOptions{}
		}
		switch {
		case opts.DisableHealth:
			return ""
		case opts.HealthPath != "":
			return opts.HealthPath
		}
		return defaultHealthPath
	}
	if r := gw.Spec.Config.Router; r != nil && r.HealthPath != "" {
		return r.HealthPath
	}
	return defaultHealthPath
}

// rawRouterBlock returns the "router" entry of the gateway's raw extraConfig.
func rawRouterBlock(gw *v1alpha1.KrakenDGateway) (json.RawMessage, bool) {
	raw := gw.Spec.Config.ExtraConfig
	if raw == nil {
		return nil, false
	}
	var ec map[string]json.RawMessage
	if json.Unmarshal(raw.Raw, &ec) != nil {
		return nil, false
	}
	block, ok := ec["router"]
	return block, ok
}
