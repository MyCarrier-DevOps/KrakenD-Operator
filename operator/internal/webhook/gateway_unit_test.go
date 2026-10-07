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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

func TestGatewayAdmission_NamingStopsBeforeTheBudget(t *testing.T) {
	old, gw := editedGateway()
	objs := []client.Object{old}
	var endpointVerdicts []configcheck.EndpointVerdict
	for i := range 15 {
		objs = append(objs, testEndpoint(fmt.Sprintf("ep-%02d", i), fmt.Sprintf("/e%d", i)))
		endpointVerdicts = append(endpointVerdicts,
			configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}, configcheck.EndpointVerdict{OK: true})
	}
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}},
		endpointVerdicts: endpointVerdicts, delay: 300 * time.Millisecond}
	v := &GatewayValidator{Client: fakeClient(objs...), Checker: chk}
	const deadline = 2500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()

	_, err := v.ValidateUpdate(ctx, old, gw)

	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "not checked within the admission time") ||
		strings.Contains(err.Error(), "more not checked)") {
		t.Fatalf("err = %v, want a 422 that the deadline, not the cap, cut short", err)
	}
	if took := time.Since(start); took > deadline+500*time.Millisecond {
		t.Errorf("answered after %s, want soon after the %s deadline", took, deadline)
	}
}

func TestGatewayAdmission_ALargeGatewayThatPassesRunsTwoChecks(t *testing.T) {
	old, gw := editedGateway()
	objs := []client.Object{old}
	for i := range 500 {
		objs = append(objs, testEndpoint(fmt.Sprintf("ep-%03d", i), fmt.Sprintf("/e%d", i)))
	}
	chk := &scriptedChecker{}
	v := &GatewayValidator{Client: fakeClient(objs...), Checker: chk}

	if resp := review(t, v, "alice", gw, old); !resp.Allowed || strings.Join(chk.calls, ",") != "root,group" {
		t.Errorf("allowed = %v, %d checks; want admitted after the root and one group check", resp.Allowed, len(chk.calls))
	}
}

func TestGatewayAdmission_AGatewayWithNoServedEndpointChecksOnlyItsRoot(t *testing.T) {
	old, gw := editedGateway()
	ep := testEndpoint("ep", "/a")
	ep.Generation = 2
	ep.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonEndpointInvalid, ObservedGeneration: 2}}
	chk := &scriptedChecker{}
	v := &GatewayValidator{Client: fakeClient(old, ep), Checker: chk}

	if resp := review(t, v, "alice", gw, old); !resp.Allowed || strings.Join(chk.calls, ",") != "root" {
		t.Errorf("allowed = %v, checks = %v; want admitted after the root alone", resp.Allowed, chk.calls)
	}
}
