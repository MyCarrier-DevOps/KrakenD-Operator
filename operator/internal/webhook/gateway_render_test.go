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
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

func TestGatewayAdmission_Render(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	ep := testEndpoint("ep", "/a")
	ok, fail := configcheck.Verdict{OK: true}, configcheck.Verdict{Output: "x"}
	epOK := configcheck.EndpointVerdict{OK: true}
	epFail := configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: "x"}
	tests := []struct {
		name             string
		old              *v1alpha1.KrakenDGateway
		objs             []client.Object
		verdicts         []configcheck.Verdict
		endpointVerdicts []configcheck.EndpointVerdict
		allowed          bool
		calls            string
		warns            string
	}{
		{"create checks the root, then the endpoints referencing it", nil, []client.Object{ep}, nil, nil, true,
			"root,group", ""},
		{"create whose waiting endpoints fail with it", nil, []client.Object{ep},
			[]configcheck.Verdict{ok, fail}, []configcheck.EndpointVerdict{epFail}, true, "root,group,endpoint", "default/ep"},
		{"create with a failing root", nil, nil, []configcheck.Verdict{fail}, nil, false, "root", ""},
		{"update keeps it passing", old, []client.Object{ep}, nil, nil, true, "root,group", ""},
		{"update breaks a served endpoint", old, []client.Object{ep},
			[]configcheck.Verdict{ok, fail}, []configcheck.EndpointVerdict{epFail, epOK}, false,
			"root,group,endpoint,endpoint", ""},
		{"update of a gateway whose endpoints already fail", old, []client.Object{ep},
			[]configcheck.Verdict{ok, fail}, []configcheck.EndpointVerdict{epFail, epFail}, true,
			"root,group,endpoint,endpoint", "already fail validation with the stored config"},
		{"update breaks the root", old, []client.Object{ep}, []configcheck.Verdict{fail, ok}, nil, false, "root,root", ""},
		{"update of a root that already fails", old, []client.Object{ep}, []configcheck.Verdict{fail, fail}, nil, true,
			"root,root", "already fails validation on its own"},
		{"update of a gateway whose endpoints already fail together", old, []client.Object{ep},
			[]configcheck.Verdict{ok, fail, fail}, []configcheck.EndpointVerdict{epOK}, true,
			"root,group,endpoint,group", "already fail validation together"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chk := &scriptedChecker{verdicts: tt.verdicts, endpointVerdicts: tt.endpointVerdicts}
			v := &GatewayValidator{Client: fakeClient(tt.objs...), Checker: chk}
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

// An edit that cannot change the rendered config is not checked: the checker
// being unavailable must not refuse an image rollback or a scale-up.
func TestGatewayAdmission_RenderNeutralEditSkipsTheCheck(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Replicas = ptr.To[int32](3)
	chk := &scriptedChecker{same: true, err: errors.New("waiting for a validation slot: context deadline exceeded")}

	resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", edited, old)

	if !resp.Allowed {
		t.Errorf("denied: %+v", resp.Result)
	}
	if len(chk.calls) != 0 {
		t.Errorf("checks = %v, want none for an edit that renders the same config", chk.calls)
	}
}

// A gateway edit whose renders cannot be compared is not judged: a transient 500.
func TestGatewayAdmission_RenderComparisonFailureIs500(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Replicas = ptr.To[int32](3)
	chk := &scriptedChecker{sameErr: errors.New("listing endpoints of gateway default/gw: cache not synced")}

	resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", edited, old)

	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
}

// A gateway write can break endpoints it does not own. A root failure is
// quoted on spec.config, because its writer owns the root; an endpoint the
// write breaks is named on spec, and nothing of it is quoted.
func TestGatewayAdmission_DenialQuotesOnlyTheRoot(t *testing.T) {
	t.Run("a root failure", func(t *testing.T) {
		chk := &scriptedChecker{verdicts: []configcheck.Verdict{{Output: "- at '/extra_config': bad"}}}

		resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", testGateway(), nil)

		if resp.Allowed || resp.Result.Details == nil || len(resp.Result.Details.Causes) != 1 {
			t.Fatalf("response = %+v, want a denial with one cause", resp.Result)
		}
		if c := resp.Result.Details.Causes[0]; c.Field != "spec.config" || !strings.Contains(c.Message, "- at '/extra_config': bad") {
			t.Errorf("cause = %+v, want spec.config quoting the root's output", c)
		}
	})
	t.Run("a broken endpoint", func(t *testing.T) {
		old := testGateway()
		edited := old.DeepCopy()
		edited.Spec.Config.Timeout = "5s"
		chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "SECRET"}},
			endpointVerdicts: []configcheck.EndpointVerdict{
				{Reason: v1alpha1.ReasonEndpointInvalid, Output: "SECRET"}, {OK: true}}}

		resp := review(t, &GatewayValidator{Client: fakeClient(testEndpoint("ep", "/a")), Checker: chk}, "alice", edited, old)

		if resp.Allowed || resp.Result.Details == nil || len(resp.Result.Details.Causes) != 1 {
			t.Fatalf("response = %+v, want a denial with one cause", resp.Result)
		}
		if c := resp.Result.Details.Causes[0]; c.Field != "spec" || !strings.Contains(c.Message, "default/ep") ||
			strings.Contains(responseText(resp), "SECRET") {
			t.Errorf("cause = %+v, want spec naming default/ep and quoting nothing of it", c)
		}
	})
}

