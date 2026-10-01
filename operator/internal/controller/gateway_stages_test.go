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
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// testNow is the instant the gateway tests' clock reads.
var testNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// newTestGatewayReconciler wires a gateway reconciler with the fakes the
// gateway tests need. A new collaborator is added here, once.
func newTestGatewayReconciler(
	c client.Client, rend renderer.Renderer, val renderer.Validator,
) *KrakenDGatewayReconciler {
	return &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  rend,
		Validator: val,
		Clock:     clocktesting.NewFakeClock(testNow),
	}
}

// servingGateway returns a gateway whose applied config is checksum and whose
// rollout has completed: ConfigValid, Available and Progressing=False.
func servingGateway(checksum, image string) *v1alpha1.KrakenDGateway {
	gw := reconciledGateway()
	gw.Status.ConfigChecksum = checksum
	gw.Status.ActiveImage = image
	for _, cond := range []metav1.Condition{
		{Type: v1alpha1.ConditionConfigValid, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConfigApplied},
		{Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: "DeploymentAvailable"},
		{Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionFalse, Reason: "RolloutComplete"},
	} {
		meta.SetStatusCondition(&gw.Status.Conditions, cond)
	}
	return gw
}

// staleDeploymentReads makes every read of the gateway Deployment return
// stale, as an informer cache does until it has seen the controller's own
// update.
func staleDeploymentReads(stale *appsv1.Deployment) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if dep, ok := obj.(*appsv1.Deployment); ok {
				stale.DeepCopyInto(dep)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// rollingGateway is servingGateway with a config rollout still reported as
// in progress.
func rollingGateway(checksum, image string) *v1alpha1.KrakenDGateway {
	gw := servingGateway(checksum, image)
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConfigDeployed,
	})
	return gw
}

// reconcileWithUnavailableValidator reconciles gw against a validator that
// cannot run, and requires the reconcile to fail so it is retried.
func reconcileWithUnavailableValidator(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway) {
	t.Helper()
	r := newTestGatewayReconciler(c, renderOutput("new"),
		&countingValidator{err: errors.New("fork/exec /usr/local/bin/krakend: no such file or directory")})
	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("an unavailable validator must fail the reconcile so it is retried")
	}
}

func TestGatewayReconcile_InfrastructureRunsWhateverTheConfigVerdict(t *testing.T) {
	cases := []struct {
		name      string
		verdict   error
		wantError bool
	}{
		{name: "rejected config", verdict: rejectedBy("- at '/endpoints/0/endpoint': bad")},
		{name: "validator unavailable", verdict: errors.New("fork/exec krakend: no such file or directory"), wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.Status.ConfigChecksum = "applied"
			c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOutput("new"), &countingValidator{err: tc.verdict})

			err := reconcileGateway(t, r, gw)
			if (err != nil) != tc.wantError {
				t.Fatalf("reconcile error = %v, want an error: %v", err, tc.wantError)
			}
			var dep appsv1.Deployment
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
				t.Fatalf("the Deployment must be reconciled whatever the config verdict: %v", err)
			}
			if got := dep.Spec.Template.Annotations[resources.PostRestartJobChecksumAnnotation]; got != "applied" {
				t.Errorf("the Deployment must carry the applied config %q, got %q", "applied", got)
			}
			var svc corev1.Service
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &svc); err != nil {
				t.Errorf("the Service must be reconciled whatever the config verdict: %v", err)
			}
		})
	}
}

func TestGatewayReconcile_NoDeploymentBeforeAnyConfigPasses(t *testing.T) {
	gw := reconciledGateway()
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("new"),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &svc); err != nil {
		t.Fatalf("the Service must be reconciled before any config passes: %v", err)
	}
	var dep appsv1.Deployment
	err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("no Deployment may exist before any config passes validation; Get returned %v", err)
	}
}

func TestGatewayReconcile_UnavailableValidatorIsNotReady(t *testing.T) {
	gw := servingGateway("applied", "img:v1")
	c := fakeClientBuilder().WithObjects(gw, makeConvergedDeployment(gw, "applied")).
		WithStatusSubresource(gw).Build()
	reconcileWithUnavailableValidator(t, c, gw)
	ready := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionUnknown || ready.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Errorf("Ready = %+v, want Unknown/%s: the newest render has not been judged",
			ready, v1alpha1.ReasonValidatorUnavailable)
	}
}

