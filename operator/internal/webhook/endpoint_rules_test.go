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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestValidateEntries(t *testing.T) {
	custom := testGateway()
	custom.Spec.Config.Router = &v1alpha1.RouterConfig{HealthPath: "/healthz"}
	rawRouter := testGateway()
	rawRouter.Spec.Config.ExtraConfig = &runtime.RawExtension{Raw: []byte(`{"router":{"health_path":"/live"}}`)}
	rawRouterBlock := func(block string) *v1alpha1.KrakenDGateway {
		gw := custom.DeepCopy()
		gw.Spec.Config.ExtraConfig = &runtime.RawExtension{Raw: []byte(`{"router":` + block + `}`)}
		return gw
	}
	post := testEndpoint("e", "/healthz")
	post.Spec.Endpoints[0].Method = "POST"
	ee := testGateway()
	ee.Spec.Edition = v1alpha1.EditionEE
	withPattern := func(path, pattern string) *v1alpha1.KrakenDEndpoint {
		ep := testEndpoint("e", path)
		ep.Spec.Endpoints[0].Backends[0].URLPattern = pattern
		return ep
	}
	withExtra := func(entry, backend string) *v1alpha1.KrakenDEndpoint {
		ep := testEndpoint("e", "/a")
		if entry != "" {
			ep.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(entry)}
		}
		if backend != "" {
			ep.Spec.Endpoints[0].Backends[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(backend)}
		}
		return ep
	}
	tests := []struct {
		name   string
		gw     *v1alpha1.KrakenDGateway
		ep     *v1alpha1.KrakenDEndpoint
		reject string // "" means valid
	}{
		{"plain", testGateway(), testEndpoint("e", "/a/{id}"), ""},
		{"reserved health", testGateway(), testEndpoint("e", "/__health"), "reserved by KrakenD"},
		{"reserved below debug", testGateway(), testEndpoint("e", "/x/__debug/y"), "reserved by KrakenD"},
		{"reserved echo", testGateway(), testEndpoint("e", "/__echo"), "reserved by KrakenD"},
		{"not reserved", testGateway(), testEndpoint("e", "/__other"), ""},
		{"custom health path", custom, testEndpoint("e", "/healthz"),
			"health endpoint (spec.config.router.healthPath)"},
		{"raw health path", rawRouter, testEndpoint("e", "/live"),
			"health endpoint (the router block of spec.config.extraConfig)"},
		{"case-variant raw key", rawRouterBlock(`{"Health_Path":"/live"}`), testEndpoint("e", "/live"), "health endpoint"},
		{"repeated slashes reach the health path", custom, testEndpoint("e", "//healthz"), "health endpoint"},
		{"a trailing slash is another route", custom, testEndpoint("e", "/healthz/"), ""},
		{"health disabled", rawRouterBlock(`{"disable_health":true,"health_path":"/healthz"}`),
			testEndpoint("e", "/healthz"), ""},
		// A router block that does not decode is read as the defaults, as the
		// route check reads it: health on /__health, whatever its other keys say.
		{"wrongly typed router key", rawRouterBlock(`{"health_path":"/live","auto_options":"yes"}`),
			testEndpoint("e", "/live"), ""},
		{"null router block reads as defaults", rawRouterBlock(`null`), testEndpoint("e", "/healthz"), ""},
		{"empty raw health_path reads as default", rawRouterBlock(`{"health_path":""}`),
			testEndpoint("e", "/healthz"), ""},
		{"unnamed wildcard on CE", testGateway(), testEndpoint("e", "/files/*"), "Enterprise feature"},
		{"unnamed wildcard on EE", ee, testEndpoint("e", "/files/*"), ""},
		{"placeholder from the path", testGateway(), withPattern("/a/{id}", "/u/{id}"), ""},
		{"placeholder not in the path", testGateway(), withPattern("/a/{id}", "/u/{other}"), "placeholder {other}"},
		{"mid-segment braces are no parameter", testGateway(), withPattern("/a/b{id}", "/u/{id}"), "placeholder {id}"},
		{"sequential placeholder", testGateway(), withPattern("/a", "/u/{resp0_id}"), ""},
		{"JWT placeholder", testGateway(), withPattern("/a", "/u/{JWT.sub}"), ""},
		{"EE-only entry namespace on CE", testGateway(), withExtra(`{"auth/api-keys":{"roles":["a"]}}`, ""),
			`spec.endpoints[0].extraConfig: Invalid value: "auth/api-keys"`},
		{"EE-only backend namespace on CE", testGateway(),
			withExtra("", `{"backend/http/client":{"proxy_address":"http://p"}}`),
			`spec.endpoints[0].backends[0].extraConfig: Invalid value: "backend/http/client"`},
		{"CE-honored client keys on CE", testGateway(),
			withExtra("", `{"backend/http/client":{"send_body_on_redirect":true}}`), ""},
		{"entirely EE-only backend namespace on CE", testGateway(),
			withExtra("", `{"auth/gcp":{"audience":"https://a"}}`),
			`spec.endpoints[0].backends[0].extraConfig: Invalid value: "auth/gcp": Enterprise-only extra_config namespace`},
		{"null client block on CE", testGateway(), withExtra("", `{"backend/http/client":null}`),
			`Invalid value: "backend/http/client"`},
		{"EE-only client key beside a CE-honored one", testGateway(),
			withExtra("", `{"backend/http/client":{"send_body_on_redirect":true,"proxy_address":"http://p"}}`),
			`Invalid value: "backend/http/client": Enterprise-only keys (proxy_address)`},
		{"CE namespaces on CE", testGateway(), withExtra(`{"qos/ratelimit/router":{"max_rate":1}}`,
			`{"qos/circuit-breaker":{"interval":1,"timeout":1,"max_errors":1}}`), ""},
		{"EE-only namespaces on EE", ee, withExtra(`{"auth/api-keys":{}}`, `{"backend/http/client":{}}`), ""},
		// AutoConfig generates documentation/openapi on every endpoint, and a CE
		// render drops it, so it is not refused.
		{"entry documentation on CE", testGateway(),
			withExtra(`{"documentation/openapi":{"audience":["public"]}}`, ""), ""},
		{"root wildcard on CE", testGateway(), testEndpoint("e", "/*"), "root wildcard"},
		{"root wildcard on EE", ee, testEndpoint("e", "/*"), "root wildcard"},
		{"only GET collides with the health endpoint", custom, post, ""},
		// A raw router block replaces the typed one, so the typed healthPath is gone.
		{"raw router replaces the typed one", rawRouterBlock(`{"auto_options":true}`),
			testEndpoint("e", "/healthz"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateEntries(tt.ep, []int{0}, tt.gw)
			if tt.reject == "" {
				if len(errs) != 0 {
					t.Errorf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tt.reject) {
				t.Errorf("errors = %v, want %q", errs, tt.reject)
			}
		})
	}
}

// An entry stored with an Enterprise-only namespace does not block edits to
// the endpoint's other entries.
func TestValidateEntries_EEOnlyNamespacesRatchet(t *testing.T) {
	ep := testEndpoint("e", "/stored", "/edited")
	ep.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(`{"auth/api-keys":{"roles":["a"]}}`)}
	if errs := validateEntries(ep, []int{1}, testGateway()); len(errs) != 0 {
		t.Errorf("errors = %v, want none: entry 0 is unchanged", errs)
	}
}