// A root with hundreds of failures must not produce an unbounded denial.
func TestGatewayAdmission_DenialCausesAreBounded(t *testing.T) {
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{
		{Output: strings.Repeat("- at '/extra_config': "+strings.Repeat("x", warningLimit)+"\n", 1000)}}}

	resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: chk}, "alice", testGateway(), nil)

	if resp.Allowed || resp.Result.Details == nil {
		t.Fatalf("response = %+v, want a denial with causes", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || len(causes[0].Message) > 2*warningLimit {
		t.Errorf("%d causes, the first of %d bytes; want one cut near %d", len(causes), len(causes[0].Message), warningLimit)
	}
}

// A checker that cannot run, at any of the checks, is a transient 500: the
// request is not judged and clients retry.
func TestGatewayAdmission_ValidatorUnavailableIs500(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	fail := configcheck.Verdict{Output: "x"}
	tests := []struct {
		name     string
		old      *v1alpha1.KrakenDGateway
		verdicts []configcheck.Verdict
		failCall int
	}{
		{"create, the root", nil, nil, 1},
		{"update, the root", old, nil, 1},
		{"update, the stored root", old, []configcheck.Verdict{fail}, 2},
		{"update, the served endpoints", old, nil, 2},
		{"update, an endpoint on its own", old, []configcheck.Verdict{{OK: true}, fail}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chk := &scriptedChecker{verdicts: tt.verdicts, failCall: tt.failCall,
				endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}},
				err:              errors.New("waiting for a validation slot: context deadline exceeded")}
			obj := testGateway()
			var oldObj runtime.Object
			if tt.old != nil {
				obj, oldObj = edited, tt.old
			}

			resp := review(t, &GatewayValidator{Client: fakeClient(testEndpoint("ep", "/a")), Checker: chk}, "alice", obj, oldObj)

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
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}},
		endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}, {OK: true}}}

	review(t, &GatewayValidator{Client: fakeClient(testEndpoint("ep", "/a")), Checker: chk}, "alice", edited, old)

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
	bumped := testGateway()
	bumped.Spec.Version = "2.12"
	if resp := review(t, v, "alice", bumped, testGateway()); len(resp.Warnings) != 1 {
		t.Errorf("version changed to 2.12: warnings = %v, want the version warning", resp.Warnings)
	}
}

