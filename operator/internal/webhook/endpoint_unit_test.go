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

// ownFailure is an endpoint verdict quoting the endpoint's own output.
func ownFailure(output string) configcheck.EndpointVerdict {
	return configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: output}
}

func TestEndpointAdmission_DeniesAnEndpointThatFailsOnItsOwn(t *testing.T) {
	chk := &scriptedChecker{endpointVerdicts: []configcheck.EndpointVerdict{
		ownFailure("- at '/endpoints/0/backend/0/host/0': svc:bad is not a host")}}
	v := &EndpointValidator{Client: fakeClient(testGateway()), Checker: chk, Memo: newAdmissionMemo()}

	resp := review(t, v, "alice", testEndpoint("new", "/a"), nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || causes[0].Field != "spec.endpoints" || !strings.Contains(causes[0].Message, "svc:bad is not a host") {
		t.Errorf("causes = %+v, want one on spec.endpoints quoting the endpoint's own output", causes)
	}
	if got := strings.Join(chk.calls, ","); got != "root,endpoint" {
		t.Errorf("checks = %s, want the root alone then the endpoint alone", got)
	}
	for i, d := range chk.deadlines {
		if d <= 0 || d > admissionBudget || !chk.memos[i] {
			t.Errorf("check %d ran with %s left and memo %v, want a deadline within %s and the memo", i, d, chk.memos[i], admissionBudget)
		}
	}
}

func TestEndpointAdmission_AGatewayRootThatFailsAloneOnlyWarns(t *testing.T) {
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{Output: "ROOT-SECRET is refused"}}}
	v := &EndpointValidator{Client: fakeClient(testGateway()), Checker: chk}

	resp := review(t, v, "alice", testEndpoint("new", "/a"), nil)

	if !resp.Allowed || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "default/gw") ||
		strings.Contains(responseText(resp), "ROOT-SECRET") {
		t.Errorf("response = %+v, warnings %v; want admitted with one warning naming default/gw and quoting nothing", resp.Result, resp.Warnings)
	}
	if got := strings.Join(chk.calls, ","); got != "root" {
		t.Errorf("checks = %s, want the root alone", got)
	}
}

func TestEndpointAdmission_AnUpdateThatStillFailsOnlyWarns(t *testing.T) {
	stored := testEndpoint("ep", "/a")
	updated := stored.DeepCopy()
	updated.Spec.Endpoints[0].Backends[0].URLPattern = "/b"
	chk := &scriptedChecker{endpointVerdicts: []configcheck.EndpointVerdict{ownFailure("still bad"), ownFailure("was bad")}}
	v := &EndpointValidator{Client: fakeClient(testGateway(), stored), Checker: chk}

	resp := review(t, v, "alice", updated, stored)

	if !resp.Allowed || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "still bad") {
		t.Errorf("response = %+v, warnings %v; want admitted with a warning quoting its own output", resp.Result, resp.Warnings)
	}
	if got := strings.Join(chk.calls, ","); got != "root,endpoint,endpoint" {
		t.Errorf("checks = %s, want the root, the update, then the stored version", got)
	}
}

func TestEndpointAdmission_AnUpdateThatNewlyFailsIsDenied(t *testing.T) {
	stored := testEndpoint("ep", "/a")
	updated := stored.DeepCopy()
	updated.Spec.Endpoints[0].Backends[0].URLPattern = "/b"
	chk := &scriptedChecker{endpointVerdicts: []configcheck.EndpointVerdict{ownFailure("newly bad"), {OK: true}}}
	v := &EndpointValidator{Client: fakeClient(testGateway(), stored), Checker: chk}

	if resp := review(t, v, "alice", updated, stored); resp.Allowed {
		t.Errorf("an update that newly fails on its own was admitted: %v", resp.Warnings)
	}
}

func TestEndpointAdmission_AMoveIsJudgedLikeACreate(t *testing.T) {
	elsewhere := testGateway()
	elsewhere.Name = "elsewhere"
	stored := testEndpoint("ep", "/a")
	stored.Spec.GatewayRef.Name = "elsewhere"
	moved := testEndpoint("ep", "/a")
	chk := &scriptedChecker{endpointVerdicts: []configcheck.EndpointVerdict{ownFailure("bad here")}}
	v := &EndpointValidator{Client: fakeClient(testGateway(), elsewhere, stored), Checker: chk}

	resp := review(t, v, "alice", moved, stored)

	if resp.Allowed || strings.Join(chk.calls, ",") != "root,endpoint" {
		t.Errorf("allowed = %v, checks = %v; want a denial without judging the version on the other gateway", resp.Allowed, chk.calls)
	}
}

func TestEndpointAdmission_APolicyAtFaultIsNamedNotQuoted(t *testing.T) {
	chk := &scriptedChecker{endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonPolicyInvalid,
		Policies: []types.NamespacedName{{Namespace: "shared", Name: "p"}}, PoliciesFailAlone: true}}}
	v := &EndpointValidator{Client: fakeClient(testGateway()), Checker: chk}

	resp := review(t, v, "alice", testEndpoint("new", "/a"), nil)

	if resp.Allowed || !strings.Contains(responseText(resp), "shared/p") {
		t.Errorf("response = %+v; want a denial naming shared/p", resp.Result)
	}
}

func TestEndpointAdmission_ACheckThatCannotRunIs500(t *testing.T) {
	stored := testEndpoint("ep", "/a")
	updated := stored.DeepCopy()
	updated.Spec.Endpoints[0].Backends[0].URLPattern = "/b"
	for name, failCall := range map[string]int{"the root": 1, "the endpoint": 2, "its stored version": 3} {
		t.Run(name, func(t *testing.T) {
			chk := &scriptedChecker{err: errors.New("no slot"), failCall: failCall,
				endpointVerdicts: []configcheck.EndpointVerdict{ownFailure("bad")}}
			v := &EndpointValidator{Client: fakeClient(testGateway(), stored), Checker: chk}

			resp := review(t, v, "alice", updated, stored)

			if resp.Allowed || resp.Result.Code != http.StatusInternalServerError || len(resp.Warnings) != 0 {
				t.Errorf("response = %+v, warnings %v; want a 500 with no warning", resp.Result, resp.Warnings)
			}
		})
	}
}

func TestEndpointAdmission_TheDenialIsBounded(t *testing.T) {
	huge := strings.Repeat("- at '/endpoints/0/backend/0/extra_config': additional properties 'x' not allowed\n", 1000)
	chk := &scriptedChecker{endpointVerdicts: []configcheck.EndpointVerdict{ownFailure(huge)}}
	v := &EndpointValidator{Client: fakeClient(testGateway()), Checker: chk}

	resp := review(t, v, "alice", testEndpoint("new", "/a"), nil)

	if text := responseText(resp); len(text) > 3*warningLimit || !strings.Contains(text, "more)") {
		t.Errorf("denial is %d bytes, want it bounded with a count of what it leaves out", len(text))
	}
}
