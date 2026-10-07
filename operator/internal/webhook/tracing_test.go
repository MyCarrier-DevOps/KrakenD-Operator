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
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// tracedValidators returns the validators over objs, with the real checker,
// whose spans and krakend runs go to rec.
func tracedValidators(rec *tracingtest.Recorder, objs ...client.Object) Validators {
	c := fakeClient(objs...)
	validator := renderer.NewValidator(renderer.ValidatorOptions{
		Executor: telemetry.TraceExecutor(acceptingExecutor{}, rec.Tracer()), BinaryPath: "krakend",
	})
	checker := configcheck.New(c, renderer.New(renderer.Options{}), validator, 1, rec.Tracer())
	return NewValidators(c, c, checker, "", rec.Tracer())
}

// An endpoint's admission is one span: its structural rules below it, then
// the config check and its krakend run.
func TestEndpointAdmission_IsOneSpanAboveItsRulesAndKrakendRun(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidators(rec, testGateway())
	admit := tracedValidator{kind: kindEndpoint, next: v.Endpoint, tracer: rec.Tracer()}

	if _, err := admit.ValidateCreate(context.Background(), testEndpoint("e", "/new")); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "admission.validate KrakenDEndpoint", "admission.structural")
	spans.RequireAncestors(t, "krakend check", "configcheck.", "admission.validate KrakenDEndpoint")
	spans.RequireParent(t, "admission.validate KrakenDEndpoint", "configcheck.Conflicts")
	spans.RequireParent(t, "admission.validate KrakenDEndpoint", "configcheck.CheckRoot")
	spans.RequireParent(t, "admission.validate KrakenDEndpoint", "configcheck.CheckEndpoint")
}

func TestGatewayAdmission_IsOneSpanAboveItsRulesAndKrakendRun(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidators(rec)
	admit := tracedValidator{kind: "KrakenDGateway", next: v.Gateway, tracer: rec.Tracer()}

	if _, err := admit.ValidateCreate(context.Background(), testGateway()); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "admission.validate KrakenDGateway", "admission.structural")
	spans.RequireAncestors(t, "krakend check", "configcheck.", "admission.validate KrakenDGateway")
}
