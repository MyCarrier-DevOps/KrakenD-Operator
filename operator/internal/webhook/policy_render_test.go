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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

func testPolicy(raw string) *v1alpha1.KrakenDBackendPolicy {
	return &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{Raw: &runtime.RawExtension{Raw: []byte(raw)}},
	}
}

// referencing returns objects for a gateway and an endpoint that references policy p.
func referencing() []client.Object {
	ep := testEndpoint("uses-p", "/a")
	ep.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
	return []client.Object{testGateway(), ep}
}

func TestPolicyAdmission_Render(t *testing.T) {
	bad := configcheck.Verdict{
		Output: "- at '/endpoints/0/backend/0/extra_config': additional properties 'qos/circuit-breakr' not allowed"}
	ok, fail := configcheck.Verdict{OK: true}, configcheck.Verdict{Output: "x"}
	epOK := configcheck.EndpointVerdict{OK: true}
	epFail := configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: "x"}
	tests := []struct {
		name             string
		objs             []client.Object
		old              *v1alpha1.KrakenDBackendPolicy
		verdicts         []configcheck.Verdict
		endpointVerdicts []configcheck.EndpointVerdict
		allowed          bool
		calls            string
		warning          string // a substring of the one warning expected; "" for none
	}{
		{"unreferenced, lints clean", nil, nil, nil, nil, true, "policy", ""},
		{"unreferenced, fails alone", nil, nil, []configcheck.Verdict{bad}, nil, false, "policy", ""},
		{"referenced, keeps its endpoints passing", referencing(), testPolicy(`{}`), nil, nil, true,
			"policy,root,group", ""},
		{"referenced, breaks its endpoint", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{ok, ok, fail}, []configcheck.EndpointVerdict{epFail, epOK}, false,
			"policy,root,group,endpoint,endpoint", ""},
		{"created, referenced, breaks its endpoint", referencing(), nil,
			[]configcheck.Verdict{ok, ok, fail}, []configcheck.EndpointVerdict{epFail}, false,
			"policy,root,group,endpoint", ""},
		{"failing alone before and after: its endpoints decide", referencing(), testPolicy(`{"x":{}}`),
			[]configcheck.Verdict{bad, bad}, nil, true, "policy,policy,root,group", ""},
		{"its endpoints already fail with the stored policy", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{ok, ok, fail}, []configcheck.EndpointVerdict{epFail, epFail}, true,
			"policy,root,group,endpoint,endpoint", "already fail validation with the stored policy"},
		{"they already fail together with the stored policy", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{ok, ok, fail, fail}, nil, true, "policy,root,group,endpoint,group",
			"already fail validation together"},
		{"they fail only together with the change", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{ok, ok, fail, ok}, nil, false, "policy,root,group,endpoint,group", ""},
		{"the gateway root fails alone", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{ok, fail}, nil, true, "policy,root", "fails validation on its own"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chk := &scriptedChecker{verdicts: tt.verdicts, endpointVerdicts: tt.endpointVerdicts}
			v := &PolicyValidator{Client: fakeClient(tt.objs...), Checker: chk}
			var old runtime.Object
			if tt.old != nil {
				old = tt.old
			}
			resp := review(t, v, "alice", testPolicy(`{"qos/circuit-breaker":{}}`), old)
			if resp.Allowed != tt.allowed {
				t.Errorf("allowed = %v, want %v (%+v)", resp.Allowed, tt.allowed, resp.Result)
			}
			if !tt.allowed && resp.Result.Code != http.StatusUnprocessableEntity {
				t.Errorf("code = %d, want 422", resp.Result.Code)
			}
			if got := strings.Join(chk.calls, ","); got != tt.calls {
				t.Errorf("checks = %s, want %s", got, tt.calls)
			}
			if tt.warning == "" && len(resp.Warnings) != 0 {
				t.Errorf("warnings = %q, want none", resp.Warnings)
			}
			if tt.warning != "" && (len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], tt.warning)) {
				t.Errorf("warnings = %q, want one containing %q", resp.Warnings, tt.warning)
			}
		})
	}
}

