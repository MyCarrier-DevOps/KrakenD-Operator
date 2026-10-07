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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
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

func TestAutoConfigAdmission_RulesAreAStructuralSpan(t *testing.T) {
	rec := tracingtest.New(t)
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := tracedValidators(rec, gw)
	admit := tracedValidator{kind: "KrakenDAutoConfig", next: v.AutoConfig, tracer: rec.Tracer()}

	if _, err := admit.ValidateCreate(context.Background(), newAutoConfigForAdditional(nil)); err != nil {
		t.Fatal(err)
	}

	rec.Ended().RequireChild(t, "admission.validate KrakenDAutoConfig", "admission.structural")
}

// A policy's admission is one span, with the policy's own check and its
// krakend run below it.
func TestPolicyAdmission_IsOneSpanAboveItsCheckAndKrakendRun(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidators(rec)
	admit := tracedValidator{kind: "KrakenDBackendPolicy", next: v.Policy, tracer: rec.Tracer()}
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}

	if _, err := admit.ValidateCreate(context.Background(), policy); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireAncestors(t, "krakend check", "configcheck.CheckPolicy", "admission.validate KrakenDBackendPolicy")
	spans.RequireParent(t, "admission.validate KrakenDBackendPolicy", "configcheck.CheckPolicy")
}

// webhookManager is the part of a manager SetupWebhooks uses: the scheme, the
// webhook server, the REST config the webhook builder reads and a field
// indexer that accepts every index. Every other method panics.
type webhookManager struct {
	ctrl.Manager
	server webhook.Server
}

func (m webhookManager) GetScheme() *runtime.Scheme { return testScheme() }

func (m webhookManager) GetWebhookServer() webhook.Server { return m.server }

func (m webhookManager) GetConfig() *rest.Config { return &rest.Config{} }

func (m webhookManager) GetFieldIndexer() client.FieldIndexer { return acceptingIndexer{} }

// acceptingIndexer accepts every field index and keeps none.
type acceptingIndexer struct{}

func (acceptingIndexer) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	return nil
}

// createReview is the AdmissionReview the API server sends to create obj.
func createReview(t *testing.T, obj client.Object) []byte {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID: "uid", Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Each admission request the webhook server receives reaches its validator
// through the path SetupWebhooks registers: the validator's span is a child of
// the request's server span, for every kind.
func TestSetupWebhooks_EachValidatorSpanIsAChildOfItsRequestsServerSpan(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	for _, tc := range []struct {
		kind string
		obj  client.Object
	}{
		{"KrakenDGateway", testGateway()},
		{"KrakenDEndpoint", testEndpoint("e", "/e")},
		{"KrakenDBackendPolicy", policy},
		{"KrakenDAutoConfig", newAutoConfigForAdditional(nil)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			rec := tracingtest.New(t)
			server := telemetry.TraceWebhookServer(webhook.NewServer(webhook.Options{}), rec.Provider())
			if err := SetupWebhooks(webhookManager{server: server}, tracedValidators(rec, testGateway())); err != nil {
				t.Fatal(err)
			}
			path := "/validate-gateway-krakend-io-v1alpha1-" + strings.ToLower(tc.kind)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(createReview(t, tc.obj)))
			req.Header.Set("Content-Type", "application/json")

			server.WebhookMux().ServeHTTP(httptest.NewRecorder(), req)

			rec.Ended().RequireChild(t, "admission "+path, "admission.validate "+tc.kind)
		})
	}
}

// A gateway update compares its config with the stored one's, then judges the
// endpoints the gateway serves in a span of its own, below the admission's.
func TestGatewayAdmission_JudgesItsServedEndpointsInASpan(t *testing.T) {
	rec := tracingtest.New(t)
	old, gw := editedGateway()
	v := tracedValidators(rec, old, testEndpoint("e", "/e"))
	admit := tracedValidator{kind: "KrakenDGateway", next: v.Gateway, tracer: rec.Tracer()}

	if _, err := admit.ValidateUpdate(context.Background(), old, gw); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "admission.validate KrakenDGateway", "admission.judge_served")
	spans.RequireChild(t, "admission.validate KrakenDGateway", "configcheck.SameConfig")
	spans.RequireParent(t, "admission.validate KrakenDGateway", "configcheck.Conflicts")
	spans.RequireParent(t, "admission.validate KrakenDGateway", "configcheck.CheckRoot")
	spans.RequireChild(t, "admission.validate KrakenDGateway", "configcheck.CheckGroup")
}

