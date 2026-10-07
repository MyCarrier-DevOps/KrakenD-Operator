package webhook

import (
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// clashWithAutoOptions answers Conflicts with a clash between default/b and
// default/c only for a gateway that turns router.auto_options on.
func clashWithAutoOptions(gw *v1alpha1.KrakenDGateway, _ []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
	if gw.Spec.Config.Router == nil || !gw.Spec.Config.Router.AutoOptions {
		return configcheck.RouteConflicts{}
	}
	return configcheck.RouteConflicts{Lost: map[types.NamespacedName][]renderer.EntryConflict{
		{Namespace: "default", Name: "c"}: {{Endpoint: "/a/{name}", Method: "POST",
			Winner: types.NamespacedName{Namespace: "default", Name: "b"},
			Detail: "OPTIONS /a/:name clashes with OPTIONS /a/:id: wildcard conflict"}},
	}}
}

func TestGatewayAdmission_RefusesARootThatMakesEndpointsClash(t *testing.T) {
	chk := &scriptedChecker{conflicts: clashWithAutoOptions}
	old := testGateway()
	gw := routerOf(testGateway(), v1alpha1.RouterConfig{AutoOptions: true})

	resp := review(t, &GatewayValidator{Client: fakeClient(old), Checker: chk}, "alice", gw, old)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	if text := responseText(resp); !strings.Contains(text, "default/c") || !strings.Contains(text, "default/b") {
		t.Errorf("denial %q must name both endpoints", text)
	}
	if len(chk.calls) != 0 {
		t.Errorf("checks = %v, want none after a structural refusal", chk.calls)
	}
}

func TestGatewayAdmission_AClashTheStoredRootAlreadyHasIsNotTheChanges(t *testing.T) {
	always := func(*v1alpha1.KrakenDGateway, []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		return clashWithAutoOptions(routerOf(testGateway(), v1alpha1.RouterConfig{AutoOptions: true}), nil)
	}
	chk := &scriptedChecker{conflicts: always}
	old := routerOf(testGateway(), v1alpha1.RouterConfig{AutoOptions: true})
	gw := routerOf(testGateway(), v1alpha1.RouterConfig{AutoOptions: true, HealthPath: "/status"})

	resp := review(t, &GatewayValidator{Client: fakeClient(old), Checker: chk}, "alice", gw, old)

	if !resp.Allowed {
		t.Errorf("an edit that keeps a stored clash was denied: %+v", resp.Result)
	}
}

// TestGatewayAdmission_RefusesARootWhileClashResolutionIsCapped caps only the
// render of the changed gateway: the stored one resolves in full, so the
// refusal can only come from the render of the change.
func TestGatewayAdmission_RefusesARootWhileClashResolutionIsCapped(t *testing.T) {
	chk := &scriptedChecker{conflicts: func(gw *v1alpha1.KrakenDGateway, _ []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		return configcheck.RouteConflicts{Capped: gw.Spec.Config.Router != nil && gw.Spec.Config.Router.HealthPath == "/status"}
	}}
	old := testGateway()
	gw := routerOf(testGateway(), v1alpha1.RouterConfig{HealthPath: "/status"})

	resp := review(t, &GatewayValidator{Client: fakeClient(old), Checker: chk}, "alice", gw, old)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(responseText(resp), configcheck.ClashesCapped) {
		t.Errorf("response = %+v, want a 422 denial saying the clashes cannot be told apart", resp.Result)
	}
	if len(chk.calls) != 0 {
		t.Errorf("checks = %v, want none after a structural refusal", chk.calls)
	}
}