// An update that leaves the spec alone, such as the protection finalizer, is
// never rendered: a stored policy that fails must not block it.
func TestPolicyAdmission_MetadataOnlyUpdateIsNotValidated(t *testing.T) {
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{Output: "bad"}}}
	v := &PolicyValidator{Client: fakeClient(referencing()...), Checker: chk}
	old := testPolicy(`{"qos/circuit-breakr":{}}`)
	policy := old.DeepCopy()
	policy.Finalizers = []string{"gateway.krakend.io/policy-protection"}

	resp := review(t, v, "alice", policy, old)

	if !resp.Allowed {
		t.Errorf("response = %+v, want a metadata-only update admitted", resp.Result)
	}
	if len(chk.calls) != 0 {
		t.Errorf("checks = %v, want none", chk.calls)
	}
}

// A gateway still renders a terminating policy, so a spec change on one is
// judged like any other: only a metadata change is skipped.
func TestPolicyAdmission_TerminatingPolicyWithBrokenSpecChangeIsRefused(t *testing.T) {
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{Output: "bad"}, {OK: true}}}
	v := &PolicyValidator{Client: fakeClient(), Checker: chk}
	old := terminating(testPolicy(`{}`))

	resp := review(t, v, "alice", terminating(testPolicy(`{"qos/circuit-breakr":{}}`)), old)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Errorf("response = %+v, want 422", resp.Result)
	}
	if got := strings.Join(chk.calls, ","); got != "policy,policy" {
		t.Errorf("checks = %s, want policy,policy", got)
	}
	if got := strings.Join(chk.args, " "); got != `policy:{"qos/circuit-breakr":{}} policy:{}` {
		t.Errorf("checks received %s, want the new policy then the stored one", got)
	}
}

func TestPolicyAdmission_DenialIsBounded(t *testing.T) {
	huge := configcheck.Verdict{
		Output: strings.Repeat("- at '/endpoints/0/backend/0/extra_config': additional properties 'qos/circuit-breakr' not allowed\n", 1000)}
	v := &PolicyValidator{Client: fakeClient(referencing()...),
		Checker: &scriptedChecker{verdicts: []configcheck.Verdict{huge}}}

	resp := review(t, v, "alice", testPolicy(`{"qos/circuit-breakr":{}}`), testPolicy(`{}`))

	if resp.Allowed {
		t.Fatal("admitted a policy that fails on its own")
	}
	if n := len(resp.Result.Message); n > 4*warningLimit {
		t.Errorf("denial is %d bytes, want it bounded near %d", n, warningLimit)
	}
}

func TestPolicyAdmission_RejectsEnterpriseOnlyRawOnACEGateway(t *testing.T) {
	const proxy = `{"backend/http/client":{"proxy_address":"http://p"}}`
	onEE := referencing()
	onEE[0].(*v1alpha1.KrakenDGateway).Spec.Edition = v1alpha1.EditionEE
	cached := testPolicy(proxy)
	cached.Spec.Cache = &v1alpha1.CacheSpec{Shared: true}
	tests := []struct {
		name    string
		objs    []client.Object
		policy  *v1alpha1.KrakenDBackendPolicy
		old     *v1alpha1.KrakenDBackendPolicy
		allowed bool
		calls   string
	}{
		{"new raw on a CE gateway", referencing(), testPolicy(proxy), nil, false, "policy"},
		{"new raw on an EE gateway", onEE, testPolicy(proxy), nil, true, "policy,root,group"},
		{"new raw, nothing references the policy", nil, testPolicy(proxy), nil, true, "policy"},
		{"new raw the CE render honors", referencing(),
			testPolicy(`{"backend/http/client":{"send_body_on_redirect":true}}`), nil, true,
			"policy,root,group"},
		{"raw unchanged, another field edited", referencing(), cached, testPolicy(proxy), true,
			"policy,root,group"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chk := &scriptedChecker{}
			v := &PolicyValidator{Client: fakeClient(tt.objs...), Checker: chk}
			var old runtime.Object
			if tt.old != nil {
				old = tt.old
			}
			resp := review(t, v, "alice", tt.policy, old)
			if resp.Allowed != tt.allowed {
				t.Errorf("allowed = %v, want %v (%+v)", resp.Allowed, tt.allowed, resp.Result)
			}
			if !tt.allowed && (resp.Result.Code != http.StatusUnprocessableEntity ||
				!strings.Contains(resp.Result.Message, "gateway default/gw runs CE")) {
				t.Errorf("denial = %+v, want a 422 naming the CE gateway", resp.Result)
			}
			if got := strings.Join(chk.calls, ","); got != tt.calls {
				t.Errorf("checks = %s, want %s", got, tt.calls)
			}
		})
	}
}