// The CRD does not bound spec.version, so the warning must not echo it whole.
func TestGatewayAdmission_VersionWarningIsBounded(t *testing.T) {
	long := testGateway()
	long.Spec.Version = strings.Repeat("é", 5*warningLimit)

	resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: &scriptedChecker{}}, "alice", long, nil)

	if len(resp.Warnings) != 1 {
		t.Fatalf("warnings = %v, want the version warning", resp.Warnings)
	}
	if w := resp.Warnings[0]; len(w) > 4*echoLimit || !utf8.ValidString(w) {
		t.Errorf("warning is %d bytes (valid UTF-8: %v), want it bounded", len(w), utf8.ValidString(w))
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
	generated := testEndpoint("generated", "/g")
	generated.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"documentation/openapi":{"summary":"s"}}`)}
	proxied := testEndpoint("proxied", "/x")
	proxied.Spec.Endpoints[0].Backends[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"backend/http/client":{"proxy_address":"http://q"}}`)}
	proxied.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "p"}
	redirects := testEndpoint("redirects", "/r")
	redirects.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "r"}
	honored := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			Raw: &runtime.RawExtension{Raw: []byte(`{"backend/http/client":{"send_body_on_redirect":true}}`)},
		},
	}
	wildcard := testEndpoint("wild", "/w/*")
	tests := []struct {
		name    string
		objs    []client.Object
		gw, old *v1alpha1.KrakenDGateway
		reject  []string // substrings of the denial; none means admitted
	}{
		{"CE root with an EE namespace", nil, withRoot(testGateway(), apiKeys), nil,
			[]string{`spec.config.extraConfig: Invalid value: "auth/api-keys"`}},
		{"CE root with CE namespaces", nil,
			withRoot(testGateway(), `{"security/cors":{"allow_origins":["*"]}}`), nil, nil},
		{"EE root with an EE namespace", nil, withRoot(ee(), apiKeys), nil, nil},
		{"unchanged CE root", nil, edited(withRoot(testGateway(), apiKeys)), withRoot(testGateway(), apiKeys), nil},
		{"CE root changed while it keeps an EE namespace", nil,
			withRoot(testGateway(), `{"auth/api-keys":{"keys":["x"]}}`), withRoot(testGateway(), apiKeys),
			[]string{`spec.config.extraConfig: Invalid value: "auth/api-keys"`}},
		{"EE to CE keeping an EE namespace in the root", nil, withRoot(testGateway(), apiKeys), withRoot(ee(), apiKeys),
			[]string{`spec.config.extraConfig: Invalid value: "auth/api-keys"`}},
		{"EE to CE with EE namespaces in use", []client.Object{keys, proxy}, testGateway(), ee(), []string{
			`spec.edition: Invalid value: "CE"`,
			"KrakenDEndpoint default/keys spec.endpoints[0].extraConfig auth/api-keys",
			"KrakenDBackendPolicy default/p spec.raw backend/http/client"}},
		{"CE create with EE namespaces in use", []client.Object{keys, proxy}, testGateway(), nil, []string{
			`spec.edition: Invalid value: "CE"`,
			"KrakenDEndpoint default/keys spec.endpoints[0].extraConfig auth/api-keys",
			"KrakenDBackendPolicy default/p spec.raw backend/http/client"}},
		{"EE to CE names the dropped keys of a partly honored block", []client.Object{proxied, proxy},
			testGateway(), ee(), []string{
				"KrakenDEndpoint default/proxied spec.endpoints[0].backends[0].extraConfig " +
					"backend/http/client: proxy_address",
				"KrakenDBackendPolicy default/p spec.raw backend/http/client: proxy_address"}},
		{"EE to CE with an entry's documentation/openapi", []client.Object{generated}, testGateway(), ee(), nil},
		{"EE to CE with nothing Enterprise-only", []client.Object{testEndpoint("plain", "/p")},
			testGateway(), ee(), nil},
		{"CE stays CE with stored EE namespaces", []client.Object{keys, proxy},
			edited(testGateway()), testGateway(), nil},
		{"EE to CE with a /prefix/* wildcard", []client.Object{wildcard}, testGateway(), ee(), []string{
			`spec.edition: Invalid value: "CE"`,
			"KrakenDEndpoint default/wild spec.endpoints[0].endpoint /w/*"}},
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
		gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
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
	dragonflyOff := testGateway()
	dragonflyOff.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: false}
	otherRedis := withFields(testGateway())
	otherRedis.Spec.Redis.ConnectionPool.Addresses = []string{"other:6379"}
	otherDocs := withFields(testGateway())
	otherDocs.Spec.Config.Documentation.Version = "2.0"
	all := []string{"spec.redis: Forbidden", "spec.config.documentation: Forbidden",
		"spec.openapi.enabled: Forbidden", "spec.dragonfly.enabled: Forbidden"}
	tests := []struct {
		name    string
		gw, old *v1alpha1.KrakenDGateway
		reject  []string // substrings of the denial; none means admitted
	}{
		{"created on CE", withFields(testGateway()), nil, all},
		{"created on EE", withFields(ee()), nil, nil},
		{"OpenAPI export disabled on CE", disabled, nil, nil},
		{"Dragonfly disabled on CE", dragonflyOff, nil, nil},
		{"unchanged on CE", edited(withFields(testGateway())), withFields(testGateway()), nil},
		{"added on CE", withFields(testGateway()), testGateway(), all},
		{"redis changed on CE", otherRedis, withFields(testGateway()), []string{"spec.redis: Forbidden"}},
		{"documentation changed on CE", otherDocs, withFields(testGateway()),
			[]string{"spec.config.documentation: Forbidden"}},
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

// Enabling spec.openapi on a CE gateway is rejected; its denial must
// not also carry the warning for a stored one.
func TestGatewayAdmission_RejectedOpenAPIOnCEDoesNotAlsoWarn(t *testing.T) {
	enabled := testGateway()
	enabled.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
	v := &GatewayValidator{Client: fakeClient(), Checker: &scriptedChecker{}}

	for name, old := range map[string]*v1alpha1.KrakenDGateway{"created": nil, "added": testGateway()} {
		t.Run(name, func(t *testing.T) {
			var oldObj runtime.Object
			if old != nil {
				oldObj = old
			}
			resp := review(t, v, "alice", enabled, oldObj)
			if resp.Allowed {
				t.Fatal("admitted, want a denial")
			}
			if len(resp.Warnings) != 0 {
				t.Errorf("warnings = %v, want none on a denial", resp.Warnings)
			}
		})
	}
}

// Enabling Dragonfly on a CE gateway is rejected; its denial must not also
// carry the warning for a stored one.
func TestGatewayAdmission_RejectedDragonflyOnCEDoesNotAlsoWarn(t *testing.T) {
	enabled := testGateway()
	enabled.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	v := &GatewayValidator{Client: fakeClient(), Checker: &scriptedChecker{}}

	for name, old := range map[string]*v1alpha1.KrakenDGateway{"created": nil, "added": testGateway()} {
		t.Run(name, func(t *testing.T) {
			var oldObj runtime.Object
			if old != nil {
				oldObj = old
			}
			resp := review(t, v, "alice", enabled, oldObj)
			if resp.Allowed {
				t.Fatal("admitted, want a denial")
			}
			if len(resp.Warnings) != 0 {
				t.Errorf("warnings = %v, want none on a denial", resp.Warnings)
			}
		})
	}
}

// A check that cannot run leaves the request unjudged: the 500 carries no
// warning about a failure that was already there.
func TestGatewayAdmission_UnavailableCheckCarriesNoWarning(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}}, failCall: 4,
		endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}},
		err:              errors.New("waiting for a validation slot: context deadline exceeded")}

	v := &GatewayValidator{Client: fakeClient(testEndpoint("ep", "/a")), Checker: chk}

	warnings, err := v.ValidateUpdate(context.Background(), old, edited)

	if err == nil {
		t.Fatal("admitted, want a 500")
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none with the error", warnings)
	}
}

