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
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	admissionv1 "k8s.io/api/admission/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	return tracedValidatorsRunning(rec, acceptingExecutor{}, objs...)
}

// tracedValidatorsRunning is tracedValidators over a krakend binary that is
// executor.
func tracedValidatorsRunning(rec *tracingtest.Recorder, executor renderer.CommandExecutor, objs ...client.Object) Validators {
	c := fakeClient(objs...)
	validator := renderer.NewValidator(renderer.ValidatorOptions{
		Executor: telemetry.TraceExecutor(executor, rec.Tracer()), BinaryPath: "krakend",
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

// krakendOutput is what a rejecting krakend prints: tenant-controlled text that
// no span may carry.
const krakendOutput = "SECRET-KRAKEND-OUTPUT"

// rejectingExecutor stands in for a krakend binary that rejects the configs for
// which reject is true, printing krakendOutput; when broken is set, it fails
// to run on those configs instead.
type rejectingExecutor struct {
	reject func(config string) bool
	broken error
}

func (e rejectingExecutor) Execute(_ context.Context, _ string, args ...string) ([]byte, error) {
	config, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		return nil, err
	}
	if e.reject(string(config)) {
		if e.broken != nil {
			return []byte(krakendOutput), e.broken
		}
		return []byte(krakendOutput), exec.Command("sh", "-c", "exit 3").Run()
	}
	return []byte("Syntax OK!"), nil
}

// rejectsWith rejects the configs that contain every one of parts.
func rejectsWith(parts ...string) func(string) bool {
	return func(config string) bool {
		for _, part := range parts {
			if !strings.Contains(config, part) {
				return false
			}
		}
		return true
	}
}

// requireCleanAdmissionSpans fails t if a span of the admission, whose
// name starts with "admission.", carries an error status or an event, or holds
// the krakend output in an attribute.
func requireCleanAdmissionSpans(t *testing.T, spans tracingtest.Spans) {
	t.Helper()
	for _, span := range spans {
		if !strings.HasPrefix(span.Name(), "admission.") {
			continue
		}
		if got := span.Status().Code; got == codes.Error {
			t.Errorf("span %q is an error (%q), want none: a denial is an answer", span.Name(), span.Status().Description)
		}
		if len(span.Events()) != 0 {
			t.Errorf("span %q has events %v, want none", span.Name(), span.Events())
		}
		for _, attr := range span.Attributes() {
			if strings.Contains(attr.Value.Emit(), "SECRET") {
				t.Errorf("span %q has attribute %v, want none of the krakend output", span.Name(), attr)
			}
		}
	}
}

// A gateway update that breaks an endpoint is denied; the denial names the
// endpoint and is on no span of the admission.
func TestGatewayAdmission_ADeniedUpdateIsNoErrorOnItsSpans(t *testing.T) {
	rec := tracingtest.New(t)
	old, gw := editedGateway()
	rejecting := rejectingExecutor{reject: rejectsWith("/bad", "1s")}
	v := tracedValidatorsRunning(rec, rejecting, old, testEndpoint("e", "/bad"))
	admit := tracedValidator{kind: "KrakenDGateway", next: v.Gateway, tracer: rec.Tracer()}

	_, err := admit.ValidateUpdate(context.Background(), old, gw)

	if !isDenial(err) {
		t.Fatalf("err = %v, want a denial", err)
	}
	spans := rec.Ended()
	spans.RequireParent(t, "admission.judge_served", "configcheck.CheckEndpoint")
	requireCleanAdmissionSpans(t, spans)
}

// An AutoConfig that names no gateway is denied; the denial is on no span.
func TestAutoConfigAdmission_ADenialIsNoErrorOnItsStructuralSpan(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidators(rec)
	admit := tracedValidator{kind: "KrakenDAutoConfig", next: v.AutoConfig, tracer: rec.Tracer()}

	_, err := admit.ValidateCreate(context.Background(), newAutoConfigForAdditional(nil))

	if !isDenial(err) {
		t.Fatalf("err = %v, want a denial", err)
	}
	spans := rec.Ended()
	spans.RequireChild(t, "admission.validate KrakenDAutoConfig", "admission.structural")
	requireCleanAdmissionSpans(t, spans)
}

// A post-restart Job the requester may not create is denied as forbidden; the
// denial, which names the requester, is on no span.
func TestGatewayAdmission_AForbiddenRequestIsNoErrorOnItsStructuralSpan(t *testing.T) {
	rec := tracingtest.New(t)
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}, Tracer: rec.Tracer()}
	admit := tracedValidator{kind: "KrakenDGateway", next: v, tracer: rec.Tracer()}
	gw := gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) { p.ServiceAccountName = "namespace-admin" })

	resp := review(t, admit, "alice", gw, nil)

	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("response = %+v, want a 403 denial", resp.Result)
	}
	spans := rec.Ended()
	spans.RequireChild(t, "admission.validate KrakenDGateway", "admission.structural")
	requireCleanAdmissionSpans(t, spans)
}

