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
	"net/http"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// acceptingExecutor stands in for a krakend binary that accepts every config,
// so the real validator runs its own route check and nothing else.
type acceptingExecutor struct{}

func (acceptingExecutor) Execute(context.Context, string, ...string) ([]byte, error) {
	return []byte("Syntax OK!"), nil
}

// realChecker is the checker the operator runs, over the endpoints in objs.
func realChecker(objs ...client.Object) (client.Client, *configcheck.Checker) {
	c := fakeClient(objs...)
	validator := renderer.NewValidator(renderer.ValidatorOptions{Executor: acceptingExecutor{}, BinaryPath: "krakend"})
	return c, configcheck.New(c, renderer.New(renderer.Options{}), validator, 1)
}

func methodEndpoint(name, method, path string) *v1alpha1.KrakenDEndpoint {
	ep := testEndpoint(name, path)
	ep.Spec.Endpoints[0].Method = method
	return ep
}

func routerOf(gw *v1alpha1.KrakenDGateway, router v1alpha1.RouterConfig) *v1alpha1.KrakenDGateway {
	gw.Spec.Config.Router = &router
	return gw
}

// The route check's real refusals, through the real checker, decide the
// gateway rule: an edit that only un-masks an older clash is admitted, and one
// that makes healthy endpoints clash is not.
func TestGatewayAdmission_FailingGatewayRatchetOnRealRouteRefusals(t *testing.T) {
	tests := []struct {
		name     string
		objs     []client.Object
		old, gw  *v1alpha1.KrakenDGateway
		denied   bool
		warnWith string
	}{
		{
			// Moving health_path off GET /s/{q}: the stored z already loses
			// GET /s/{p}/x to the older x at render time, so nothing fails.
			name: "moving health_path un-masks an older clash",
			objs: []client.Object{methodEndpoint("x", "GET", "/s/{q}"), methodEndpoint("z", "GET", "/s/{p}/x")},
			old:  routerOf(testGateway(), v1alpha1.RouterConfig{HealthPath: "/s/:p"}),
			gw:   routerOf(testGateway(), v1alpha1.RouterConfig{HealthPath: "/healthz"}),
		},
		{
			// auto_options joins every method's paths in one tree, so GET /a/{id}
			// and POST /a/{name} clash once it is on: a new clash the change makes.
			name: "turning auto_options on makes healthy endpoints clash",
			objs: []client.Object{
				methodEndpoint("b", "GET", "/a/{id}"), methodEndpoint("c", "POST", "/a/{name}"),
				methodEndpoint("x1", "GET", "/x/{id}"), methodEndpoint("x2", "GET", "/x/{id}.json"),
			},
			old:    routerOf(testGateway(), v1alpha1.RouterConfig{}),
			gw:     routerOf(testGateway(), v1alpha1.RouterConfig{AutoOptions: true}),
			denied: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, chk := realChecker(tt.objs...)

			resp := review(t, &GatewayValidator{Client: c, Checker: chk}, "alice", tt.gw, tt.old)

			if tt.denied {
				if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
					t.Errorf("response = %+v, want a 422 denial", resp.Result)
				}
				return
			}
			if !resp.Allowed {
				t.Fatalf("denied: %+v", resp.Result)
			}
			if tt.warnWith == "" {
				if len(resp.Warnings) != 0 {
					t.Errorf("warnings = %v, want none", resp.Warnings)
				}
				return
			}
			if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], tt.warnWith) {
				t.Errorf("warnings = %v, want one containing %q", resp.Warnings, tt.warnWith)
			}
		})
	}
}
