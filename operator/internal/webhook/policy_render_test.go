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
	bad := failing("policy-lint", 0,
		"- at '/endpoints/0/backend/0/extra_config': additional properties 'qos/circuit-breakr' not allowed")
	tests := []struct {
		name     string
		objs     []client.Object
		old      *v1alpha1.KrakenDBackendPolicy
		verdicts []configcheck.Verdict
		allowed  bool
		calls    string
		warning  string // a substring of the one warning expected; "" for none
	}{
		{"unreferenced, lints clean", nil, nil, nil, true, "policy", ""},
		{"unreferenced, fails alone", nil, nil, []configcheck.Verdict{bad}, false, "policy", ""},
		{"referenced, keeps its gateway passing", referencing(), nil, nil, true, "policy,gateway+policy", ""},
		{"referenced, breaks its gateway", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{{OK: true}, failing("uses-p", 0, "bad"), {OK: true}}, false,
			"policy,gateway+policy,gateway+policy", ""},
		{"created, referenced, breaks its gateway", referencing(), nil,
			[]configcheck.Verdict{{OK: true}, failing("uses-p", 0, "bad"), {OK: true}}, false,
			"policy,gateway+policy,gateway", ""},
		{"referenced gateway already broken", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{{OK: true}, failing("uses-p", 0, "bad"), failing("other", 0, "old")}, true,
			"policy,gateway+policy,gateway+policy", "gateway default/gw already fails validation"},
		{"failing alone before and after: the gateways decide", referencing(), testPolicy(`{"x":{}}`),
			[]configcheck.Verdict{bad, bad}, true, "policy,policy,gateway+policy", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chk := &scriptedChecker{verdicts: tt.verdicts}
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
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{failing("policy-lint", 0, "bad")}}
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
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{failing("policy-lint", 0, "bad"), {OK: true}}}
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
	huge := configcheck.Verdict{}
	for range 1000 {
		huge.Findings = append(huge.Findings, configcheck.Finding{
			Endpoint: types.NamespacedName{Namespace: "default", Name: "uses-p"}, Index: 0,
			Message: "- at '/endpoints/0/backend/0/extra_config': additional properties 'qos/circuit-breakr' not allowed"})
	}
	v := &PolicyValidator{Client: fakeClient(referencing()...),
		Checker: &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, huge, {OK: true}}}}

	resp := review(t, v, "alice", testPolicy(`{"qos/circuit-breakr":{}}`), testPolicy(`{}`))

	if resp.Allowed {
		t.Fatal("admitted a policy that breaks its gateway")
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
		{"new raw on an EE gateway", onEE, testPolicy(proxy), nil, true, "policy,gateway+policy"},
		{"new raw, nothing references the policy", nil, testPolicy(proxy), nil, true, "policy"},
		{"new raw the CE render honors", referencing(),
			testPolicy(`{"backend/http/client":{"send_body_on_redirect":true}}`), nil, true,
			"policy,gateway+policy"},
		{"raw unchanged, another field edited", referencing(), cached, testPolicy(proxy), true,
			"policy,gateway+policy"},
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
	objs := []client.Object{}
	verdicts := []configcheck.Verdict{{OK: true}}
	for i := range 2 * maxEntryCauses {
		gw := testGateway()
		gw.Name = fmt.Sprintf("gw-%02d", i)
		ep := testEndpoint(fmt.Sprintf("uses-p-%02d", i), "/a")
		ep.Spec.GatewayRef.Name = gw.Name
		ep.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
		objs = append(objs, gw, ep)
		verdicts = append(verdicts, failing("e", 0, "bad"), configcheck.Verdict{OK: true})
	}
	v := &PolicyValidator{Client: fakeClient(objs...), Checker: &scriptedChecker{verdicts: verdicts}}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed {
		t.Fatal("admitted a policy that breaks every gateway")
	}
	causes := resp.Result.Details.Causes
	if len(causes) > maxEntryCauses+1 {
		t.Errorf("%d causes, want at most %d and a summary of the rest", len(causes), maxEntryCauses+1)
	}
	if last := causes[len(causes)-1].Message; !strings.Contains(last, "20 more gateways") {
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

// Each gateway is judged on its own, with the new policy after and the stored
// one before: the breaking gateway is the one cause, the failing one a warning.
func TestPolicyAdmission_JudgesEachGatewayWithTheRightPolicy(t *testing.T) {
	const stored, changed = `{}`, `{"x":{}}`
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{
		{OK: true},                  // the new policy alone
		failing("e", 0, "bad"),      // gw-a with the new policy
		{OK: true},                  // gw-a with the stored policy
		{OK: true},                  // gw-b with the new policy
		failing("e", 0, "bad"),      // gw-c with the new policy
		failing("e", 0, "old fail"), // gw-c with the stored policy
	}}
	v := &PolicyValidator{Client: fakeClient(referencingGateways("gw-a", "gw-b", "gw-c")...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(changed), testPolicy(stored))

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want 422", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || !strings.Contains(causes[0].Message, "breaks gateway default/gw-a") {
		t.Errorf("causes = %+v, want one naming gateway default/gw-a", causes)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "gateway default/gw-c already fails") {
		t.Errorf("warnings = %q, want one naming gateway default/gw-c", resp.Warnings)
	}
	wantArgs := []string{
		"policy:" + changed,
		"default/gw-a:" + changed, "default/gw-a:" + stored,
		"default/gw-b:" + changed,
		"default/gw-c:" + changed, "default/gw-c:" + stored,
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

// A check that cannot run in the middle of the fan-out leaves the request
// unjudged: a retryable 500, never a partial verdict.
func TestPolicyAdmission_CheckerErrorMidFanOutIs500(t *testing.T) {
	chk := &scriptedChecker{err: errors.New("no slot"), failCall: 3}
	v := &PolicyValidator{Client: fakeClient(referencingGateways("gw-a", "gw-b")...), Checker: chk}

	resp := review(t, v, "alice", testPolicy(`{"x":{}}`), testPolicy(`{}`))

	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
	if got := strings.Join(chk.calls, ","); got != "policy,gateway+policy,gateway+policy" {
		t.Errorf("checks = %s, want the fan-out to stop at the failing check", got)
	}
}

// Removing a finalizer from a terminating policy must never be refused, even
// when the stored raw reads differently as bytes: the spec is the same.
func TestPolicyAdmission_TerminatingPolicyWithTheSameSpecIsNotValidated(t *testing.T) {
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{failing("policy-lint", 0, "bad")}}
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