// A policy write whose gateways cannot be checked is a 500; the failure marks
// the spans of the decision, and the text of what failed is on none of them.
func TestPolicyAdmission_AFailedCheckMarksItsSpansWithoutTheText(t *testing.T) {
	rec := tracingtest.New(t)
	broken := rejectingExecutor{reject: rejectsWith(`"/a"`), broken: errors.New("SECRET-NO-BINARY")}
	v := tracedValidatorsRunning(rec, broken, referencing()...)
	admit := tracedValidator{kind: "KrakenDBackendPolicy", next: v.Policy, tracer: rec.Tracer()}

	_, err := admit.ValidateCreate(context.Background(), testPolicy(`{}`))

	if !apierrors.IsInternalError(err) {
		t.Fatalf("err = %v, want a 500", err)
	}
	spans := rec.Ended()
	for _, name := range []string{
		"admission.validate KrakenDBackendPolicy", "admission.screen_policy", "admission.judge_policy",
	} {
		span := spans.One(t, name)
		if got := span.Status(); got.Code != codes.Error || got.Description != decisionFailed {
			t.Errorf("span %q status = %+v, want an error described %q", name, got, decisionFailed)
		}
		if len(span.Events()) != 0 {
			t.Errorf("span %q has events %v, want none", name, span.Events())
		}
	}
}

// parentsOf returns the names of the parents of the spans named name, sorted.
func parentsOf(spans tracingtest.Spans, name string) []string {
	var parents []string
	for _, span := range spans.Named(name) {
		parent := "<root>"
		if p := spans.Parent(span); p != nil {
			parent = p.Name()
		}
		parents = append(parents, parent)
	}
	slices.Sort(parents)
	return parents
}

func requireParents(t *testing.T, spans tracingtest.Spans, name string, want ...string) {
	t.Helper()
	if got := parentsOf(spans, name); !slices.Equal(got, want) {
		t.Errorf("spans %q have parents %v, want %v; spans: %s", name, got, want, spans)
	}
}

// A gateway created over endpoints that fail together is admitted with a
// warning; the warning's checks are below the admission's own span.
func TestGatewayAdmission_ACreateWarningsChecksAreBelowTheAdmission(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidatorsRunning(rec, rejectingExecutor{reject: rejectsWith("/bad")}, testEndpoint("e", "/bad"))
	admit := tracedValidator{kind: "KrakenDGateway", next: v.Gateway, tracer: rec.Tracer()}

	warnings, err := admit.ValidateCreate(context.Background(), testGateway())

	if err != nil || len(warnings) == 0 {
		t.Fatalf("warnings, err = %v, %v; want a warning", warnings, err)
	}
	spans := rec.Ended()
	spans.RequireParent(t, "admission.validate KrakenDGateway", "configcheck.CheckGroup")
	spans.RequireParent(t, "admission.validate KrakenDGateway", "configcheck.CheckEndpoint")
	requireCleanAdmissionSpans(t, spans)
}

