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
	}{
		{"unreferenced, lints clean", nil, nil, nil, true, "policy"},
		{"unreferenced, fails alone", nil, nil, []configcheck.Verdict{bad}, false, "policy"},
		{"referenced, keeps its gateway passing", referencing(), nil, nil, true, "policy,gateway+policy"},
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
		})
	}
}
