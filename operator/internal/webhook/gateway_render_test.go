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

	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

func rootFailure(msg string) configcheck.Verdict {
	return configcheck.Verdict{Findings: []configcheck.Finding{{Index: -1, Message: msg}}}
}

func TestGatewayAdmission_Render(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	tests := []struct {
		name     string
		old      *v1alpha1.KrakenDGateway
		verdicts []configcheck.Verdict
		allowed  bool
		calls    string
	}{
		{"create checks the root alone", nil, nil, true, "isolated"},
		{"create with a failing root", nil,
			[]configcheck.Verdict{rootFailure("'timeout' time: unknown unit")}, false, "isolated"},
		{"update keeps it passing", old, nil, true, "gateway"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chk := &scriptedChecker{verdicts: tt.verdicts}
			v := &GatewayValidator{Client: fakeClient(), Checker: chk}
			obj := testGateway()
			var oldObj runtime.Object
			if tt.old != nil {
				obj, oldObj = edited, tt.old
			}
			resp := review(t, v, "alice", obj, oldObj)
			if resp.Allowed != tt.allowed {
				t.Errorf("allowed = %v, want %v (%+v)", resp.Allowed, tt.allowed, resp.Result)
			}
			if !tt.allowed && resp.Result.Code != http.StatusUnprocessableEntity {
				t.Errorf("code = %d, want 422", resp.Result.Code)
			}
			if got := strings.Join(chk.calls, ","); got != tt.calls {
				t.Errorf("checks = %s, want %s", got, tt.calls)
			}
		})
	}
}
