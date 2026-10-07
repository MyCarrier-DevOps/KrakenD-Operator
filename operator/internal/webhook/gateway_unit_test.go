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