func TestGatewayReconcile_UnavailableValidatorStillRefreshesRolloutConditions(t *testing.T) {
	gw := rollingGateway("applied", "img:v1")
	c := fakeClientBuilder().WithObjects(gw, makeConvergedDeployment(gw, "applied")).
		WithStatusSubresource(gw).Build()
	reconcileWithUnavailableValidator(t, c, gw)
	got := getGateway(t, c, gw)
	if got.Status.Phase != v1alpha1.PhaseRunning {
		t.Errorf("phase = %s, want %s: the converged rollout must be recorded while the validator is down",
			got.Status.Phase, v1alpha1.PhaseRunning)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionUnknown || ready.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Errorf("Ready = %+v, want Unknown/%s", ready, v1alpha1.ReasonValidatorUnavailable)
	}
}

func TestGatewayReconcile_UnavailableValidatorStillReportsFailedRollout(t *testing.T) {
	gw := rollingGateway("applied", "img:v1")
	dep := makeConvergedDeployment(gw, "applied")
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	}}
	c := fakeClientBuilder().WithObjects(gw, dep).WithStatusSubresource(gw).Build()
	reconcileWithUnavailableValidator(t, c, gw)
	ready := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonRolloutFailed {
		t.Errorf("Ready = %+v, want False/%s: a failed rollout outranks the unjudged render",
			ready, v1alpha1.ReasonRolloutFailed)
	}
}

func TestGatewayReconcile_UnavailableDeploymentDuringRolloutIsDeployingNotError(t *testing.T) {
	gw := rollingGateway("applied", "img:v1")
	dep := makeConvergedDeployment(gw, "applied")
	dep.Status = appsv1.DeploymentStatus{
		Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable",
		}},
	}
	c := fakeClientBuilder().WithObjects(gw, dep).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if phase := getGateway(t, c, gw).Status.Phase; phase != v1alpha1.PhaseDeploying {
		t.Errorf("phase = %s, want %s: a Deployment that is unavailable while its rollout is in flight is not an error",
			phase, v1alpha1.PhaseDeploying)
	}
}

func TestGatewayReconcile_ImageChangeIsNotReadyUntilTheDeploymentRunsIt(t *testing.T) {
	gw := servingGateway("applied", "img:v1")
	stale := makeConvergedDeployment(gw, "applied")
	c := fakeClientBuilder().WithObjects(gw, stale).WithStatusSubresource(gw).
		WithInterceptorFuncs(staleDeploymentReads(stale)).Build()
	rend := &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "applied", DesiredImage: "img:v2",
	}}
	r := newTestGatewayReconciler(c, rend, &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	progressing := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason != "DeploymentUpdated" {
		t.Errorf("Progressing = %+v, want True/DeploymentUpdated while the Deployment still runs img:v1", progressing)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want not True before the Deployment runs img:v2", ready)
	}
}

func TestGatewayReconcile_PluginChangeIsNotReadyUntilTheDeploymentRunsIt(t *testing.T) {
	gw := servingGateway("applied", "img:v1")
	gw.Status.PluginChecksum = "plugins-old"
	stale := makeConvergedDeployment(gw, "applied")
	stale.Spec.Template.Annotations[resources.PluginChecksumAnnotation] = "plugins-old"
	c := fakeClientBuilder().WithObjects(gw, stale).WithStatusSubresource(gw).
		WithInterceptorFuncs(staleDeploymentReads(stale)).Build()
	rend := &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "applied", DesiredImage: "img:v1", PluginChecksum: "plugins-new",
	}}
	r := newTestGatewayReconciler(c, rend, &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	progressing := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason != "DeploymentUpdated" {
		t.Errorf("Progressing = %+v, want True/DeploymentUpdated while the Deployment still runs the old plugins",
			progressing)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want not True before the Deployment runs the new plugins", ready)
	}
}