// An endpoint that fails with the update is checked again with the stored
// gateway: a check handed the wrong gateway would compare the change with
// itself.
func TestGatewayAdmission_ChecksTheNewGatewayThenTheStoredOne(t *testing.T) {
	tests := []struct {
		name         string
		fromEdition  v1alpha1.Edition
		toEdition    v1alpha1.Edition
		wantGateways string
	}{
		{"same edition", v1alpha1.EditionCE, v1alpha1.EditionCE, "CE/5s,CE/5s,CE/5s,CE/3s"},
		{"Enterprise to Community", v1alpha1.EditionEE, v1alpha1.EditionCE, "CE/5s,CE/5s,CE/5s,EE/3s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := testGateway()
			old.Spec.Edition, old.Spec.Config.Timeout = tt.fromEdition, "3s"
			edited := old.DeepCopy()
			edited.Spec.Edition, edited.Spec.Config.Timeout = tt.toEdition, "5s"
			chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}},
				endpointVerdicts: []configcheck.EndpointVerdict{{Reason: v1alpha1.ReasonEndpointInvalid}, {OK: true}}}

			resp := review(t, &GatewayValidator{Client: fakeClient(testEndpoint("ep", "/a")), Checker: chk},
				"alice", edited, old)

			if resp.Allowed {
				t.Fatalf("response = %+v, want a denial", resp.Result)
			}
			if got := strings.Join(chk.gateways, ","); got != tt.wantGateways {
				t.Errorf("checks were handed gateways %s, want %s (root, group, endpoint with the update, "+
					"endpoint with the stored gateway)", got, tt.wantGateways)
			}
		})
	}
}

