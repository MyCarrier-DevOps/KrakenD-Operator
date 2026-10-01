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

package controller

import (
	"context"
	"fmt"
	"io/fs"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// countingValidator returns err from every Validate call and counts the
// calls, i.e. how often krakend check would have run. Its validation copy
// depends on the edition mode the way the real one does for a config with
// an EE-only wildcard endpoint.
type countingValidator struct {
	calls int
	err   error
}

func (v *countingValidator) Validate(context.Context, []byte) error {
	v.calls++
	return v.err
}

func (v *countingValidator) PrepareValidationCopy(jsonData []byte, eeWithoutFallback bool) ([]byte, error) {
	if eeWithoutFallback {
		return append([]byte("ee-validation-copy:"), jsonData...), nil
	}
	return jsonData, nil
}

// rejectedBy returns a validation error shaped like a krakend check
// rejection.
func rejectedBy(output string) *renderer.ValidationError {
	return &renderer.ValidationError{Output: output, Err: fmt.Errorf("exit status 1")}
}

// gatewayStatusWrites builds a fake client seeded with objs that records the
// phase of every gateway status update it receives.
func gatewayStatusWrites(objs ...client.Object) (client.Client, *[]v1alpha1.GatewayPhase) {
	var phases []v1alpha1.GatewayPhase
	c := fakeClientBuilder().
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.KrakenDGateway{}, &v1alpha1.KrakenDEndpoint{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context, c client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if gw, ok := obj.(*v1alpha1.KrakenDGateway); ok && sub == "status" {
					phases = append(phases, gw.Status.Phase)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	return c, &phases
}

func reconcileGateway(t *testing.T, r *KrakenDGatewayReconciler, gw *v1alpha1.KrakenDGateway) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})
	return err
}

func getGateway(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway) *v1alpha1.KrakenDGateway {
	t.Helper()
	var got v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

// renderOutput returns a renderer that always renders the same config with
// the given checksum.
func renderOutput(checksum string) *mockRenderer {
	return &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3,"checksum":"` + checksum + `"}`), Checksum: checksum, DesiredImage: "img:v1",
	}}
}

func TestGatewayReconcile_SteadyStateWritesNoStatus(t *testing.T) {
	gw := testGateway()
	c, phases := gatewayStatusWrites(gw)
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: renderOutput("good"), Validator: &mockValidator{},
	}

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	first := len(*phases)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if extra := (*phases)[first:]; len(extra) != 0 {
		t.Errorf("a reconcile with nothing to change wrote status %d times: %v", len(extra), extra)
	}
	for _, p := range *phases {
		if p == v1alpha1.PhasePending || p == v1alpha1.PhaseRendering || p == v1alpha1.PhaseValidating {
			t.Errorf("transient phase %q was written", p)
		}
	}
}

func TestGatewayReconcile_RejectedRenderIsNotRevalidated(t *testing.T) {
	gw := testGateway()
	c, phases := gatewayStatusWrites(gw)
	recorder := fakeRecorder()
	validator := &countingValidator{err: rejectedBy("bad endpoint")}
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: recorder,
		Renderer: renderOutput("bad"), Validator: validator,
	}

	for i := range 3 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	if validator.calls != 1 {
		t.Errorf("krakend check ran %d times for one unchanged render, want 1", validator.calls)
	}
	if len(*phases) != 1 || (*phases)[0] != v1alpha1.PhaseError {
		t.Errorf("status writes = %v, want exactly one, with phase Error", *phases)
	}
	if n := len(recorder.Events); n != 1 {
		t.Errorf("got %d events, want one ConfigValidationFailed", n)
	}
}

func TestGatewayReconcile_EditionModeFlipRevalidatesSameRender(t *testing.T) {
	gw := testGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	c, _ := gatewayStatusWrites(gw)
	validator := &countingValidator{err: rejectedBy("wildcards must be named")}
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: renderOutput("same"), Validator: validator,
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	// The license monitor switches the gateway to CE fallback: the render
	// is unchanged, but the validation input is not.
	degraded := getGateway(t, c, gw)
	meta.SetStatusCondition(&degraded.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionLicenseDegraded, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ReasonLicenseFallbackCE, Message: "license expired",
	})
	if err := c.Status().Update(context.Background(), degraded); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if validator.calls != 2 {
		t.Errorf("krakend check ran %d times, want 2: CE fallback changes what is validated", validator.calls)
	}
}

func TestGatewayReconcile_RememberedRejectionRestoresOverwrittenStatus(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw)
	validator := &countingValidator{err: rejectedBy("bad endpoint")}
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: renderOutput("bad"), Validator: validator,
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	// Another writer replaces the conditions with a stale copy.
	clobbered := getGateway(t, c, gw)
	meta.SetStatusCondition(&clobbered.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionConfigValid, Status: metav1.ConditionTrue,
		Reason: "ConfigValid", Message: "Configuration passed validation",
	})
	clobbered.Status.Phase = v1alpha1.PhaseRunning
	if err := c.Status().Update(context.Background(), clobbered); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionConfigValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || got.Status.Phase != v1alpha1.PhaseError {
		t.Errorf("ConfigValid = %+v, phase = %s; want the remembered rejection restored", cond, got.Status.Phase)
	}
	if validator.calls != 1 {
		t.Errorf("krakend check ran %d times, want 1", validator.calls)
	}
}

func TestGatewayReconcile_ValidatorUnavailableIsRetried(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhaseRunning
	gw.Status.ConfigChecksum = "applied"
	c, writes := gatewayStatusWrites(gw)
	validator := &countingValidator{err: fmt.Errorf("running krakend check: %w", fs.ErrNotExist)}
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: renderOutput("new"), Validator: validator,
	}

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("expected an error, so the reconcile is retried with backoff")
	}

	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionConfigValid)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Errorf("ConfigValid = %+v, want Unknown/ValidatorUnavailable", cond)
	}
	if got.Status.Phase != v1alpha1.PhaseRunning || got.Status.ConfigChecksum != "applied" {
		t.Errorf("phase = %s, checksum = %s; an unavailable validator must not change either",
			got.Status.Phase, got.Status.ConfigChecksum)
	}
	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("expected the retry to fail the same way")
	}
	if validator.calls != 2 {
		t.Errorf("krakend check ran %d times over two reconciles, want 2: nothing is remembered", validator.calls)
	}
	if len(*writes) != 1 {
		t.Errorf("%d status writes over two reconciles with the same cause, want 1: "+
			"each write re-enqueues the gateway ahead of the backoff", len(*writes))
	}
}
