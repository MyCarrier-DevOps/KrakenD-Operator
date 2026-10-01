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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	gw.Status.ActiveImage = "img:v1"
	now := metav1.Now()
	gw.Status.Conditions = []metav1.Condition{
		{Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentAvailable",
			Message: "All replicas are available", LastTransitionTime: now},
		{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "RolloutComplete",
			Message: "Deployment rollout completed successfully", LastTransitionTime: now},
	}
	c, writes := gatewayStatusWrites(gw, makeConvergedDeployment(gw, "applied"))
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
	if ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady); ready == nil ||
		ready.Status != metav1.ConditionUnknown || ready.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Errorf("Ready = %+v, want Unknown/ValidatorUnavailable", ready)
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

func TestGatewayReconcile_ValidationMessageIsBounded(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw)
	recorder := fakeRecorder()
	line := "ERROR at '/endpoints/0/backend/0/extra_config/qos~1circuit-breaker/interval': got string, want integer"
	huge := strings.Repeat(line+"\n", 1000)
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: recorder,
		Renderer: renderOutput("bad"), Validator: &countingValidator{err: rejectedBy(huge)},
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid)
	if cond == nil {
		t.Fatal("ConfigValid not set")
	}
	if len(cond.Message) > 4096 {
		t.Errorf("condition message is %d bytes, want at most 4096", len(cond.Message))
	}
	if !strings.Contains(cond.Message, line) || !strings.HasSuffix(cond.Message, "more lines)") {
		t.Errorf("message should keep the first lines and count the rest, got %d bytes ending %q",
			len(cond.Message), cond.Message[max(0, len(cond.Message)-80):])
	}
	event := <-recorder.Events
	if len(event) > 4096+len("Warning ConfigValidationFailed ") {
		t.Errorf("event message is %d bytes, want the same bound as the condition", len(event))
	}
}

func TestGatewayReconcile_OversizedRejectionWarnsOnce(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw)
	recorder := fakeRecorder()
	huge := strings.Repeat("ERROR at '/endpoints/0': additional properties not allowed\n", 1000)
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: recorder,
		Renderer: renderOutput("bad"), Validator: &countingValidator{err: rejectedBy(huge)},
	}
	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(recorder.Events); got != 1 {
		t.Errorf("got %d events for one oversized rejection reconciled twice, want 1", got)
	}
}

// setGatewaySeries gives the gateway a series in every per-gateway metric.
func setGatewaySeries(namespace, name string) {
	endpointsPerGateway.WithLabelValues(namespace, name).Set(4)
	gatewayInfo.WithLabelValues(namespace, name, "CE", "2.7.0").Set(1)
	dragonflyReady.WithLabelValues(namespace, name).Set(1)
	licenseExpirySeconds.WithLabelValues(namespace, name).Set(100)
	reconcileDuration.WithLabelValues("gateway", namespace, name).Observe(0.1)
}

// remainingGatewaySeries deletes and counts the gateway's leftover series.
func remainingGatewaySeries(namespace, name string) int {
	gateway := prometheus.Labels{"namespace": namespace, "name": name}
	n := endpointsPerGateway.DeletePartialMatch(gateway) +
		gatewayInfo.DeletePartialMatch(gateway) +
		dragonflyReady.DeletePartialMatch(gateway) +
		licenseExpirySeconds.DeletePartialMatch(gateway)
	return n + reconcileDuration.DeletePartialMatch(prometheus.Labels{
		"controller": "gateway", "namespace": namespace, "name": name,
	})
}

func TestGatewayReconcile_TerminatingGatewayIsLeftAlone(t *testing.T) {
	gw := testGateway()
	gw.Namespace = "terminating"
	gw.Finalizers = []string{"test.krakend.io/hold"}
	gw.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	c, phases := gatewayStatusWrites(gw)
	setGatewaySeries(gw.Namespace, gw.Name)
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer:  &mockRenderer{err: fmt.Errorf("a terminating gateway must not be rendered")},
		Validator: &mockValidator{},
	}

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); !apierrors.IsNotFound(err) {
		t.Errorf("Deployment exists for a terminating gateway (get err = %v)", err)
	}
	if len(*phases) != 0 {
		t.Errorf("status written for a terminating gateway: %v", *phases)
	}
	if n := remainingGatewaySeries(gw.Namespace, gw.Name); n != 0 {
		t.Errorf("%d metric series left for a terminating gateway, want 0", n)
	}
}

func TestGatewayReconcile_DeletedGatewayDropsItsMetrics(t *testing.T) {
	setGatewaySeries("deleted", "gone-gw")
	r := &KrakenDGatewayReconciler{
		Client: fakeClientBuilder().Build(), Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: &mockRenderer{}, Validator: &mockValidator{},
	}
	gone := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gone-gw", Namespace: "deleted"}}

	if err := reconcileGateway(t, r, gone); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := remainingGatewaySeries("deleted", "gone-gw"); n != 0 {
		t.Errorf("%d metric series left for a deleted gateway, want 0", n)
	}
}

func TestGatewayReconcile_RevertAfterUnavailableValidatorClearsRetrying(t *testing.T) {
	gw := testGateway()
	gw.Status.ConfigChecksum = "applied"
	c, _ := gatewayStatusWrites(gw)
	validator := &countingValidator{err: fmt.Errorf("running krakend check: %w", fs.ErrNotExist)}
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: renderOutput("new"), Validator: validator,
	}
	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("expected an error, so the reconcile is retried with backoff")
	}
	if cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid); cond == nil ||
		cond.Status != metav1.ConditionUnknown || cond.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Fatalf("ConfigValid = %+v, want Unknown/ValidatorUnavailable before the revert", cond)
	}

	// The input is reverted: the render equals the applied configuration again.
	r.Renderer = renderOutput("applied")
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionConfigValid)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonConfigApplied {
		t.Errorf("ConfigValid = %+v, want True/ConfigApplied once the render is the applied config again", cond)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady); ready == nil ||
		ready.Reason == v1alpha1.ReasonValidatorUnavailable {
		t.Errorf("Ready = %+v, want it no longer ValidatorUnavailable after the revert", ready)
	}
}
