package controller

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

func TestEndpointAccepted_AnExcludedEndpointIsNotServedForItsOwnReason(t *testing.T) {
	gw := testGateway()
	bad := testEndpoint("bad", "/b")
	bad.Generation = 3
	tests := []struct {
		name    string
		verdict configcheck.EndpointVerdict
		says    string
	}{
		{"its own fault", configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid,
			Output: "- at '/endpoints/0/backend/0/host/0': bad host"}, "bad host"},
		{"a policy at fault", configcheck.EndpointVerdict{Reason: v1alpha1.ReasonPolicyInvalid,
			Policies: []types.NamespacedName{{Namespace: "default", Name: "p"}}, PoliciesFailAlone: true}, "default/p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The render also lists bad as conflicted: an endpoint that is both
			// excluded and conflicted serves nothing, so it keeps no conflicts.
			key := client.ObjectKeyFromObject(bad)
			rv := newRenderVerdicts(&renderer.RenderOutput{
				ConflictedEndpoints: []types.NamespacedName{key},
				EntryConflicts: map[types.NamespacedName][]renderer.EntryConflict{
					key: {{Endpoint: "/b", Method: "GET", Winner: types.NamespacedName{Namespace: "default", Name: "older"}}},
				},
			}, map[types.NamespacedName]configcheck.EndpointVerdict{key: tt.verdict})

			a := endpointAccepted(gw, bad, rv)

			c := a.condition
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tt.verdict.Reason || c.ObservedGeneration != 3 ||
				!strings.HasPrefix(c.Message, "Not served by gateway default/test-gw, which serves its other endpoints: ") ||
				!strings.Contains(c.Message, tt.says) {
				t.Errorf("Accepted = %+v, want False/%s at generation 3, not served by default/test-gw, saying %q",
					c, tt.verdict.Reason, tt.says)
			}
			if a.conflicts != nil || a.keepConflicts {
				t.Errorf("acceptance = %+v, want status.conflicts cleared: nothing of it is served", a)
			}
		})
	}
}

func TestExclusionCondition_AnUnappliedExclusionSaysWhenItTakesEffect(t *testing.T) {
	v := configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: "- at '/endpoints/0': bad"}

	c := exclusionCondition(testGateway(), testEndpoint("bad", "/b"), v, false)

	const want = "Will not be served when gateway default/test-gw next applies its config: " +
		"this endpoint fails krakend check on its own: - at '/endpoints/0': bad"
	if c.Message != want {
		t.Errorf("Message = %q, want %q", c.Message, want)
	}
}

// contentValidator judges a check from the config's text, in either mode. It
// rejects a config that contains one of markers, printing that marker's
// output, and, when together is set, a config that contains every one of
// together, as only a combined config can. failValidate rejects every full
// check. The lint run numbered unavailableAt (1-based) cannot run. lints and
// validates count the runs of each mode.
type contentValidator struct {
	markers          map[string]string
	together         []string
	failValidate     bool
	unavailableAt    int
	lints, validates int
}

func (v *contentValidator) Lint(_ context.Context, data []byte, _ v1alpha1.Edition) error {
	v.lints++
	if v.lints == v.unavailableAt {
		return errors.New("fork/exec /usr/local/bin/krakend: resource temporarily unavailable")
	}
	return v.judge(string(data))
}

func (v *contentValidator) Validate(_ context.Context, data []byte, _ v1alpha1.Edition) error {
	v.validates++
	if v.failValidate {
		return rejectedBy("ERROR testing the configuration file")
	}
	return v.judge(string(data))
}

func (v *contentValidator) judge(config string) error {
	if len(v.together) > 0 && !slices.ContainsFunc(v.together, func(m string) bool { return !strings.Contains(config, m) }) {
		return rejectedBy("TOGETHER-ONLY failure quoting " + strings.Join(v.together, " and "))
	}
	for marker, output := range v.markers {
		if strings.Contains(config, marker) {
			return rejectedBy(output)
		}
	}
	return nil
}

