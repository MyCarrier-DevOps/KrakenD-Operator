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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	broken := failing("ep", 0, "broken elsewhere")
	tests := []struct {
		name     string
		old      *v1alpha1.KrakenDGateway
		verdicts []configcheck.Verdict
		allowed  bool
		calls    string
		warns    string
	}{
		{"create checks the root alone", nil, nil, true, "isolated", ""},
		{"create with a failing root", nil,
			[]configcheck.Verdict{rootFailure("'timeout' time: unknown unit")}, false, "isolated", ""},
		{"update keeps it passing", old, nil, true, "gateway", ""},
		{"update breaks it", old, []configcheck.Verdict{broken, {OK: true}}, false, "gateway,gateway", ""},
		{"update of a broken gateway, root still fine", old, []configcheck.Verdict{broken, broken, {OK: true}}, true,
			"gateway,gateway,isolated", "already fails validation"},
		{"update of a broken gateway breaks the root", old,
			[]configcheck.Verdict{broken, broken, rootFailure("bad"), {OK: true}}, false,
			"gateway,gateway,isolated,isolated", ""},
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
			if tt.warns != "" && (len(resp.Warnings) == 0 || !strings.Contains(resp.Warnings[0], tt.warns)) {
				t.Errorf("warnings = %v, want %q", resp.Warnings, tt.warns)
			}
		})
	}
}

// A gateway write can break endpoints it does not own: each cause goes on the
// field the user edits, and the endpoints it breaks are named.
func TestGatewayAdmission_DenialAttributesFindings(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	after := failing("ep", 2, "bad regexp")
	after.Findings = append(after.Findings, rootFailure("'timeout' time: unknown unit").Findings...)
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{after, {OK: true}}}

	resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", edited, old)

	if resp.Allowed || resp.Result.Details == nil {
		t.Fatalf("response = %+v, want a denial with causes", resp.Result)
	}
	causes := map[string]string{}
	for _, c := range resp.Result.Details.Causes {
		causes[c.Field] = c.Message
	}
	if got := causes["spec.config"]; !strings.Contains(got, "unknown unit") || strings.Contains(got, "bad regexp") {
		t.Errorf("spec.config cause = %q, want the root finding alone", got)
	}
	if got := causes["spec"]; !strings.Contains(got, "default/ep spec.endpoints[2]: bad regexp") ||
		strings.Contains(got, "unknown unit") {
		t.Errorf("spec cause = %q, want the endpoint finding naming default/ep spec.endpoints[2]", got)
	}
}

// A root with hundreds of failures must not produce an unbounded denial.
func TestGatewayAdmission_DenialCausesAreBounded(t *testing.T) {
	var findings []configcheck.Finding
	for i := 0; i < 3*maxEntryCauses; i++ {
		findings = append(findings, configcheck.Finding{Index: -1, Message: strings.Repeat("x", 3*warningLimit)})
	}
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{Findings: findings}}}

	resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", testGateway(), nil)

	if resp.Allowed || resp.Result.Details == nil {
		t.Fatalf("response = %+v, want a denial with causes", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) > maxEntryCauses+1 {
		t.Errorf("%d causes, want at most %d", len(causes), maxEntryCauses+1)
	}
	for _, c := range causes {
		if len(c.Message) > 2*warningLimit {
			t.Errorf("cause on %s is %d bytes, want it cut near %d", c.Field, len(c.Message), warningLimit)
		}
	}
}

// A checker that cannot run, at any of the checks, is a transient 500: the
// request is not judged and clients retry.
func TestGatewayAdmission_ValidatorUnavailableIs500(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	broken := failing("ep", 0, "broken elsewhere")
	tests := []struct {
		name     string
		old      *v1alpha1.KrakenDGateway
		verdicts []configcheck.Verdict
		failCall int
	}{
		{"create", nil, nil, 1},
		{"update, the gateway with the change", old, nil, 1},
		{"update, the stored gateway", old, []configcheck.Verdict{broken}, 2},
		{"update, the root with the change", old, []configcheck.Verdict{broken, broken}, 3},
		{"update, the stored root", old, []configcheck.Verdict{broken, broken, rootFailure("bad")}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chk := &scriptedChecker{verdicts: tt.verdicts, failCall: tt.failCall,
				err: errors.New("waiting for a validation slot: context deadline exceeded")}
			obj := testGateway()
			var oldObj runtime.Object
			if tt.old != nil {
				obj, oldObj = edited, tt.old
			}

			resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", obj, oldObj)

			if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
				t.Errorf("response = %+v, want 500", resp.Result)
			}
			if !strings.Contains(resp.Result.Message, "validating the gateway config") {
				t.Errorf("message = %q, want it to say the validation could not run", resp.Result.Message)
			}
		})
	}
}