func TestPolicyAdmission_NamesWhatACEGatewayDrops(t *testing.T) {
	v := &PolicyValidator{Client: fakeClient(referencing()...), Checker: &scriptedChecker{}}
	raw := `{"auth/gcp":{"audience":"https://a"},"backend/http/client":{"proxy_address":"http://p"}}`

	resp := review(t, v, "alice", testPolicy(raw), nil)

	if resp.Allowed || len(resp.Result.Details.Causes) != 1 {
		t.Fatalf("response = %+v, want one cause", resp.Result)
	}
	cause := resp.Result.Details.Causes[0]
	if cause.Field != "spec.raw" || !strings.Contains(cause.Message, `"auth/gcp, backend/http/client (proxy_address)"`) {
		t.Errorf("cause = %+v, want spec.raw naming the namespace and the dropped key", cause)
	}
}

func TestPolicyAdmission_DenialListsABoundedNumberOfGateways(t *testing.T) {
	names := make([]string, 2*maxEntryCauses)
	verdicts := []configcheck.Verdict{{OK: true}}
	var endpointVerdicts []configcheck.EndpointVerdict
	for i := range names {
		names[i] = fmt.Sprintf("gw-%02d", i)
		verdicts = append(verdicts, configcheck.Verdict{OK: true}, configcheck.Verdict{Output: "x"})
		endpointVerdicts = append(endpointVerdicts,
			configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}, configcheck.EndpointVerdict{OK: true})
	}
	objs := referencingGateways(names...)
	v := &PolicyValidator{Client: fakeClient(objs...),
		Checker: &scriptedChecker{verdicts: verdicts, endpointVerdicts: endpointVerdicts}}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed {
		t.Fatal("admitted a policy that breaks every gateway")
	}
	causes := resp.Result.Details.Causes
	if len(causes) > maxEntryCauses+1 {
		t.Errorf("%d causes, want at most %d and a summary of the rest", len(causes), maxEntryCauses+1)
	}
	if last := causes[len(causes)-1].Message; !strings.Contains(last, "refused on 20 more gateways") {
		t.Errorf("last cause = %q, want it to count the 20 gateways left out", last)
	}
}

// referencingGateways returns a CE gateway per name and an endpoint on each
// that references policy p.
func referencingGateways(names ...string) []client.Object {
	var objs []client.Object
	for _, name := range names {
		gw := testGateway()
		gw.Name = name
		ep := testEndpoint("uses-p-"+name, "/a")
		ep.Spec.GatewayRef.Name = name
		ep.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
		objs = append(objs, gw, ep)
	}
	return objs
}