func TestGatewayReconcile_ARootThatFailsAloneBlamesNoEndpoint(t *testing.T) {
	gw := reconciledGateway()
	gw.Spec.Config.ExtraConfig = &runtime.RawExtension{Raw: []byte(`{"test/root-bad":{}}`)}
	good := testEndpoint("good", "/a")
	c := fakeClientBuilder().WithObjects(gw, good).WithStatusSubresource(gw, good).Build()
	val := &contentValidator{markers: map[string]string{
		"test/root-bad": "- at '/extra_config': additional properties 'test/root-bad' not allowed"}}
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	cv := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || cv.Reason != v1alpha1.ReasonGatewayRootInvalid || !strings.Contains(cv.Message, "test/root-bad") {
		t.Errorf("ConfigValid = %+v, want %s quoting the root's own output", cv, v1alpha1.ReasonGatewayRootInvalid)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(good)); cond != nil {
		t.Errorf("good Accepted = %+v, want none: a root failure judges no endpoint", cond)
	}
	if val.lints != 1 || val.validates != 0 {
		t.Errorf("ran %d lints and %d full checks, want the root alone", val.lints, val.validates)
	}
}

const badHostOutput = "- at '/endpoints/0/backend/0/host/0': http://invalid.test is not a valid host"

// badHosted is the test gateway's endpoint name at path, whose backend host
// the contentValidator below rejects.
func badHosted(name, path string) *v1alpha1.KrakenDEndpoint {
	ep := testEndpoint(name, path)
	ep.Spec.Endpoints[0].Backends[0].Host = []string{"http://invalid.test"}
	return ep
}

func rejectsBadHosts() *contentValidator {
	return &contentValidator{markers: map[string]string{"invalid.test": badHostOutput}}
}

func TestGatewayReconcile_AFirstRenderMarksAStaleAcceptedEndpointInvalid(t *testing.T) {
	gw := reconciledGateway() // recreated: no config has ever been applied
	bad := withAccepted(badHosted("bad", "/b"), metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	c := fakeClientBuilder().WithObjects(gw, bad).WithStatusSubresource(gw, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), rejectsBadHosts())

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var stored v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bad), &stored); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAccepted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonEndpointInvalid {
		t.Errorf("Accepted = %+v, want False/%s over the previous gateway's True", cond, v1alpha1.ReasonEndpointInvalid)
	}
	if status, _, _ := v1alpha1.EndpointReady(stored.Status.Conditions); status == metav1.ConditionTrue {
		t.Error("endpoint is Ready although it fails on its own")
	}
}

// appliedJSON is the config the stored gateway applies.
func appliedJSON(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway) string {
	t.Helper()
	stored := getGateway(t, c, gw)
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: gw.Namespace, Name: resources.ConfigMapName(stored, stored.Status.ConfigChecksum)}
	if err := c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("reading the applied config: %v", err)
	}
	return cm.Data[resources.ConfigKey]
}

func TestGatewayReconcile_AnEndpointThatFailsAloneIsExcludedAndTheRestApplied(t *testing.T) {
	gw := reconciledGateway()
	good, bad := testEndpoint("good", "/a"), badHosted("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), rejectsBadHosts())

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cv := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || cv.Status != metav1.ConditionTrue || cv.Reason != v1alpha1.ReasonConfigApplied {
		t.Errorf("ConfigValid = %+v, want True/%s: the rest of the gateway is applied", cv, v1alpha1.ReasonConfigApplied)
	}
	if applied := appliedJSON(t, c, gw); !strings.Contains(applied, `"/a"`) || strings.Contains(applied, `"/b"`) {
		t.Errorf("applied config:\n%s\nwant /a served and /b left out", applied)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(bad)); cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != v1alpha1.ReasonEndpointInvalid || !strings.Contains(cond.Message, "is not a valid host") {
		t.Errorf("bad Accepted = %+v, want False/%s quoting its own output", cond, v1alpha1.ReasonEndpointInvalid)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(good)); cond == nil || cond.Reason != v1alpha1.ReasonAccepted {
		t.Errorf("good Accepted = %+v, want Accepted", cond)
	}
}

func TestGatewayReconcile_ASafetyNetFailureAppliesNothingAndKeepsTheLiveConflicts(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	good, bad := testEndpoint("good", "/a"), badHosted("bad", "/b")
	fresh := []v1alpha1.EndpointConflict{{Endpoint: "/b", Method: "GET", Winner: "default/fresh"}}
	bad.Status.Conflicts = fresh
	staleList := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if eps, ok := list.(*v1alpha1.KrakenDEndpointList); ok {
				for i := range eps.Items {
					if eps.Items[i].Name == "bad" {
						eps.Items[i].Status.Conflicts = []v1alpha1.EndpointConflict{
							{Endpoint: "/b", Method: "GET", Winner: "default/stale"}}
					}
				}
			}
			return nil
		},
	}
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).
		WithInterceptorFuncs(staleList).Build()
	val := &contentValidator{markers: map[string]string{"invalid.test": badHostOutput}, failValidate: true}
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	stored := getGateway(t, c, gw)
	cv := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || cv.Reason != v1alpha1.ReasonCombinedConfigInvalid || stored.Status.ConfigChecksum != "applied" {
		t.Errorf("ConfigValid = %+v, checksum %s; want %s with the applied config kept",
			cv, stored.Status.ConfigChecksum, v1alpha1.ReasonCombinedConfigInvalid)
	}
	var got v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bad), &got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAccepted); cond == nil ||
		cond.Reason != v1alpha1.ReasonEndpointInvalid ||
		!strings.HasPrefix(cond.Message, "Will not be served when gateway default/test-gw next applies its config: ") {
		t.Errorf("bad Accepted = %+v, want %s worded for a config not yet applied", cond, v1alpha1.ReasonEndpointInvalid)
	}
	if !reflect.DeepEqual(got.Status.Conflicts, fresh) {
		t.Errorf("status.conflicts = %+v, want the live %+v kept", got.Status.Conflicts, fresh)
	}
}

