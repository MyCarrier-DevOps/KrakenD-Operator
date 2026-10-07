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
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

// editedGateway returns the stored test gateway and an update of it that
// changes its root.
func editedGateway() (old, gw *v1alpha1.KrakenDGateway) {
	old = testGateway()
	gw = testGateway()
	gw.Spec.Config.Timeout = "1s"
	return old, gw
}

func TestGatewayAdmission_AnUpdateThatBreaksAServedEndpointIsDeniedByName(t *testing.T) {
	old, gw := editedGateway()
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "SECRET-OF-ep"}},
		endpointVerdicts: []configcheck.EndpointVerdict{
			{Reason: v1alpha1.ReasonEndpointInvalid, Output: "SECRET-OF-ep"}, {OK: true}}}
	v := &GatewayValidator{Client: fakeClient(old, testEndpoint("ep", "/a")), Checker: chk, Memo: newAdmissionMemo()}

	resp := review(t, v, "alice", gw, old)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(responseText(resp), "default/ep") || strings.Contains(responseText(resp), "SECRET") {
		t.Errorf("response = %+v; want a 422 naming default/ep and quoting nothing of it", resp.Result)
	}
	if got := strings.Join(chk.calls, ","); got != "root,group,endpoint,endpoint" {
		t.Errorf("checks = %s, want the root, the served endpoints with the new root, then the endpoint on its "+
			"own with the new root and with the stored one", got)
	}
}

// An endpoint that failed before the update, and that the gateway has not
// excluded yet, must not hide one that the update breaks.
func TestGatewayAdmission_AnAlreadyFailingEndpointDoesNotHideABrokenOne(t *testing.T) {
	old, gw := editedGateway()
	epFail := configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}
	chk := &scriptedChecker{
		// The root, the served endpoints with the update; then the served
		// endpoints with the stored root, which stale fails too: a judgement
		// that would hide victim, so the webhook must not rely on it.
		verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}, {Output: "x"}},
		endpointVerdicts: []configcheck.EndpointVerdict{
			epFail, epFail, // stale: fails with the update and without it
			epFail, {OK: true}, // victim: fails only with the update
		},
	}
	v := &GatewayValidator{Client: fakeClient(old, testEndpoint("stale", "/a"), testEndpoint("victim", "/b")), Checker: chk}

	resp := review(t, v, "alice", gw, old)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, warnings %q; want a 422", resp.Result, resp.Warnings)
	}
	if text := responseText(resp); !strings.Contains(text, "default/victim") || strings.Contains(text, "default/stale") {
		t.Errorf("denial = %q, want it to name default/victim only", text)
	}
}

func TestGatewayAdmission_AMaskedEndpointIsJudgedOnItsOwn(t *testing.T) {
	old, gw := editedGateway()
	chk := &scriptedChecker{
		verdicts: []configcheck.Verdict{{OK: true},
			{OK: true, Masked: []types.NamespacedName{{Namespace: "default", Name: "ep"}}}},
		endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}, {OK: true}},
	}
	v := &GatewayValidator{Client: fakeClient(old, testEndpoint("ep", "/a")), Checker: chk}

	resp := review(t, v, "alice", gw, old)

	if resp.Allowed || !strings.Contains(responseText(resp), "default/ep") {
		t.Errorf("response = %+v; want a denial naming default/ep, whose lost entry the group never checked", resp.Result)
	}
	if got := strings.Join(chk.calls, ","); got != "root,group,endpoint,endpoint" {
		t.Errorf("checks = %s, want the masked endpoint checked on its own with the update and without it", got)
	}
}

func TestGatewayAdmission_ACreateWhoseWaitingEndpointsCannotBeCheckedWarns(t *testing.T) {
	chk := &scriptedChecker{err: errors.New("no validation slot in time"), failCall: 3,
		verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}}}

	resp := review(t, &GatewayValidator{Client: fakeClient(testEndpoint("ep", "/a")), Checker: chk},
		"alice", testGateway(), nil)

	if !resp.Allowed || len(resp.Warnings) != 1 ||
		!strings.Contains(resp.Warnings[0], "could not check the endpoints that already reference this gateway") {
		t.Errorf("response = %+v, warnings = %v, want admitted with a could-not-check warning", resp.Result, resp.Warnings)
	}
}