// Each gateway is judged with the new policy, and the endpoints of those that
// fail are checked with the new policy and the stored one: the breaking
// gateway is the one cause, the already failing one a warning.
func TestPolicyAdmission_JudgesEachGatewayWithTheRightPolicy(t *testing.T) {
	const stored, changed = `{}`, `{"x":{}}`
	ok, fail := configcheck.Verdict{OK: true}, configcheck.Verdict{Output: "x"}
	epFail := configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}
	chk := &scriptedChecker{
		verdicts: []configcheck.Verdict{
			ok,       // the new policy alone
			ok, fail, // gw-a: root, its endpoints with the new policy
			ok, ok, // gw-b
			ok, fail, // gw-c
		},
		endpointVerdicts: []configcheck.EndpointVerdict{
			epFail, {OK: true}, // gw-a's endpoint: with the new policy, with the stored one
			epFail, epFail, // gw-c's endpoint
		},
	}
	v := &PolicyValidator{Client: fakeClient(referencingGateways("gw-a", "gw-b", "gw-c")...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(changed), testPolicy(stored))

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want 422", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || !strings.Contains(causes[0].Message, "breaks gateway default/gw-a") {
		t.Errorf("causes = %+v, want one naming gateway default/gw-a", causes)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "gateway default/gw-c: endpoints that use this policy already fail") {
		t.Errorf("warnings = %q, want one naming gateway default/gw-c", resp.Warnings)
	}
	wantArgs := []string{
		"policy:" + changed,
		"-", "default/gw-a:" + changed,
		"default/uses-p-gw-a[GET /a]:" + changed, "default/uses-p-gw-a[GET /a]:" + stored,
		"-", "default/gw-b:" + changed,
		"-", "default/gw-c:" + changed,
		"default/uses-p-gw-c[GET /a]:" + changed, "default/uses-p-gw-c[GET /a]:" + stored,
	}
	if got := strings.Join(chk.args, " "); got != strings.Join(wantArgs, " ") {
		t.Errorf("checks received %s, want %s", got, strings.Join(wantArgs, " "))
	}
	for i, d := range chk.deadlines {
		if d <= 0 || d > admissionBudget {
			t.Errorf("check %d had %s left, want a deadline within %s", i, d, admissionBudget)
		}
	}
}

// A check that cannot run leaves its gateway unjudged. With no gateway
// refusing the change, the request is a retryable 500, never a partial verdict.
func TestPolicyAdmission_CheckerErrorMidFanOutIs500(t *testing.T) {
	chk := &scriptedChecker{err: errors.New("no slot"), failCall: 3}
	v := &PolicyValidator{Client: fakeClient(referencingGateways("gw-a", "gw-b")...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
	if got := strings.Join(chk.calls, ","); got != "policy,root,group" {
		t.Errorf("checks = %s, want every gateway screened, each stopping at the check that cannot run", got)
	}
}

// Removing a finalizer from a terminating policy must never be refused, even
// when the stored raw reads differently as bytes: the spec is the same.
func TestPolicyAdmission_TerminatingPolicyWithTheSameSpecIsNotValidated(t *testing.T) {
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{Output: "bad"}}}
	v := &PolicyValidator{Client: fakeClient(), Checker: chk}
	old := terminating(testPolicy(`{"a":1}`))
	policy := unfinalized(testPolicy(`{ "a": 1 }`))
	policy.SetDeletionTimestamp(old.GetDeletionTimestamp())

	warnings, err := v.ValidateUpdate(context.Background(), old, policy)

	if err != nil || len(warnings) != 0 {
		t.Errorf("warnings = %q, err = %v, want the finalizer removal admitted silently", warnings, err)
	}
	if len(chk.calls) != 0 {
		t.Errorf("checks = %v, want none", chk.calls)
	}
}

// A policy on many gateways that already fail must not answer with a warning
// per gateway: the count and the bytes stay bounded.
func TestPolicyAdmission_WarningsAreBounded(t *testing.T) {
	names := make([]string, 40)
	verdicts := []configcheck.Verdict{{OK: true}}
	var endpointVerdicts []configcheck.EndpointVerdict
	for i := range names {
		names[i] = fmt.Sprintf("gw-%02d", i)
		verdicts = append(verdicts, configcheck.Verdict{OK: true}, configcheck.Verdict{Output: "x"})
		endpointVerdicts = append(endpointVerdicts,
			configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: strings.Repeat("x", 3*warningLimit)},
			configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: strings.Repeat("y", 3*warningLimit)})
	}
	v := &PolicyValidator{Client: fakeClient(referencingGateways(names...)...),
		Checker: &scriptedChecker{verdicts: verdicts, endpointVerdicts: endpointVerdicts}}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if !resp.Allowed {
		t.Fatalf("response = %+v, want the change admitted: every gateway already fails", resp.Result)
	}
	total := 0
	for _, w := range resp.Warnings {
		total += len(w)
	}
	// The API server cuts every warning of a response to 256 characters once
	// they pass 4096 in all.
	const apiServerWarningBudget = 4096
	if len(resp.Warnings) > 6 || total > apiServerWarningBudget {
		t.Errorf("%d warnings of %d bytes, want at most 6 and %d bytes", len(resp.Warnings), total, apiServerWarningBudget)
	}
	if last := resp.Warnings[len(resp.Warnings)-1]; !strings.Contains(last, "35 more gateways already fail validation") {
		t.Errorf("last warning = %q, want it to count the 35 gateways left out", last)
	}
}