// A policy write screens each gateway that uses it in a span of its own,
// below the admission's.
func TestPolicyAdmission_ScreensEachGatewayInASpan(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidators(rec, referencing()...)
	admit := tracedValidator{kind: "KrakenDBackendPolicy", next: v.Policy, tracer: rec.Tracer()}

	if _, err := admit.ValidateCreate(context.Background(), testPolicy(`{}`)); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "admission.validate KrakenDBackendPolicy", "admission.screen_policy")
	spans.RequireParent(t, "admission.screen_policy", "configcheck.CheckRoot")
	spans.RequireParent(t, "admission.screen_policy", "configcheck.CheckGroup")
}

// Once every gateway is screened, a policy write judges each in a span of its
// own, below the admission's.
func TestPolicyAdmission_JudgesEachGatewayInASpan(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidators(rec, referencing()...)
	admit := tracedValidator{kind: "KrakenDBackendPolicy", next: v.Policy, tracer: rec.Tracer()}

	if _, err := admit.ValidateCreate(context.Background(), testPolicy(`{}`)); err != nil {
		t.Fatal(err)
	}

	rec.Ended().RequireChild(t, "admission.validate KrakenDBackendPolicy", "admission.judge_policy")
}

// outcomeValidator stands in for a validator whose decision is err.
type outcomeValidator struct{ err error }

func (v outcomeValidator) ValidateCreate(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, v.err
}

func (v outcomeValidator) ValidateUpdate(context.Context, runtime.Object, runtime.Object) (admission.Warnings, error) {
	return nil, v.err
}

func (v outcomeValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, v.err
}

// A denial is the validator's answer, not a failure: its span is not an error,
// and the denial, which can quote the tenant's object, is on no span.
func TestAdmission_ADenialIsNoErrorAndItsTextIsOnNoSpan(t *testing.T) {
	rec := tracingtest.New(t)
	denial := invalid(kindEndpoint, "e",
		field.ErrorList{field.Invalid(field.NewPath("spec"), "TENANT-VALUE", "TENANT-DETAIL")})
	admit := tracedValidator{kind: kindEndpoint, next: outcomeValidator{err: denial}, tracer: rec.Tracer()}

	_, err := admit.ValidateCreate(context.Background(), testEndpoint("e", "/e"))

	if err == nil {
		t.Fatal("the denial was swallowed")
	}
	span := rec.Ended().One(t, "admission.validate KrakenDEndpoint")
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("status = %v, want unset: a denial is not an error", got)
	}
	if n := len(span.Events()); n != 0 {
		t.Errorf("%d events on the span, want none: %v", n, span.Events())
	}
}

// A decision that could not be reached is an error on the span, with no text
// of what failed: the span that failed records it.
func TestAdmission_AFailureToDecideIsAnErrorWithoutItsText(t *testing.T) {
	rec := tracingtest.New(t)
	failure := unavailable(errors.New("lookup failed: TENANT-DETAIL"))
	admit := tracedValidator{kind: kindEndpoint, next: outcomeValidator{err: failure}, tracer: rec.Tracer()}

	if _, err := admit.ValidateCreate(context.Background(), testEndpoint("e", "/e")); err == nil {
		t.Fatal("the failure was swallowed")
	}

	span := rec.Ended().One(t, "admission.validate KrakenDEndpoint")
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("status = %v, want error", got)
	}
	if got := span.Status().Description; strings.Contains(got, "TENANT") {
		t.Errorf("status description = %q, want none of the failure's text", got)
	}
	if n := len(span.Events()); n != 0 {
		t.Errorf("%d events on the span, want none: %v", n, span.Events())
	}
}