// Every check runs under the admission budget, so a slow validator answers 500
// before the API server's own timeout.
func TestGatewayAdmission_ChecksRunUnderTheAdmissionBudget(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	broken := failing("ep", 0, "broken elsewhere")
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{broken, broken, rootFailure("bad"), {OK: true}}}

	review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", edited, old)

	if len(chk.deadlines) != 4 {
		t.Fatalf("%d checks ran, want 4", len(chk.deadlines))
	}
	for i, left := range chk.deadlines {
		if left <= 0 || left > admissionBudget {
			t.Errorf("check %d ran with %s left, want a deadline within %s", i+1, left, admissionBudget)
		}
	}
}

func TestGatewayAdmission_WarnsOnAnotherKrakenDMinor(t *testing.T) {
	v := &GatewayValidator{Client: fakeClient(), Checker: &scriptedChecker{}}
	other := testGateway()
	other.Spec.Version = "2.12"
	patch := testGateway()
	patch.Spec.Version = "2.13.4"
	if resp := review(t, v, "alice", other, nil); len(resp.Warnings) != 1 ||
		!strings.Contains(resp.Warnings[0], "validated with KrakenD "+configcheck.ValidatorVersion) {
		t.Errorf("2.12 warnings = %v, want the version warning", resp.Warnings)
	}
	if resp := review(t, v, "alice", patch, nil); len(resp.Warnings) != 0 {
		t.Errorf("2.13.4 warnings = %v, want none", resp.Warnings)
	}
	labeled := other.DeepCopy()
	labeled.Spec.Replicas = ptr.To[int32](2)
	if resp := review(t, v, "alice", labeled, other); len(resp.Warnings) != 0 {
		t.Errorf("unchanged version warned again: %v", resp.Warnings)
	}
}

func TestGatewayAdmission_CERejectsEnterpriseOnlyNamespaces(t *testing.T) {
	withRoot := func(gw *v1alpha1.KrakenDGateway, raw string) *v1alpha1.KrakenDGateway {
		gw.Spec.Config.ExtraConfig = &runtime.RawExtension{Raw: []byte(raw)}
		return gw
	}
	edited := func(gw *v1alpha1.KrakenDGateway) *v1alpha1.KrakenDGateway {
		gw.Spec.Replicas = ptr.To[int32](2)
		return gw
	}
	ee := func() *v1alpha1.KrakenDGateway {
		gw := testGateway()
		gw.Spec.Edition = v1alpha1.EditionEE
		return gw
	}
	apiKeys := `{"auth/api-keys":{"keys":[]}}`
	keys := testEndpoint("keys", "/k")
	keys.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(`{"auth/api-keys":{"roles":["a"]}}`)}
	keys.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
	proxy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			Raw: &runtime.RawExtension{Raw: []byte(`{"backend/http/client":{"proxy_address":"http://p"}}`)},
		},
	}
	redirects := testEndpoint("redirects", "/r")
	redirects.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "r"}
	honored := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			Raw: &runtime.RawExtension{Raw: []byte(`{"backend/http/client":{"send_body_on_redirect":true}}`)},
		},
	}
	tests := []struct {
		name    string
		objs    []client.Object
		gw, old *v1alpha1.KrakenDGateway
		reject  []string // substrings of the denial; none means admitted
	}{
		{"CE root with an EE namespace", nil, withRoot(testGateway(), apiKeys), nil,
			[]string{`spec.config.extraConfig: Invalid value: "auth/api-keys"`}},
		{"CE root with CE namespaces", nil, withRoot(testGateway(), `{"security/cors":{"allow_origins":["*"]}}`), nil, nil},
		{"EE root with an EE namespace", nil, withRoot(ee(), apiKeys), nil, nil},
		{"unchanged CE root", nil, edited(withRoot(testGateway(), apiKeys)), withRoot(testGateway(), apiKeys), nil},
		{"EE to CE with EE namespaces in use", []client.Object{keys, proxy}, testGateway(), ee(), []string{
			`spec.edition: Invalid value: "CE"`,
			"KrakenDEndpoint default/keys spec.endpoints[0].extraConfig auth/api-keys",
			"KrakenDBackendPolicy default/p spec.raw backend/http/client"}},
		{"EE to CE with nothing Enterprise-only", []client.Object{testEndpoint("plain", "/p")}, testGateway(), ee(), nil},
		{"CE stays CE with stored EE namespaces", []client.Object{keys, proxy}, edited(testGateway()), testGateway(), nil},
		{"EE to CE with only keys CE honors", []client.Object{redirects, honored}, testGateway(), ee(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &GatewayValidator{Client: fakeClient(tt.objs...), Checker: &scriptedChecker{}}
			var old runtime.Object
			if tt.old != nil {
				old = tt.old
			}
			resp := review(t, v, "alice", tt.gw, old)
			if len(tt.reject) == 0 {
				if !resp.Allowed {
					t.Errorf("denied: %+v", resp.Result)
				}
				return
			}
			if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
				t.Fatalf("response = %+v, want a 422 denial", resp.Result)
			}
			for _, want := range tt.reject {
				if !strings.Contains(resp.Result.Message, want) {
					t.Errorf("denial %q does not contain %q", resp.Result.Message, want)
				}
			}
		})
	}
}

