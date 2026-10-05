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
			"policy,gateway+policy,gateway", ""},
		{"referenced gateway already broken", referencing(), testPolicy(`{}`),
			[]configcheck.Verdict{{OK: true}, failing("uses-p", 0, "bad"), failing("other", 0, "old")}, true,
			"policy,gateway+policy,gateway", "gateway default/gw already fails validation"},
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