// maskingEndpoints are two endpoints of the test gateway that share GET
// /same: older serves it, and newer's entry loses it, so the whole render
// leaves that entry out. newer's lost entry has the backend host the
// contentValidator rejects; its other entry, GET /e, is valid.
func maskingEndpoints() (older, newer *v1alpha1.KrakenDEndpoint) {
	older = testEndpoint("a", "/same")
	newer = badHosted("e", "/same")
	newer.Spec.Endpoints = append(newer.Spec.Endpoints, testEndpoint("e", "/e").Spec.Endpoints[0])
	return older, newer
}

func TestGatewayReconcile_AMaskedEndpointIsJudgedOnItsOwn(t *testing.T) {
	gw := reconciledGateway()
	older, newer := maskingEndpoints()
	c := fakeClientBuilder().WithObjects(gw, older, newer).WithStatusSubresource(gw, older, newer).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), rejectsBadHosts())

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(newer)); cond == nil ||
		cond.Reason != v1alpha1.ReasonEndpointInvalid || !strings.Contains(cond.Message, "is not a valid host") {
		t.Errorf("e Accepted = %+v, want %s: its lost entry fails on its own", cond, v1alpha1.ReasonEndpointInvalid)
	}
	if applied := appliedJSON(t, c, gw); strings.Contains(applied, `"/e"`) {
		t.Errorf("applied config:\n%s\nwant e left out whole", applied)
	}
}

func TestGatewayReconcile_AMaskedEndpointIsJudgedOnTheFastPath(t *testing.T) {
	gw := reconciledGateway()
	older, newer := maskingEndpoints()
	c := fakeClientBuilder().WithObjects(gw, older, newer).WithStatusSubresource(gw, older, newer).Build()
	// An earlier validator passed e's lost entry, so the whole render was
	// applied with e served in part.
	if err := reconcileGateway(t, newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&contentValidator{}), gw); err != nil {
		t.Fatal(err)
	}
	applied := getGateway(t, c, gw).Status.ConfigChecksum

	// A new process with a validator that rejects it: the render is still the
	// applied config.
	if err := reconcileGateway(t, newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		rejectsBadHosts()), gw); err != nil {
		t.Fatal(err)
	}

	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(newer)); cond == nil ||
		cond.Reason != v1alpha1.ReasonEndpointInvalid {
		t.Errorf("e Accepted = %+v, want %s although the render is the applied config", cond, v1alpha1.ReasonEndpointInvalid)
	}
	if got := getGateway(t, c, gw).Status.ConfigChecksum; got == applied {
		t.Errorf("checksum %s unchanged, want the config without e applied", got)
	}
}