// An update of a gateway whose root fails on its own, and fails as before, is
// admitted with a warning; the stored root's check is below the admission's span.
func TestGatewayAdmission_AFailingRootChecksTheStoredRootBelowTheAdmission(t *testing.T) {
	rec := tracingtest.New(t)
	old, gw := editedGateway()
	v := tracedValidatorsRunning(rec, rejectingExecutor{reject: func(string) bool { return true }}, old)
	admit := tracedValidator{kind: "KrakenDGateway", next: v.Gateway, tracer: rec.Tracer()}

	warnings, err := admit.ValidateUpdate(context.Background(), old, gw)

	if err != nil || len(warnings) == 0 {
		t.Fatalf("warnings, err = %v, %v; want a warning", warnings, err)
	}
	spans := rec.Ended()
	requireParents(t, spans, "configcheck.CheckRoot", "admission.validate KrakenDGateway", "admission.validate KrakenDGateway")
	requireCleanAdmissionSpans(t, spans)
}

// An update over a stored root that fails on its own judges the endpoints
// under the failing root; every check of the judging is below its span, and
// the stored root's is too.
func TestGatewayAdmission_JudgingUnderAFailingStoredRootIsBelowItsSpan(t *testing.T) {
	served := testEndpoint("e", "/bad")
	served.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionTrue}}
	for _, tc := range []struct {
		name string
		ep   *v1alpha1.KrakenDEndpoint
	}{
		{"an endpoint the last config left out", testEndpoint("e", "/bad")},
		{"an endpoint the last config serves", served},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tracingtest.New(t)
			old, gw := editedGateway()
			// The stored config lacks the edit's timeout, so it fails on its own.
			rejecting := rejectingExecutor{reject: func(config string) bool {
				return !strings.Contains(config, "1s") || strings.Contains(config, "/bad")
			}}
			v := tracedValidatorsRunning(rec, rejecting, old, tc.ep)
			admit := tracedValidator{kind: "KrakenDGateway", next: v.Gateway, tracer: rec.Tracer()}

			warnings, err := admit.ValidateUpdate(context.Background(), old, gw)

			if err == nil && len(warnings) == 0 {
				t.Fatal("admitted without a word, want a denial or a warning of the endpoints")
			}
			spans := rec.Ended()
			requireParents(t, spans, "configcheck.CheckRoot",
				"admission.judge_served", "admission.validate KrakenDGateway")
			spans.RequireParent(t, "admission.judge_served", "configcheck.CheckEndpoint")
			requireCleanAdmissionSpans(t, spans)
		})
	}
}

// An endpoint update that fails, over a stored version that failed too, is
// admitted with a warning; both checks are below the admission's span.
func TestEndpointAdmission_ItsStoredVersionsCheckIsBelowTheAdmission(t *testing.T) {
	rec := tracingtest.New(t)
	old := testEndpoint("e", "/bad")
	v := tracedValidatorsRunning(rec, rejectingExecutor{reject: rejectsWith("/bad")}, testGateway(), old)
	admit := tracedValidator{kind: kindEndpoint, next: v.Endpoint, tracer: rec.Tracer()}

	warnings, err := admit.ValidateUpdate(context.Background(), old, testEndpoint("e", "/bad/x"))

	if err != nil || len(warnings) == 0 {
		t.Fatalf("warnings, err = %v, %v; want a warning", warnings, err)
	}
	spans := rec.Ended()
	spans.RequireParent(t, "admission.validate KrakenDEndpoint", "configcheck.CheckEndpoint")
	requireCleanAdmissionSpans(t, spans)
}

// A policy write that breaks an endpoint is denied; the judging of the
// gateway's endpoints is below its span.
func TestPolicyAdmission_JudgingAGatewaysEndpointsIsBelowItsSpan(t *testing.T) {
	rec := tracingtest.New(t)
	v := tracedValidatorsRunning(rec, rejectingExecutor{reject: rejectsWith("/a")}, referencing()...)
	admit := tracedValidator{kind: "KrakenDBackendPolicy", next: v.Policy, tracer: rec.Tracer()}

	warnings, err := admit.ValidateCreate(context.Background(), testPolicy(`{}`))

	if err != nil || len(warnings) == 0 {
		t.Fatalf("warnings, err = %v, %v; want a warning", warnings, err)
	}
	spans := rec.Ended()
	spans.RequireParent(t, "admission.judge_policy", "configcheck.CheckEndpoint")
	requireCleanAdmissionSpans(t, spans)
}
