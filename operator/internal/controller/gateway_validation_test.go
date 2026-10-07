package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
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
			rv := newRenderVerdicts(&renderer.RenderOutput{},
				map[types.NamespacedName]configcheck.EndpointVerdict{client.ObjectKeyFromObject(bad): tt.verdict})

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