func TestGatewayReconcile_CountsOnlyTheRejectionsOfObjectsOnTheirOwn(t *testing.T) {
	gw := reconciledGateway()
	good, bad := testEndpoint("good", "/a"), badHosted("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), rejectsBadHosts())
	before := testutil.ToFloat64(configValidationFailures)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	// bad's own check counts; the whole render's rejection, which bad
	// causes, does not.
	if got := testutil.ToFloat64(configValidationFailures) - before; got != 1 {
		t.Errorf("counted %v rejections, want 1", got)
	}
}

// dragonflyGateway is the test gateway with Dragonfly enabled, and a client
// whose cluster has the Dragonfly CRD but no Dragonfly yet, so the
// controller renders the Dragonfly address it will serve.
func dragonflyGateway(objs ...client.Object) (*v1alpha1.KrakenDGateway, client.Client) {
	gw := reconciledGateway()
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(dragonflyGVK)).
		WithObjects(append([]client.Object{gw}, objs...)...).WithStatusSubresource(append([]client.Object{gw}, objs...)...).Build()
	return gw, c
}

func TestGatewayReconcile_TheRootIsCheckedWithTheDetectedDragonfly(t *testing.T) {
	gw, c := dragonflyGateway(testEndpoint("good", "/a"))
	dns := resources.DragonflyServiceDNS(gw)
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&contentValidator{markers: map[string]string{dns: "- at '/extra_config/redis': " + dns + " refused"}})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if cv := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid); cv == nil ||
		cv.Reason != v1alpha1.ReasonGatewayRootInvalid || !strings.Contains(cv.Message, dns) {
		t.Errorf("ConfigValid = %+v, want %s quoting the Dragonfly address", cv, v1alpha1.ReasonGatewayRootInvalid)
	}
}

func TestGatewayReconcile_EachEndpointIsCheckedWithTheDetectedDragonfly(t *testing.T) {
	gw, c := dragonflyGateway(testEndpoint("good", "/a"), testEndpoint("e", "/e"))
	dns := resources.DragonflyServiceDNS(gw)
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&contentValidator{together: []string{dns, `"/e"`}})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if cond := storedAccepted(t, c, types.NamespacedName{Namespace: gw.Namespace, Name: "e"}); cond == nil ||
		cond.Reason != v1alpha1.ReasonEndpointInvalid {
		t.Errorf("e Accepted = %+v, want %s: it fails with the Dragonfly address the gateway serves",
			cond, v1alpha1.ReasonEndpointInvalid)
	}
}

func TestGatewayReconcile_ASteadyPassWithAnExcludedEndpointRunsNoCheck(t *testing.T) {
	gw := reconciledGateway()
	good, bad := testEndpoint("good", "/a"), badHosted("bad", "/b")
	writes := 0
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDEndpoint](&writes)).Build()
	val := rejectsBadHosts()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	lints, validates, written := val.lints, val.validates, writes

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if val.lints != lints || val.validates != validates || writes != written {
		t.Errorf("the steady pass ran %d lints, %d full checks and %d endpoint writes, want none",
			val.lints-lints, val.validates-validates, writes-written)
	}
}

func TestGatewayReconcile_AFailureOnlyTogetherIsTheGatewaysAndQuotesNothing(t *testing.T) {
	gw := reconciledGateway()
	a, b := testEndpoint("a", "/a"), testEndpoint("b", "/b")
	c := fakeClientBuilder().WithObjects(gw, a, b).WithStatusSubresource(gw, a, b).Build()
	val := &contentValidator{together: []string{`"/a"`, `"/b"`}}
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	cv := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || cv.Reason != v1alpha1.ReasonCombinedConfigInvalid || strings.Contains(cv.Message, "TOGETHER-ONLY") {
		t.Errorf("ConfigValid = %+v, want %s quoting no output", cv, v1alpha1.ReasonCombinedConfigInvalid)
	}
	for _, ep := range []*v1alpha1.KrakenDEndpoint{a, b} {
		if cond := storedAccepted(t, c, client.ObjectKeyFromObject(ep)); cond != nil {
			t.Errorf("%s Accepted = %+v, want none: no endpoint fails on its own", ep.Name, cond)
		}
	}
}