func TestPolicyAdmission_TheDenialNamesTheBrokenEndpointsAndQuotesNone(t *testing.T) {
	stored := testPolicy(`{"qos/http-cache":{"shared":true}}`)
	changed := testPolicy(`{"qos/http-cache":{"shared":false}}`)
	chk := &scriptedChecker{
		verdicts: []configcheck.Verdict{{OK: true}, {OK: true}, {Output: "SECRET-OF-uses-p"}},
		endpointVerdicts: []configcheck.EndpointVerdict{
			{Reason: v1alpha1.ReasonEndpointInvalid, Output: "SECRET-OF-uses-p"}, {OK: true}},
	}
	v := &PolicyValidator{Client: fakeClient(append(referencing(), stored)...), Checker: chk}

	resp := review(t, v, "alice", changed, stored)

	if resp.Allowed || !strings.Contains(responseText(resp), "default/uses-p") || strings.Contains(responseText(resp), "SECRET") {
		t.Errorf("response = %+v; want a denial naming default/uses-p and quoting nothing of it", resp.Result)
	}
	if got := strings.Join(chk.calls, ","); got != "policy,root,group,endpoint,endpoint" {
		t.Errorf("checks = %s, want the policy alone, the root, the group with the change, then the endpoint "+
			"with the change and with the stored policy", got)
	}
}

// policyUsers returns the gateway and, for each name, an endpoint on it that
// references policy p, in name order.
func policyUsers(names ...string) []client.Object {
	objs := []client.Object{testGateway()}
	for i, name := range names {
		ep := testEndpoint(name, fmt.Sprintf("/u%d", i))
		ep.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
		objs = append(objs, ep)
	}
	return objs
}

// An endpoint that failed before the change, and that its gateway has not
// excluded yet, must not hide one that the change breaks.
func TestPolicyAdmission_AnAlreadyFailingEndpointDoesNotHideABrokenOne(t *testing.T) {
	epFail := configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}
	chk := &scriptedChecker{
		// The policy alone, the root, the group with the change; then the
		// group with the stored policy, which stale fails too: a judgement
		// that would hide victim, so the webhook must not rely on it.
		verdicts: []configcheck.Verdict{{OK: true}, {OK: true}, {Output: "x"}, {Output: "x"}},
		endpointVerdicts: []configcheck.EndpointVerdict{
			epFail, epFail, // stale: fails with the change and without it
			epFail, {OK: true}, // victim: fails only with the change
		},
	}
	v := &PolicyValidator{Client: fakeClient(policyUsers("stale", "victim")...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, warnings %q; want a 422", resp.Result, resp.Warnings)
	}
	if text := responseText(resp); !strings.Contains(text, "default/victim") || strings.Contains(text, "default/stale") {
		t.Errorf("denial = %q, want it to name default/victim only", text)
	}
}

func TestPolicyAdmission_AMaskedEndpointIsJudgedOnItsOwn(t *testing.T) {
	chk := &scriptedChecker{
		verdicts: []configcheck.Verdict{{OK: true}, {OK: true},
			{OK: true, Masked: []types.NamespacedName{{Namespace: "default", Name: "uses-p"}}}},
		endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}, {OK: true}},
	}
	v := &PolicyValidator{Client: fakeClient(referencing()...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed || !strings.Contains(responseText(resp), "default/uses-p") {
		t.Errorf("response = %+v; want a denial naming default/uses-p, whose lost entry the group never checked", resp.Result)
	}
	if got := strings.Join(chk.calls, ","); got != "policy,root,group,endpoint,endpoint" {
		t.Errorf("checks = %s, want the masked endpoint checked on its own with and without the change", got)
	}
}

func TestPolicyAdmission_AnExcludedEndpointIsNotJudged(t *testing.T) {
	stored := testPolicy(`{"qos/http-cache":{"shared":true}}`)
	changed := testPolicy(`{"qos/http-cache":{"shared":false}}`)
	objs := referencing()
	ep := objs[1].(*v1alpha1.KrakenDEndpoint)
	ep.Generation = 2
	ep.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonEndpointInvalid, ObservedGeneration: 2}}
	chk := &scriptedChecker{}
	v := &PolicyValidator{Client: fakeClient(append(objs, stored)...), Checker: chk}

	if resp := review(t, v, "alice", changed, stored); !resp.Allowed || strings.Join(chk.calls, ",") != "policy,root" {
		t.Errorf("allowed = %v, checks = %v; want admitted with only the policy and the root checked", resp.Allowed, chk.calls)
	}
}