// An EE to CE switch that cannot list the gateway's endpoints is not judged:
// a transient 500.
func TestGatewayAdmission_EditionSwitchLookupFailureIs500(t *testing.T) {
	funcs := interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
		...client.ListOption) error {
		return errors.New("cache not synced")
	}}
	old := testGateway()
	old.Spec.Edition = v1alpha1.EditionEE
	v := &GatewayValidator{Client: fakeClientBuilderWith(funcs), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", testGateway(), old)

	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
	if !strings.Contains(resp.Result.Message, "listing the endpoints of gateway default/gw") {
		t.Errorf("message = %q, want it to name the failed lookup", resp.Result.Message)
	}
}

// The lookups of an edition switch run under the admission budget, so a slow
// cache read ends with a clear error before the API server's timeout.
func TestGatewayAdmission_EditionSwitchLookupsRunUnderTheAdmissionBudget(t *testing.T) {
	var listed, got time.Duration
	left := func(ctx context.Context) time.Duration {
		d, _ := ctx.Deadline()
		return time.Until(d)
	}
	funcs := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			listed = left(ctx)
			return c.List(ctx, l, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			got = left(ctx)
			return c.Get(ctx, key, obj, opts...)
		},
	}
	keys := testEndpoint("keys", "/k")
	keys.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
	old := testGateway()
	old.Spec.Edition = v1alpha1.EditionEE
	v := &GatewayValidator{Client: fakeClientBuilderWith(funcs, keys), Checker: &scriptedChecker{}}

	review(t, v, "alice", testGateway(), old)

	for what, d := range map[string]time.Duration{"endpoint List": listed, "policy Get": got} {
		if d <= 0 || d > admissionBudget {
			t.Errorf("the %s ran with %s left, want a deadline within %s", what, d, admissionBudget)
		}
	}
}

// A gateway with hundreds of endpoints using EE namespaces must not produce an
// unbounded denial.
func TestGatewayAdmission_EditionSwitchDenialIsBounded(t *testing.T) {
	var objs []client.Object
	for i := range 300 {
		ep := testEndpoint(fmt.Sprintf("keys-%d", i), "/k")
		ep.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(`{"auth/api-keys":{"roles":["a"]}}`)}
		objs = append(objs, ep)
	}
	old := testGateway()
	old.Spec.Edition = v1alpha1.EditionEE
	v := &GatewayValidator{Client: fakeClient(objs...), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", testGateway(), old)

	if resp.Allowed || resp.Result.Details == nil || len(resp.Result.Details.Causes) != 1 {
		t.Fatalf("response = %+v, want a denial with one cause", resp.Result)
	}
	if got := len(resp.Result.Details.Causes[0].Message); got > 2*warningLimit {
		t.Errorf("cause is %d bytes, want it cut near %d", got, warningLimit)
	}
}

func TestGatewayAdmission_CERejectsEnterpriseOnlyFields(t *testing.T) {
	withFields := func(gw *v1alpha1.KrakenDGateway) *v1alpha1.KrakenDGateway {
		gw.Spec.Redis = &v1alpha1.RedisSpec{
			ConnectionPool: v1alpha1.RedisConnectionPool{Addresses: []string{"redis:6379"}},
		}
		gw.Spec.Config.Documentation = &v1alpha1.DocumentationConfig{Version: "1.0"}
		gw.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
		return gw
	}
	edited := func(gw *v1alpha1.KrakenDGateway) *v1alpha1.KrakenDGateway {
		gw.Spec.Replicas = ptr.To[int32](2)
		return gw
	}
	ee := func() *v1alpha1.KrakenDGateway {
		gw := testGateway()
		gw.Spec.Edition = v1alpha1.EditionEE
		return gw
	}
	disabled := testGateway()
	disabled.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: false}
	all := []string{"spec.redis: Forbidden", "spec.config.documentation: Forbidden", "spec.openapi.enabled: Forbidden"}
	tests := []struct {
		name    string
		gw, old *v1alpha1.KrakenDGateway
		reject  []string // substrings of the denial; none means admitted
	}{
		{"created on CE", withFields(testGateway()), nil, all},
		{"created on EE", withFields(ee()), nil, nil},
		{"OpenAPI export disabled on CE", disabled, nil, nil},
		{"unchanged on CE", edited(withFields(testGateway())), withFields(testGateway()), nil},
		{"added on CE", withFields(testGateway()), testGateway(), all},
		{"EE to CE while set", withFields(testGateway()), withFields(ee()), all},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &GatewayValidator{Client: fakeClient(), Checker: &scriptedChecker{}}
			var old runtime.Object
			if tt.old != nil {
				old = tt.old
			}
			resp := review(t, v, "alice", tt.gw, old)
			if len(tt.reject) == 0 {
				if !resp.Allowed {
					t.Errorf("denied: %+v", resp.Result)
				}
				return
			}
			if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
				t.Fatalf("response = %+v, want a 422 denial", resp.Result)
			}
			for _, want := range tt.reject {
				if !strings.Contains(resp.Result.Message, want) {
					t.Errorf("denial %q does not contain %q", resp.Result.Message, want)
				}
			}
		})
	}
}
