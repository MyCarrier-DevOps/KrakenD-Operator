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
	tests := []struct {
		name   string
		gw     *v1alpha1.KrakenDGateway
		ep     *v1alpha1.KrakenDEndpoint
		reject string // "" means valid
	}{
		{"plain", testGateway(), testEndpoint("e", "/a/{id}"), ""},
		{"reserved health", testGateway(), testEndpoint("e", "/__health"), "reserved by KrakenD"},
		{"reserved below debug", testGateway(), testEndpoint("e", "/x/__debug/y"), "reserved by KrakenD"},
		{"not reserved", testGateway(), testEndpoint("e", "/__other"), ""},
		{"custom health path", custom, testEndpoint("e", "/healthz"), "health endpoint"},
		{"raw health path", rawRouter, testEndpoint("e", "/live"), "health endpoint"},
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
		{"placeholder not in the path", testGateway(), withPattern("/a/{id}", "/u/{other}"), "placeholder {other}"},
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