func TestGatewayReconcile_AnOutageMidwayKeepsTheFinishedVerdicts(t *testing.T) {
	gw := reconciledGateway()
	a, b, bad := testEndpoint("a", "/a"), testEndpoint("b", "/b"), badHosted("c", "/c")
	c := fakeClientBuilder().WithObjects(gw, a, b, bad).WithStatusSubresource(gw, a, b, bad).Build()
	val := rejectsBadHosts()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val)
	if err := reconcileGateway(t, r, gw); err != nil { // lints: the root, a, b, c
		t.Fatal(err)
	}
	// a changes, so its check is the first to run again, and it cannot run.
	var changed v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(a), &changed); err != nil {
		t.Fatal(err)
	}
	changed.Spec.Endpoints[0].Backends[0].URLPattern = "/changed"
	changed.Generation++
	if err := c.Update(context.Background(), &changed); err != nil {
		t.Fatal(err)
	}
	val.unavailableAt = val.lints + 1

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("reconcile succeeded although a check could not run")
	}
	cv := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || cv.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Errorf("ConfigValid = %+v, want %s", cv, v1alpha1.ReasonValidatorUnavailable)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(bad)); cond == nil || cond.Reason != v1alpha1.ReasonEndpointInvalid {
		t.Errorf("c Accepted = %+v, want its exclusion kept on a pass a check could not finish", cond)
	}

	val.unavailableAt = 0
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	// The first pass checks the root, a, b and c. The failed pass checks a,
	// which cannot run. The last pass checks a again; b and c were judged
	// before the outage and are not run again.
	if val.lints != 6 {
		t.Errorf("ran %d lints in all, want 6: the verdicts finished before the outage are kept", val.lints)
	}
}

func TestGatewayReconcile_ARestartedOperatorKeepsTheAppliedConfig(t *testing.T) {
	gw := reconciledGateway()
	good, bad := testEndpoint("good", "/a"), badHosted("bad", "/b")
	gatewayWrites := 0
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDGateway](&gatewayWrites)).Build()
	if err := reconcileGateway(t, newTestGatewayReconciler(c, renderer.New(renderer.Options{}), rejectsBadHosts()), gw); err != nil {
		t.Fatal(err)
	}
	applied, written := getGateway(t, c, gw).Status.ConfigChecksum, gatewayWrites
	var before corev1.ConfigMapList
	if err := c.List(context.Background(), &before, client.InNamespace(gw.Namespace)); err != nil {
		t.Fatal(err)
	}

	val := rejectsBadHosts() // a new process: a cold memo
	if err := reconcileGateway(t, newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val), gw); err != nil {
		t.Fatal(err)
	}

	var after corev1.ConfigMapList
	if err := c.List(context.Background(), &after, client.InNamespace(gw.Namespace)); err != nil {
		t.Fatal(err)
	}
	if got := getGateway(t, c, gw).Status.ConfigChecksum; got != applied || gatewayWrites != written ||
		len(after.Items) != len(before.Items) {
		t.Errorf("after a restart: checksum %s (was %s), %d new gateway status writes, %d ConfigMaps (were %d); want nothing changed",
			got, applied, gatewayWrites-written, len(after.Items), len(before.Items))
	}
	if val.lints != 3 || val.validates != 1 {
		t.Errorf("the cold pass ran %d lints and %d full checks, want 3 and 1: the render without bad is the applied one",
			val.lints, val.validates)
	}
}

func TestGatewayReconcile_AFixedEndpointIsServedAgain(t *testing.T) {
	gw := reconciledGateway()
	good, bad := testEndpoint("good", "/a"), badHosted("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), rejectsBadHosts())
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	var stored v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bad), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Endpoints[0].Backends[0].Host = []string{"http://svc:8080"}
	stored.Generation++
	if err := c.Update(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if applied := appliedJSON(t, c, gw); !strings.Contains(applied, `"/b"`) {
		t.Errorf("applied config:\n%s\nwant the fixed /b served", applied)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(bad)); cond == nil || cond.Reason != v1alpha1.ReasonAccepted {
		t.Errorf("bad Accepted = %+v, want Accepted once fixed", cond)
	}
}

func TestGatewayReconcile_AnEndpointThatPassesAgainLosesItsExclusionOnAFailedPass(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	fixed := withAccepted(testEndpoint("fixed", "/a"), metav1.ConditionFalse, v1alpha1.ReasonEndpointInvalid)
	c := fakeClientBuilder().WithObjects(gw, fixed).WithStatusSubresource(gw, fixed).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &contentValidator{failValidate: true})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(fixed)); cond != nil {
		t.Errorf("Accepted = %+v, want the stale exclusion lifted: the endpoint passes on its own", cond)
	}
}