func TestPolicyAdmission_NamingStopsAtTwentyEndpoints(t *testing.T) {
	names := make([]string, maxEntryCauses+5)
	var endpointVerdicts []configcheck.EndpointVerdict
	for i := range names {
		names[i] = fmt.Sprintf("uses-p-%02d", i)
		endpointVerdicts = append(endpointVerdicts,
			configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}, configcheck.EndpointVerdict{OK: true})
	}
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {OK: true}, {Output: "x"}},
		endpointVerdicts: endpointVerdicts}
	v := &PolicyValidator{Client: fakeClient(policyUsers(names...)...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	text := responseText(resp)
	if resp.Allowed || !strings.Contains(text, "default/uses-p-19") || strings.Contains(text, "default/uses-p-20") ||
		!strings.Contains(text, "(+5 more not checked)") {
		t.Errorf("denial = %q, want the first 20 endpoints named and 5 counted", text)
	}
}

func TestPolicyAdmission_ADenialSurvivesALaterCheckThatCannotRun(t *testing.T) {
	chk := &scriptedChecker{err: errors.New("no slot"), failCall: 6,
		verdicts:         []configcheck.Verdict{{OK: true}, {OK: true}, {Output: "x"}},
		endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}, {OK: true}}}
	v := &PolicyValidator{Client: fakeClient(policyUsers("victim", "zz-other")...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want the 422 the first endpoint already earned", resp.Result)
	}
	if text := responseText(resp); !strings.Contains(text, "default/victim") ||
		!strings.Contains(text, "(1 not checked within the admission time)") {
		t.Errorf("denial = %q, want default/victim named and the endpoint left unchecked counted", text)
	}
}

func TestPolicyAdmission_NamingStopsBeforeTheBudget(t *testing.T) {
	names := make([]string, 15)
	var endpointVerdicts []configcheck.EndpointVerdict
	for i := range names {
		names[i] = fmt.Sprintf("uses-p-%02d", i)
		endpointVerdicts = append(endpointVerdicts,
			configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}, configcheck.EndpointVerdict{OK: true})
	}
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {OK: true}, {Output: "x"}},
		endpointVerdicts: endpointVerdicts, delay: 300 * time.Millisecond}
	v := &PolicyValidator{Client: fakeClient(policyUsers(names...)...), Checker: chk}
	const deadline = 2500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()

	_, err := v.ValidateUpdate(ctx, testPolicy(`{}`), testPolicy(`{"x":{}}`))

	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "not checked within the admission time") ||
		strings.Contains(err.Error(), "more not checked)") {
		t.Fatalf("err = %v, want a 422 that the deadline, not the cap, cut short", err)
	}
	if took := time.Since(start); took > deadline+500*time.Millisecond {
		t.Errorf("answered after %s, want soon after the %s deadline", took, deadline)
	}
}

func TestPolicyAdmission_ScreensEveryGatewayBeforeNaming(t *testing.T) {
	chk := &scriptedChecker{
		verdicts:         []configcheck.Verdict{{OK: true}, {OK: true}, {Output: "x"}, {OK: true}, {OK: true}},
		endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}, {OK: true}},
	}
	v := &PolicyValidator{Client: fakeClient(referencingGateways("gw-a", "gw-b")...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed {
		t.Errorf("admitted a policy that breaks default/uses-p-gw-a")
	}
	if got := strings.Join(chk.calls, ","); got != "policy,root,group,root,group,endpoint,endpoint" {
		t.Errorf("checks = %s, want both gateways screened before any endpoint is named", got)
	}
}