// With no spec.version the image tag is empty unless spec.image is set, so the
// warning says the version is unknown instead of quoting nothing.
func TestGatewayAdmission_EmptyVersionWarningSaysTheVersionIsEmpty(t *testing.T) {
	empty := testGateway()
	empty.Spec.Version = ""

	resp := review(t, &GatewayValidator{Client: fakeClient(), Checker: &scriptedChecker{}}, "alice", empty, nil)

	if len(resp.Warnings) != 1 {
		t.Fatalf("warnings = %v, want the version warning", resp.Warnings)
	}
	w := resp.Warnings[0]
	if !strings.HasPrefix(w, "spec.version is empty") || !strings.Contains(w, "spec.image") ||
		strings.Contains(w, "spec.version :") {
		t.Errorf("warning = %q, want it to say spec.version is empty and the image tag depends on spec.image", w)
	}
}

// Endpoints that fail with a new gateway are named in one warning, which
// stays bounded however many there are, and keeps its count of those not checked.
func TestGatewayAdmission_CreateWarningIsBounded(t *testing.T) {
	objs := []client.Object{}
	var endpointVerdicts []configcheck.EndpointVerdict
	for i := range 2 * maxEntryCauses {
		objs = append(objs, testEndpoint(fmt.Sprintf("endpoint-with-a-long-name-%02d", i), fmt.Sprintf("/e%d", i)))
		endpointVerdicts = append(endpointVerdicts, configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid})
	}
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}}, endpointVerdicts: endpointVerdicts}

	resp := review(t, &GatewayValidator{Client: fakeClient(objs...), Checker: chk}, "alice", testGateway(), nil)

	if !resp.Allowed || len(resp.Warnings) != 1 {
		t.Fatalf("response = %+v, warnings = %v, want admitted with one warning", resp.Result, resp.Warnings)
	}
	w := resp.Warnings[0]
	if len(w) > 2*warningLimit || !utf8.ValidString(w) {
		t.Errorf("warning is %d bytes (valid UTF-8: %v), want it bounded by the warning limit", len(w), utf8.ValidString(w))
	}
	if want := fmt.Sprintf("(+%d more not checked)", maxEntryCauses); !strings.HasSuffix(w, want) {
		t.Errorf("warning = %q, want it to end with the count %q", w, want)
	}
}

// However long the list of endpoints a gateway update breaks, the denial
// keeps its count of those not checked.
func TestGatewayAdmission_TheDenialKeepsItsCountOfEndpointsNotChecked(t *testing.T) {
	old := testGateway()
	edited := old.DeepCopy()
	edited.Spec.Config.Timeout = "5s"
	objs := []client.Object{}
	var endpointVerdicts []configcheck.EndpointVerdict
	for i := range maxEntryCauses + 5 {
		objs = append(objs, testEndpoint(fmt.Sprintf("carrier-integrations-orders-api-endpoint-%02d", i), fmt.Sprintf("/e%d", i)))
		endpointVerdicts = append(endpointVerdicts,
			configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid}, configcheck.EndpointVerdict{OK: true})
	}
	chk := &scriptedChecker{verdicts: []configcheck.Verdict{{OK: true}, {Output: "x"}}, endpointVerdicts: endpointVerdicts}

	resp := review(t, &GatewayValidator{Client: fakeClient(objs...), Checker: chk}, "alice", edited, old)

	if resp.Allowed || resp.Result.Details == nil || len(resp.Result.Details.Causes) != 1 {
		t.Fatalf("response = %+v, want a denial with one cause", resp.Result)
	}
	msg := resp.Result.Details.Causes[0].Message
	if want := "(+5 more not checked)"; !strings.HasSuffix(msg, want) || len(msg) > 2*warningLimit+len("Invalid value: ") {
		t.Errorf("cause = %q (%d bytes), want it to end with %q within the bound", msg, len(msg), want)
	}
}

// The check of the endpoints that already name a new gateway is advisory: when
// it cannot run, the create is admitted with a warning, not refused.
func TestGatewayAdmission_CreateAdmitsWhenTheEndpointCheckCannotRun(t *testing.T) {
	chk := &scriptedChecker{err: errors.New("no validation slot in time"), failCall: 2}

	resp := review(t, &GatewayValidator{Client: fakeClient(testEndpoint("ep", "/a")), Checker: chk}, "alice", testGateway(), nil)

	if !resp.Allowed || len(resp.Warnings) != 1 ||
		!strings.Contains(resp.Warnings[0], "could not check the endpoints that already reference this gateway") {
		t.Errorf("response = %+v, warnings = %v, want admitted with a could-not-check warning", resp.Result, resp.Warnings)
	}
}
