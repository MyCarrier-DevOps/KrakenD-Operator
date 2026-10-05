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
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
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
		APIReader: c,

		LicenseParser: &mockLicenseParser{err: errors.New("no license in this test")},
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
			appliedCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name:            resources.ConfigMapName(gw, "applied"),
				Namespace:       gw.Namespace,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(gw, v1alpha1.GroupVersion.WithKind("KrakenDGateway"))},
			}}
			resources.BuildConfigMap(appliedCM, gw, []byte(`{"applied":true}`), "applied")
			c := fakeClientBuilder().WithObjects(gw, appliedCM).WithStatusSubresource(gw).Build()
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
			var cm corev1.ConfigMap
			getObject(t, c, gw, appliedCM.Name, &cm)
			if got, want := cm.Data["krakend.json"], appliedCM.Data["krakend.json"]; got != want {
				t.Errorf("a config that did not pass must not reach the ConfigMap: got %q, want %q", got, want)
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
	ready := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonConfigValidationFailed {
		t.Errorf("Ready = %+v, want False/%s: nothing is running and the config was rejected",
			ready, v1alpha1.ReasonConfigValidationFailed)
	}
}

func TestGatewayReconcile_UnavailableValidatorIsNotReady(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
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
	gw := rollingGateway("applied", convergedImage)
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
	gw := rollingGateway("applied", convergedImage)
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
	gw := rollingGateway("applied", convergedImage)
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
	gw := servingGateway("applied", convergedImage)
	stale := makeConvergedDeployment(gw, "applied")
	gw.Spec.Image = "img:v2"
	c := fakeClientBuilder().WithObjects(gw, stale).WithStatusSubresource(gw).
		WithInterceptorFuncs(staleDeploymentReads(stale)).Build()
	rend := &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "applied",
	}}
	r := newTestGatewayReconciler(c, rend, &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	progressing := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason != "DeploymentUpdated" {
		t.Errorf("Progressing = %+v, want True/DeploymentUpdated while the Deployment still runs the old image", progressing)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want not True before the Deployment runs img:v2", ready)
	}
}

func TestGatewayReconcile_PluginChangeIsNotReadyUntilTheDeploymentRunsIt(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	gw.Status.PluginChecksum = "plugins-old"
	stale := makeConvergedDeployment(gw, "applied")
	stale.Spec.Template.Annotations[resources.PluginChecksumAnnotation] = "plugins-old"
	c := fakeClientBuilder().WithObjects(gw, stale).WithStatusSubresource(gw).
		WithInterceptorFuncs(staleDeploymentReads(stale)).Build()
	rend := &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "applied", PluginChecksum: "plugins-new",
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

func TestGatewayReconcile_RejectedRenderWithADeploymentWritesStatusOnce(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	c, phases := gatewayStatusWrites(gw, makeConvergedDeployment(gw, "applied"))
	r := newTestGatewayReconciler(c, renderOutput("new"),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")})

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if len(*phases) != 1 {
		t.Errorf("two reconciles of a rejected render wrote status %d times (%v), want once", len(*phases), *phases)
	}
}

func TestDeploymentConverged(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(dep *appsv1.Deployment)
		want   func(in *infraInputs)
		wantOK bool
	}{
		{name: "matching Deployment", wantOK: true},
		{
			name: "container image rewritten after admission",
			mutate: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Spec.Containers[0].Image = convergedImage + "@sha256:abc"
			},
			wantOK: true,
		},
		{
			name:   "image annotation differs",
			want:   func(in *infraInputs) { in.image = "img:v2" },
			wantOK: false,
		},
		{
			name: "image annotation missing on a Deployment from before the operator set it",
			mutate: func(dep *appsv1.Deployment) {
				delete(dep.Spec.Template.Annotations, resources.ImageAnnotation)
			},
			wantOK: false,
		},
		{
			name:   "plugin checksum differs",
			want:   func(in *infraInputs) { in.pluginChecksum = "plugins-new" },
			wantOK: false,
		},
		{
			name: "plugin annotation present while none is wanted",
			mutate: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Annotations[resources.PluginChecksumAnnotation] = "plugins-old"
			},
			wantOK: false,
		},
		{
			name:   "license checksum differs",
			want:   func(in *infraInputs) { in.licenseChecksum = "license-new" },
			wantOK: false,
		},
		{
			name: "license annotation present while none is wanted",
			mutate: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Annotations[resources.LicenseChecksumAnnotation] = "license-old"
			},
			wantOK: false,
		},
		{
			name: "spec not yet observed",
			mutate: func(dep *appsv1.Deployment) {
				dep.Generation = 2
				dep.Status.ObservedGeneration = 1
			},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := makeConvergedDeployment(servingGateway("applied", convergedImage), "applied")
			if tc.mutate != nil {
				tc.mutate(dep)
			}
			in := convergedInputs("applied")
			if tc.want != nil {
				tc.want(&in)
			}
			if got := deploymentConverged(dep, in); got != tc.wantOK {
				t.Errorf("deploymentConverged = %v, want %v", got, tc.wantOK)
			}
		})
	}
}

func TestGatewayReconcile_ImageRolloutCompletesWhenTheDeploymentRunsIt(t *testing.T) {
	gw := rollingGateway("applied", convergedImage)
	gw.Spec.Image = "img:v2"
	dep := makeConvergedDeployment(gw, "applied")
	dep.Spec.Template.Annotations[resources.ImageAnnotation] = "img:v2"
	dep.Spec.Template.Spec.Containers[0].Image = "img:v2"
	// The fake client returns the Deployment as CreateOrUpdate wrote it, so
	// the seed would be overwritten; reads return it as the cache holds it.
	c := fakeClientBuilder().WithObjects(gw, dep).WithStatusSubresource(gw).
		WithInterceptorFuncs(staleDeploymentReads(dep)).Build()
	rend := &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "applied",
	}}
	r := newTestGatewayReconciler(c, rend, &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ready := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True once the Deployment runs img:v2", ready)
	}
}

// eventsWithReason drains rec and counts the recorded events with reason.
func eventsWithReason(rec *record.FakeRecorder, reason string) int {
	n := 0
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, " "+reason+" ") {
			n++
		}
	}
	return n
}

// optionalCRDMapper is a RESTMapper that knows the scheme's kinds plus gvks.
// It stands in for a cluster where those optional CRDs are installed.
func optionalCRDMapper(gvks ...schema.GroupVersionKind) meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	for gvk := range testScheme().AllKnownTypes() {
		m.Add(gvk, meta.RESTScopeNamespace)
	}
	for _, gvk := range gvks {
		m.Add(gvk, meta.RESTScopeNamespace)
	}
	return m
}

// deploymentPastProgressDeadline is gw's Deployment as the Deployment
// controller reports it once a rollout has exceeded its progress deadline.
func deploymentPastProgressDeadline(gw *v1alpha1.KrakenDGateway) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type:   appsv1.DeploymentProgressing,
			Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded",
		}}},
	}
}

func TestGatewayReconcile_EventsOnlyOnConditionTransitions(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		setup  func(gw *v1alpha1.KrakenDGateway) []client.Object
	}{
		{
			name:   "VirtualService reconciled after its CRD appeared",
			reason: v1alpha1.ReasonIstioVSCreated,
			setup: func(gw *v1alpha1.KrakenDGateway) []client.Object {
				gw.Spec.Istio = &v1alpha1.IstioSpec{
					Enabled: true, Hosts: []string{"api.example.com"}, Gateways: []string{"istio-system/gw"},
				}
				meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
					Type: v1alpha1.ConditionIstioConfigured, Status: metav1.ConditionFalse, Reason: "CRDNotInstalled",
				})
				return nil
			},
		},
		{
			name:   "rollout past its progress deadline",
			reason: v1alpha1.ReasonRolloutFailed,
			setup: func(gw *v1alpha1.KrakenDGateway) []client.Object {
				return []client.Object{deploymentPastProgressDeadline(gw)}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.Status.ConfigChecksum = "applied"
			objs := append([]client.Object{gw}, tc.setup(gw)...)
			c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(virtualServiceGVK)).
				WithObjects(objs...).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

			for range 3 {
				if err := reconcileGateway(t, r, gw); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			}
			if got := eventsWithReason(r.Recorder.(*record.FakeRecorder), tc.reason); got != 1 {
				t.Errorf("%s events over three reconciles = %d, want 1 (only the transition)", tc.reason, got)
			}
		})
	}
}

// renderOf is a renderer that renders config, with its real checksum.
func renderOf(config string) *mockRenderer {
	return &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(config), Checksum: hash.SHA256Hex([]byte(config)),
	}}
}

// getObject fetches name from gw's namespace into obj and fails the test on
// error.
func getObject(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway, name string, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: gw.Namespace, Name: name}, obj); err != nil {
		t.Fatalf("getting %s: %v", name, err)
	}
}

// mountedConfig returns the ConfigMap the gateway Deployment's pods mount as
// their config.
func mountedConfig(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway) string {
	t.Helper()
	var dep appsv1.Deployment
	getObject(t, c, gw, gw.Name, &dep)
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "config" && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}

func TestGatewayReconcile_PublishesTheAppliedConfigAsAnImmutableConfigMap(t *testing.T) {
	gw := reconciledGateway()
	const config = `{"version":3,"name":"published"}`
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := resources.ConfigMapName(gw, hash.SHA256Hex([]byte(config)))
	var cm corev1.ConfigMap
	getObject(t, c, gw, want, &cm)
	if cm.Immutable == nil || !*cm.Immutable {
		t.Error("the published config ConfigMap must be immutable")
	}
	if cm.Data[resources.ConfigKey] != config {
		t.Errorf("config data = %q, want %q", cm.Data[resources.ConfigKey], config)
	}
	if got := mountedConfig(t, c, gw); got != want {
		t.Errorf("Deployment mounts %q, want %q", got, want)
	}
}

// legacyConfigMap is the ConfigMap an operator version before content
// addressing kept under the gateway's own name.
func legacyConfigMap(gw *v1alpha1.KrakenDGateway, config string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            gw.Name,
			Namespace:       gw.Namespace,
			Labels:          resources.StandardLabels(gw),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(gw, v1alpha1.GroupVersion.WithKind("KrakenDGateway"))},
		},
		Data: map[string]string{resources.ConfigKey: config},
	}
}

// legacyDeployment is a gateway Deployment that mounts legacyConfigMap.
func legacyDeployment(gw *v1alpha1.KrakenDGateway) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "config",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: gw.Name},
				}},
			}},
		}}},
	}
}

func TestGatewayReconcile_UpgradeWhileRejectedSeedsAppliedConfigMapFromLegacy(t *testing.T) {
	gw := reconciledGateway()
	const lastGood = `{"version":3,"name":"last-good"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(lastGood))
	c := fakeClientBuilder().
		WithObjects(gw, legacyConfigMap(gw, lastGood), legacyDeployment(gw)).
		WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rejected"}`),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := resources.ConfigMapName(gw, gw.Status.ConfigChecksum)
	var cm corev1.ConfigMap
	getObject(t, c, gw, want, &cm)
	if cm.Data[resources.ConfigKey] != lastGood {
		t.Errorf("seeded config = %q, want the last-good %q", cm.Data[resources.ConfigKey], lastGood)
	}
	if got := mountedConfig(t, c, gw); got != want {
		t.Errorf("Deployment mounts %q, want the seeded %q", got, want)
	}
}

func TestGatewayReconcile_HoldsTheDeploymentWhenNoConfigMapHoldsTheAppliedConfig(t *testing.T) {
	gw := reconciledGateway()
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(`{"version":3,"name":"gone"}`))
	c := fakeClientBuilder().WithObjects(gw, legacyDeployment(gw)).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rejected"}`),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := mountedConfig(t, c, gw); got != gw.Name {
		t.Errorf("Deployment mounts %q; with no ConfigMap holding the applied config it must be left mounting %q",
			got, gw.Name)
	}
}

func TestGatewayReconcile_RefusesAConfigMapItDoesNotControl(t *testing.T) {
	gw := reconciledGateway()
	const config = `{"version":3,"name":"squatted"}`
	squatter := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: resources.ConfigMapName(gw, hash.SHA256Hex([]byte(config))), Namespace: gw.Namespace,
	}, Data: map[string]string{resources.ConfigKey: `{"version":3,"name":"something else"}`}}
	c := fakeClientBuilder().WithObjects(gw, squatter).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

	err := reconcileGateway(t, r, gw)
	if err == nil || !strings.Contains(err.Error(), "not controlled by gateway") {
		t.Fatalf("reconcile error = %v, want a refusal to serve a ConfigMap the gateway does not control", err)
	}
	if got := getGateway(t, c, gw); got.Status.ConfigChecksum != "" {
		t.Errorf("status.configChecksum = %q; a config that was never published must not be recorded as applied",
			got.Status.ConfigChecksum)
	}
}

func TestGatewayReconcile_UpgradeRolloutDoesNotRerunThePostRestartJob(t *testing.T) {
	const config = `{"version":3,"name":"already-applied"}`
	sum := hash.SHA256Hex([]byte(config))
	gw := makeGWWithJob("echo published")
	gw.Status.ConfigChecksum = sum
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, gw, sum)
	if err != nil {
		t.Fatal(err)
	}
	gw.Status.LastPostRestartJobChecksum = jobChecksum
	dep := makeConvergedDeployment(gw, sum)
	dep.Spec.Template.Spec.Volumes = legacyDeployment(gw).Spec.Template.Spec.Volumes
	c := fakeClientBuilder().WithObjects(gw, legacyConfigMap(gw, config), dep).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(gw.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("the migration rollout re-ran the post-restart Job: %d Job(s) created", len(jobs.Items))
	}
	want := resources.ConfigMapName(gw, sum)
	if got := mountedConfig(t, c, gw); got != want {
		t.Errorf("Deployment mounts %q, want it re-pointed at %q", got, want)
	}
	var migrated corev1.ConfigMap
	getObject(t, c, gw, want, &migrated)
	if migrated.Data[resources.ConfigKey] != config {
		t.Errorf("migrated config = %q, want the legacy content %q", migrated.Data[resources.ConfigKey], config)
	}
}

func TestGatewayReconcile_RestoresADeletedAppliedConfigMapFromTheAppliedConfigNotTheRejectedRender(t *testing.T) {
	gw := reconciledGateway()
	const lastGood = `{"version":3,"name":"last-good"}`
	const rejected = `{"version":3,"name":"rejected"}`
	// A ReplicaSet still mounts the legacy ConfigMap, so garbage collection
	// keeps it as the only copy the restore can come from.
	c := fakeClientBuilder().
		WithObjects(gw, legacyConfigMap(gw, lastGood), gatewayReplicaSet(gw, "test-gw-old", gw.Name, 1)).
		WithStatusSubresource(gw).Build()
	rend := renderOf(lastGood)
	val := &countingValidator{}
	r := newTestGatewayReconciler(c, rend, val)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile of the last-good config: %v", err)
	}
	appliedName := resources.ConfigMapName(gw, hash.SHA256Hex([]byte(lastGood)))
	var applied corev1.ConfigMap
	getObject(t, c, gw, appliedName, &applied)
	if err := c.Delete(context.Background(), &applied); err != nil {
		t.Fatal(err)
	}

	*rend = *renderOf(rejected)
	val.err = rejectedBy("- at '/endpoints/0/endpoint': bad")
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile of the rejected config: %v", err)
	}

	var restored corev1.ConfigMap
	getObject(t, c, gw, appliedName, &restored)
	if got := restored.Data[resources.ConfigKey]; got != lastGood {
		t.Errorf("restored config = %q, want the applied %q and never the rejected render", got, lastGood)
	}
	if got := mountedConfig(t, c, gw); got != appliedName {
		t.Errorf("Deployment mounts %q, want the restored %q", got, appliedName)
	}
	var rejectedCM corev1.ConfigMap
	err := c.Get(context.Background(), types.NamespacedName{
		Namespace: gw.Namespace, Name: resources.ConfigMapName(gw, hash.SHA256Hex([]byte(rejected))),
	}, &rejectedCM)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the rejected render must never be published; Get returned %v", err)
	}
}

func TestGatewayReconcile_HoldsTheDeploymentWhenTheAppliedConfigMapIsNotTheGatewaysOwn(t *testing.T) {
	const applied = `{"version":3,"name":"applied"}`
	cases := []struct {
		name   string
		render *mockRenderer
		val    renderer.Validator
	}{
		{
			name:   "newer render rejected",
			render: renderOf(`{"version":3,"name":"rejected"}`),
			val:    &countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")},
		},
		{name: "render is the applied config", render: renderOf(applied), val: &mockValidator{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(applied))
			squatter := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: resources.ConfigMapName(gw, gw.Status.ConfigChecksum), Namespace: gw.Namespace,
			}, Data: map[string]string{resources.ConfigKey: `{"version":3,"name":"something else"}`}}
			c := fakeClientBuilder().WithObjects(gw, squatter, legacyDeployment(gw)).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, tc.render, tc.val)

			err := reconcileGateway(t, r, gw)
			if err == nil || !strings.Contains(err.Error(), "not controlled by gateway") {
				t.Fatalf("reconcile error = %v, want a refusal to serve a ConfigMap the gateway does not control", err)
			}
			if got := mountedConfig(t, c, gw); got != gw.Name {
				t.Errorf("Deployment mounts %q; it must be held mounting %q", got, gw.Name)
			}
		})
	}
}

func TestGatewayReconcile_HeldDeploymentReportsNoRollout(t *testing.T) {
	gw := servingGateway(hash.SHA256Hex([]byte(`{"version":3,"name":"gone"}`)), convergedImage)
	c := fakeClientBuilder().WithObjects(gw, legacyDeployment(gw)).WithStatusSubresource(gw).Build()
	// The render names another image, but the Deployment is held, so no
	// rollout starts.
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rejected"}`),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")})
	restartsBefore := testutil.ToFloat64(rollingRestarts)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	progressing := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing != nil && progressing.Reason == "DeploymentUpdated" {
		t.Errorf("Progressing = %+v; a held Deployment starts no rollout", progressing)
	}
	if got := testutil.ToFloat64(rollingRestarts); got != restartsBefore {
		t.Errorf("rollingRestarts rose from %v to %v for a rollout that never started", restartsBefore, got)
	}
}

func TestGatewayReconcile_HoldLogSaysWhyTheDeploymentIsHeld(t *testing.T) {
	const applied = `{"version":3,"name":"applied"}`
	gw := reconciledGateway()
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(applied))
	squatter := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: resources.ConfigMapName(gw, gw.Status.ConfigChecksum), Namespace: gw.Namespace,
	}}
	c := fakeClientBuilder().WithObjects(gw, squatter, legacyDeployment(gw)).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rejected"}`),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")})
	var logged strings.Builder
	ctx := logf.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		logged.WriteString(prefix + args + "\n")
	}, funcr.Options{}))

	_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})

	if !strings.Contains(logged.String(), "holding the Deployment as it is") ||
		!strings.Contains(logged.String(), "not controlled by gateway") {
		t.Errorf("the hold log must say the ConfigMap failed verification, got:\n%s", logged.String())
	}
}

// ownedConfigMap is a config ConfigMap controlled by gw and created at
// created. revision marks a content-addressed revision; otherwise it stands
// for the pre-content-addressing ConfigMap.
func ownedConfigMap(gw *v1alpha1.KrakenDGateway, name string, created time.Time, revision bool) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:              name,
		Namespace:         gw.Namespace,
		Labels:            resources.StandardLabels(gw),
		CreationTimestamp: metav1.NewTime(created),
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(gw, v1alpha1.GroupVersion.WithKind("KrakenDGateway")),
		},
	}}
	if revision {
		cm.Labels[resources.ConfigRevisionLabel] = name
	}
	return cm
}

// gatewayReplicaSet is a ReplicaSet of gw's Deployment that mounts configMap
// and runs replicas pods.
func gatewayReplicaSet(gw *v1alpha1.KrakenDGateway, name, configMap string, replicas int32) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: gw.Namespace,
			Labels:    resources.StandardLabels(gw),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: gw.Name, UID: "dep-uid", Controller: ptr.To(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: ptr.To(replicas),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
				Name: "config",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
				}},
			}}}},
		},
		Status: appsv1.ReplicaSetStatus{Replicas: replicas},
	}
}

// remainingConfigMaps lists the ConfigMap names left in gw's namespace,
// sorted.
func remainingConfigMaps(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway) []string {
	t.Helper()
	var list corev1.ConfigMapList
	if err := c.List(context.Background(), &list, client.InNamespace(gw.Namespace)); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	slices.Sort(names)
	return names
}

func TestCollectConfigMaps_DeletesWhatNothingCanMount(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	at := func(h int) time.Time { return testNow.Add(time.Duration(h) * time.Hour) }
	unowned := ownedConfigMap(gw, "someone-elses", at(0), true)
	unowned.OwnerReferences = nil
	c := fakeClientBuilder().WithObjects(
		gw, unowned,
		ownedConfigMap(gw, gw.Name, at(0), false), // pre-content-addressing
		ownedConfigMap(gw, "test-gw-config-r1", at(1), true),
		ownedConfigMap(gw, "test-gw-config-r2", at(2), true),
		ownedConfigMap(gw, "test-gw-config-r3", at(3), true),
		ownedConfigMap(gw, "test-gw-config-r4", at(4), true),
		ownedConfigMap(gw, "test-gw-config-r5", at(5), true),
		ownedConfigMap(gw, "test-gw-config-r6", at(6), true),
		gatewayReplicaSet(gw, "test-gw-live", "test-gw-config-r2", 1),
		gatewayReplicaSet(gw, "test-gw-idle", gw.Name, 0),
	).Build()
	r := newTestGatewayReconciler(c, &mockRenderer{}, &mockValidator{})

	if err := r.collectConfigMaps(context.Background(), gw, "test-gw-config-r6"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := []string{"someone-elses", "test-gw-config-r2", "test-gw-config-r4", "test-gw-config-r5", "test-gw-config-r6"}
	if got := remainingConfigMaps(t, c, gw); !slices.Equal(got, want) {
		t.Errorf("remaining ConfigMaps = %v, want %v", got, want)
	}
}

func TestCollectConfigMaps_KeepsInUseEvenWhenOldest(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	at := func(h int) time.Time { return testNow.Add(time.Duration(h) * time.Hour) }
	c := fakeClientBuilder().WithObjects(gw,
		ownedConfigMap(gw, "test-gw-config-r1", at(1), true), // reverted to: oldest, no ReplicaSet yet
		ownedConfigMap(gw, "test-gw-config-r2", at(2), true),
		ownedConfigMap(gw, "test-gw-config-r3", at(3), true),
		ownedConfigMap(gw, "test-gw-config-r4", at(4), true),
		ownedConfigMap(gw, "test-gw-config-r5", at(5), true),
	).Build()
	r := newTestGatewayReconciler(c, &mockRenderer{}, &mockValidator{})

	if err := r.collectConfigMaps(context.Background(), gw, "test-gw-config-r1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := []string{"test-gw-config-r1", "test-gw-config-r4", "test-gw-config-r5"}
	if got := remainingConfigMaps(t, c, gw); !slices.Equal(got, want) {
		t.Errorf("remaining ConfigMaps = %v, want %v (in use is kept however old, inside a history of %d)",
			got, want, configMapHistoryLimit)
	}
}

func TestCollectConfigMaps_ListsNoReplicaSetsWhenNothingIsCollectable(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	lists := 0
	c := fakeClientBuilder().WithObjects(gw,
		ownedConfigMap(gw, "test-gw-config-r1", testNow, true),
		ownedConfigMap(gw, "test-gw-config-r2", testNow.Add(time.Hour), true),
	).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.ReplicaSetList); ok {
				lists++
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	r := newTestGatewayReconciler(c, &mockRenderer{}, &mockValidator{})

	if err := r.collectConfigMaps(context.Background(), gw, "test-gw-config-r2"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if lists != 0 {
		t.Errorf("ReplicaSet lists = %d, want 0 when nothing can be collected", lists)
	}
}

func TestGatewayReconcile_CollectsTheLegacyConfigMapOnceNoLiveReplicaSetMountsIt(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	const config = `{"version":3,"name":"migrated"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	oldRS := gatewayReplicaSet(gw, "test-gw-old", gw.Name, 1)
	c := fakeClientBuilder().WithObjects(gw, legacyConfigMap(gw, config), legacyDeployment(gw), oldRS).
		WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := remainingConfigMaps(t, c, gw); !slices.Contains(got, gw.Name) {
		t.Fatalf("remaining ConfigMaps = %v; the legacy ConfigMap must survive while a live ReplicaSet mounts it", got)
	}

	var live appsv1.ReplicaSet
	getObject(t, c, gw, oldRS.Name, &live)
	live.Spec.Replicas = ptr.To(int32(0))
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	live.Status.Replicas = 0 // the rollout finished: the old ReplicaSet is scaled to zero
	if err := c.Status().Update(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := remainingConfigMaps(t, c, gw); slices.Contains(got, gw.Name) {
		t.Errorf("remaining ConfigMaps = %v; the legacy ConfigMap must be collected once the rollout finished", got)
	}
}

func TestPublishConfig_VerifiesTheConfigMapALostCreateRaceLeftBehind(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	const config = `{"version":3,"name":"raced"}`
	checksum := hash.SHA256Hex([]byte(config))
	foreign := ownedConfigMap(gw, resources.ConfigMapName(gw, checksum), testNow, true)
	foreign.OwnerReferences = nil
	live := fakeClientBuilder().WithObjects(gw, foreign).Build()
	// The cache has not seen the ConfigMap yet, so the Get misses and the
	// Create loses to what the API server already holds.
	stale := interceptor.NewClient(live, interceptor.Funcs{
		Get: func(
			ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return apierrors.NewNotFound(corev1.Resource("configmaps"), key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := newTestGatewayReconciler(stale, &mockRenderer{}, &mockValidator{})
	r.APIReader = live

	err := r.publishConfig(context.Background(), gw, []byte(config), checksum)

	if err == nil || !strings.Contains(err.Error(), "not controlled by gateway") {
		t.Errorf("publishConfig = %v, want the lost create race's ConfigMap rejected as not the gateway's own", err)
	}
}

func TestGatewayReconcile_ACollectionFailureDoesNotStallTheRestOfTheInfrastructure(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: ptr.To(int32(2)), MaxReplicas: 4}
	const config = `{"version":3,"name":"migrated"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	c := fakeClientBuilder().WithObjects(gw, legacyConfigMap(gw, config), legacyDeployment(gw)).
		WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})
	// The legacy ConfigMap is a candidate, so collection reads ReplicaSets;
	// the operator's role may not allow it.
	r.APIReader = interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.ReplicaSetList); ok {
				return errors.New("replicasets is forbidden")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	err := reconcileGateway(t, r, gw)

	if err == nil || !strings.Contains(err.Error(), "replicasets is forbidden") {
		t.Errorf("reconcile = %v, want the collection failure returned", err)
	}
	var hpa autoscalingv2.HorizontalPodAutoscaler
	getObject(t, c, gw, gw.Name, &hpa)
}

func TestPublishConfig_FailsWhenTheConfigMapAVanishedCreateRaceLeftIsGone(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	const config = `{"version":3,"name":"raced"}`
	// The Create loses to a ConfigMap that is deleted before the live read.
	c := interceptor.NewClient(fakeClientBuilder().WithObjects(gw).Build(), interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(corev1.Resource("configmaps"), obj.GetName())
		},
	})
	r := newTestGatewayReconciler(c, &mockRenderer{}, &mockValidator{})

	err := r.publishConfig(context.Background(), gw, []byte(config), hash.SHA256Hex([]byte(config)))

	if err == nil {
		t.Error("publishConfig = nil, want an error so the pass retries; the ConfigMap is not there")
	}
}

func TestCollectConfigMaps_KeepsWhatAScaledDownReplicaSetStillRunsPodsFor(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	at := func(h int) time.Time { return testNow.Add(time.Duration(h) * time.Hour) }
	draining := gatewayReplicaSet(gw, "test-gw-draining", "test-gw-config-r1", 0)
	draining.Status.Replicas = 1 // scaled to zero, its pod is still terminating
	c := fakeClientBuilder().WithObjects(gw,
		ownedConfigMap(gw, "test-gw-config-r1", at(1), true),
		ownedConfigMap(gw, "test-gw-config-r2", at(2), true),
		ownedConfigMap(gw, "test-gw-config-r3", at(3), true),
		ownedConfigMap(gw, "test-gw-config-r4", at(4), true),
		draining,
	).Build()
	r := newTestGatewayReconciler(c, &mockRenderer{}, &mockValidator{})

	if err := r.collectConfigMaps(context.Background(), gw, "test-gw-config-r4"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if got := remainingConfigMaps(t, c, gw); !slices.Contains(got, "test-gw-config-r1") {
		t.Errorf("remaining ConfigMaps = %v, want test-gw-config-r1 kept while a pod of its ReplicaSet runs", got)
	}
}

// testEndpoint is a KrakenDEndpoint on the test gateway serving GET path.
func testEndpoint(name, path string) *v1alpha1.KrakenDEndpoint {
	return &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: path, Method: "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: "/x"}},
			}},
		},
	}
}

// badNamespaceVerdict is krakend check's lint output for an unknown
// extra_config key on rendered entry 1, which is GET /b, owned by default/bad
// when the gateway serves /a (default/good) and /b.
const badNamespaceVerdict = "- at '/endpoints/1/extra_config': additional properties 'bad/ns' not allowed"

func TestGatewayReconcile_RejectedConfigNamesTheEndpointAtFault(t *testing.T) {
	gw := reconciledGateway()
	good, bad := testEndpoint("good", "/a"), testEndpoint("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(bad)); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonGatewayConfigRejected ||
		!strings.Contains(cond.Message, "bad/ns") {
		t.Errorf("bad endpoint Accepted = %+v, want False/%s naming the finding",
			cond, v1alpha1.ReasonGatewayConfigRejected)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(good)); cond != nil {
		t.Errorf("good endpoint Accepted = %+v; an endpoint no finding names keeps the applied render's verdict (none yet)",
			cond)
	}
	cv := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || !strings.Contains(cv.Message, "default/bad") {
		t.Errorf("ConfigValid = %+v, want its message to name default/bad", cv)
	}
}

func TestGatewayReconcile_RememberedRejectionKeepsAttribution(t *testing.T) {
	gw := reconciledGateway()
	good, bad := testEndpoint("good", "/a"), testEndpoint("bad", "/b")
	endpointWrites := 0
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDEndpoint](&endpointWrites)).Build()
	val := &countingValidator{err: rejectedBy(badNamespaceVerdict)}
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	writesAfterFirst := endpointWrites
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if val.calls != 1 {
		t.Fatalf("validator calls = %d, want 1: the rejection memo answers the second pass", val.calls)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(bad)); cond == nil ||
		cond.Reason != v1alpha1.ReasonGatewayConfigRejected {
		t.Errorf("after the remembered rejection the bad endpoint's Accepted = %+v, want it still %s",
			cond, v1alpha1.ReasonGatewayConfigRejected)
	}
	if n := endpointWrites - writesAfterFirst; n != 0 {
		t.Errorf("endpoint status writes on the remembered pass = %d, want 0", n)
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonGatewayConfigRejected); n != 1 {
		t.Errorf("GatewayConfigRejected events over two reconciles = %d, want 1", n)
	}
}

func TestGatewayReconcile_AppliedConfigClearsTheBlame(t *testing.T) {
	gw := reconciledGateway()
	bad := testEndpoint("bad", "/b")
	writes := 0
	c := fakeClientBuilder().WithObjects(gw, bad).WithStatusSubresource(gw, bad).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDEndpoint](&writes)).Build()
	val := &countingValidator{err: rejectedBy("- at '/endpoints/0/extra_config': additional properties 'bad/ns' not allowed")}
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), val)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var fixed v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bad), &fixed); err != nil {
		t.Fatal(err)
	}
	fixed.Spec.Endpoints[0].Endpoint = "/b-fixed"
	if err := c.Update(context.Background(), &fixed); err != nil {
		t.Fatal(err)
	}
	val.err = nil
	drainEvents(r.Recorder.(*record.FakeRecorder))
	writesBefore := writes
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(bad)); cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonAccepted {
		t.Errorf("after the fix was applied, Accepted = %+v, want True/%s", cond, v1alpha1.ReasonAccepted)
	}
	if n := writes - writesBefore; n != 1 {
		t.Errorf("endpoint status writes when the fix was applied = %d, want 1", n)
	}
	normal := 0
	for _, e := range drainEvents(r.Recorder.(*record.FakeRecorder)) {
		if strings.HasPrefix(e, "Normal "+v1alpha1.ReasonAccepted+" ") {
			normal++
		}
	}
	if normal != 1 {
		t.Errorf("Normal %s events when the blame cleared = %d, want 1", v1alpha1.ReasonAccepted, normal)
	}
}

func TestGatewayReconcile_RejectionNamingNoEndpointBlamesNone(t *testing.T) {
	gw := reconciledGateway()
	ep := testEndpoint("ep", "/a")
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(gw, ep).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy("Parsing configuration file: krakend.json\n")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(ep)); cond != nil {
		t.Errorf("endpoint Accepted = %+v; a rejection naming no endpoint blames none", cond)
	}
	got := getGateway(t, c, gw)
	cv := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || cv.Status != metav1.ConditionFalse || cv.Reason != v1alpha1.ReasonConfigValidationFailed ||
		!strings.Contains(cv.Message, "no finding names a KrakenDEndpoint") {
		t.Errorf("ConfigValid = %+v, want False/%s saying no finding names an endpoint",
			cv, v1alpha1.ReasonConfigValidationFailed)
	}
	if got.Status.ConfigChecksum != "" {
		t.Errorf("configChecksum = %q, want it unset: nothing was applied", got.Status.ConfigChecksum)
	}
}

func TestGatewayReconcile_RejectedFirstRenderOverridesStaleAccepted(t *testing.T) {
	gw := reconciledGateway() // recreated: no config has ever been applied
	bad := testEndpoint("bad", "/b")
	// The previous gateway of the same name accepted this endpoint.
	bad.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: 1,
			Reason: v1alpha1.ReasonRefsResolved},
		{Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1,
			Reason: v1alpha1.ReasonAccepted},
	}
	c := fakeClientBuilder().WithObjects(gw, bad).WithStatusSubresource(gw, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy("- at '/endpoints/0/extra_config': additional properties 'bad/ns' not allowed")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var stored v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bad), &stored); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAccepted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonGatewayConfigRejected {
		t.Errorf("Accepted = %+v, want False/%s over the previous gateway's True",
			cond, v1alpha1.ReasonGatewayConfigRejected)
	}
	if status, _, _ := v1alpha1.EndpointReady(stored.Status.Conditions); status == metav1.ConditionTrue {
		t.Error("endpoint is Ready although the new gateway rejected its config")
	}
}

// withAccepted seeds ep's stored Accepted condition.
func withAccepted(ep *v1alpha1.KrakenDEndpoint, status metav1.ConditionStatus, reason string) *v1alpha1.KrakenDEndpoint {
	ep.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: ep.Generation,
			Reason: v1alpha1.ReasonRefsResolved},
		{Type: v1alpha1.ConditionAccepted, Status: status, ObservedGeneration: ep.Generation, Reason: reason},
	}
	return ep
}

func TestGatewayReconcile_RejectionLiftsABlameThatMovedAway(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	// An earlier rejection blamed good; the findings now name bad.
	good := withAccepted(testEndpoint("good", "/a"), metav1.ConditionFalse, v1alpha1.ReasonGatewayConfigRejected)
	bad := testEndpoint("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(good)); cond != nil {
		t.Errorf("good endpoint Accepted = %+v; a blame no finding repeats must be lifted", cond)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(bad)); cond == nil ||
		cond.Reason != v1alpha1.ReasonGatewayConfigRejected {
		t.Errorf("bad endpoint Accepted = %+v, want %s", cond, v1alpha1.ReasonGatewayConfigRejected)
	}
}

func TestGatewayReconcile_RejectionNamingNobodyLiftsAnOldBlame(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	ep := withAccepted(testEndpoint("ep", "/a"), metav1.ConditionFalse, v1alpha1.ReasonGatewayConfigRejected)
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(gw, ep).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy("- at '/extra_config': additional properties 'bad/ns' not allowed")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(ep)); cond != nil {
		t.Errorf("Accepted = %+v; a rejection naming no endpoint must lift the old blame", cond)
	}
}

func TestGatewayReconcile_RememberedRejectionAfterALiftedBlameWritesNothing(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	good := withAccepted(testEndpoint("good", "/a"), metav1.ConditionFalse, v1alpha1.ReasonGatewayConfigRejected)
	bad := testEndpoint("bad", "/b")
	writes := 0
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDEndpoint](&writes)).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	after := writes
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if n := writes - after; n != 0 {
		t.Errorf("endpoint status writes on the remembered pass = %d, want 0", n)
	}
}

func TestGatewayReconcile_NeverAppliedGatewayRemovesAStaleAccepted(t *testing.T) {
	gw := reconciledGateway() // recreated: no config has ever been applied
	stale := withAccepted(testEndpoint("stale", "/a"), metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	bad := testEndpoint("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, stale, bad).WithStatusSubresource(gw, stale, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertNotAccepted(t, c, stale)
}

// assertNotAccepted fails when ep still carries an Accepted condition or reads Ready.
func assertNotAccepted(t *testing.T, c client.Client, ep *v1alpha1.KrakenDEndpoint) {
	t.Helper()
	var stored v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &stored); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAccepted); cond != nil {
		t.Errorf("endpoint %s Accepted = %+v; no config has been applied, so none may be claimed", ep.Name, cond)
	}
	if status, _, _ := v1alpha1.EndpointReady(stored.Status.Conditions); status == metav1.ConditionTrue {
		t.Errorf("endpoint %s is Ready although no config has been applied", ep.Name)
	}
}

func TestGatewayReconcile_UnavailableValidatorOnANeverAppliedGatewayRemovesAStaleAccepted(t *testing.T) {
	gw := reconciledGateway()
	stale := withAccepted(testEndpoint("stale", "/a"), metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	c := fakeClientBuilder().WithObjects(gw, stale).WithStatusSubresource(gw, stale).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: errors.New("fork/exec /usr/local/bin/krakend: no such file or directory")})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("an unavailable validator must fail the reconcile so it is retried")
	}
	assertNotAccepted(t, c, stale)
}

func TestGatewayReconcile_UnavailableValidatorLiftsAnOldBlame(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	ep := withAccepted(testEndpoint("ep", "/a"), metav1.ConditionFalse, v1alpha1.ReasonGatewayConfigRejected)
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(gw, ep).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: errors.New("fork/exec /usr/local/bin/krakend: no such file or directory")})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("an unavailable validator must fail the reconcile so it is retried")
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(ep)); cond != nil {
		t.Errorf("Accepted = %+v; the config was not judged, so the old blame must lift", cond)
	}
}

func TestRejectionSummary_CountsFindingsNamingNoEndpoint(t *testing.T) {
	atts := []renderer.Attribution{
		{Endpoint: types.NamespacedName{Namespace: "default", Name: "bad"}, Index: 1, Message: "m1"},
		{Index: -1, Message: "m2"},
		{Index: -1, Message: "m3"},
	}
	want := "Rejected by krakend check; findings name KrakenDEndpoint(s) default/bad; 2 finding(s) name no endpoint."
	if got := rejectionSummary(atts); got != want {
		t.Errorf("rejectionSummary = %q, want %q", got, want)
	}
	if got := rejectionSummary(nil); !strings.Contains(got, "no finding names a KrakenDEndpoint") {
		t.Errorf("rejectionSummary(nil) = %q", got)
	}
}

func TestRejectionsByEndpoint_ListsEachFindingOncePerEndpoint(t *testing.T) {
	owner := types.NamespacedName{Namespace: "default", Name: "both"}
	// A router error blaming two entries of one CR is reported once per entry.
	atts := []renderer.Attribution{
		{Endpoint: owner, Index: 0, Message: "conflict"},
		{Endpoint: owner, Index: 1, Message: "conflict"},
	}
	msg := rejectionsByEndpoint(atts)[owner]
	if strings.Count(msg, "conflict") != 1 {
		t.Errorf("message = %q, want the finding once", msg)
	}
}

func TestGatewayReconcile_ServingGatewayKeepsAnUnblamedVerdict(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	good := withAccepted(testEndpoint("good", "/a"), metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	bad := testEndpoint("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, good, bad).WithStatusSubresource(gw, good, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(good)); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("good Accepted = %+v, want the applied render's True kept", cond)
	}
}

func TestGatewayReconcile_ServingGatewayKeepsAVerdictWhileTheValidatorIsUnavailable(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	good := withAccepted(testEndpoint("good", "/a"), metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	c := fakeClientBuilder().WithObjects(gw, good).WithStatusSubresource(gw, good).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: errors.New("fork/exec /usr/local/bin/krakend: no such file or directory")})
	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("an unavailable validator must fail the reconcile so it is retried")
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(good)); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("good Accepted = %+v, want the applied render's True kept", cond)
	}
}

func TestGatewayReconcile_StaleListCannotRemoveALiveAccepted(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	// The endpoint was blamed before; the live object has since been accepted.
	live := withAccepted(testEndpoint("x", "/a"), metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	staleList := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if eps, ok := list.(*v1alpha1.KrakenDEndpointList); ok {
				for i := range eps.Items {
					withAccepted(&eps.Items[i], metav1.ConditionFalse, v1alpha1.ReasonGatewayConfigRejected)
				}
			}
			return nil
		},
	}
	c := fakeClientBuilder().WithObjects(gw, live).WithStatusSubresource(gw, live).
		WithInterceptorFuncs(staleList).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy("- at '/extra_config': additional properties 'bad/ns' not allowed")})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(live)); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v; a stale list must not remove the live True", cond)
	}
}

func TestGatewayReconcile_StaleEmptyChecksumKeepsAnAcceptedEndpoint(t *testing.T) {
	cached := reconciledGateway() // the cache has not seen the first apply
	liveGW := servingGateway("applied", convergedImage)
	ep := withAccepted(testEndpoint("x", "/a"), metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	bad := testEndpoint("bad", "/b")
	c := fakeClientBuilder().WithObjects(cached, ep, bad).WithStatusSubresource(cached, ep, bad).Build()
	live := fakeClientBuilder().WithObjects(liveGW).WithStatusSubresource(liveGW).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})
	r.APIReader = live

	if err := reconcileGateway(t, r, cached); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := storedAccepted(t, c, client.ObjectKeyFromObject(ep)); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v; a stale empty checksum must not strip an accepted endpoint", cond)
	}
}

func TestGatewayReconcile_PartlyConflictedEndpointIsPartiallyAccepted(t *testing.T) {
	gw := reconciledGateway()
	older := testEndpoint("older", "/shared")
	older.CreationTimestamp = metav1.NewTime(testNow)
	newer := testEndpoint("newer", "/shared")
	newer.Spec.Endpoints = append(newer.Spec.Endpoints, v1alpha1.EndpointEntry{
		Endpoint: "/own", Method: "GET",
		Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: "/x"}},
	})
	newer.CreationTimestamp = metav1.NewTime(testNow.Add(time.Minute))
	lostAll := testEndpoint("lost-all", "/shared")
	lostAll.CreationTimestamp = metav1.NewTime(testNow.Add(2 * time.Minute))
	c := fakeClientBuilder().WithObjects(gw, older, newer, lostAll).
		WithStatusSubresource(gw, older, newer, lostAll).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	wantConflicts := []v1alpha1.EndpointConflict{{Endpoint: "/shared", Method: "GET", Winner: "default/older"}}
	for _, tc := range []struct {
		name      string
		status    metav1.ConditionStatus
		reason    string
		conflicts []v1alpha1.EndpointConflict
	}{
		{"older", metav1.ConditionTrue, v1alpha1.ReasonAccepted, nil},
		{"newer", metav1.ConditionTrue, v1alpha1.ReasonPartiallyAccepted, wantConflicts},
		{"lost-all", metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict, wantConflicts},
	} {
		var ep v1alpha1.KrakenDEndpoint
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: tc.name}, &ep); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted)
		if cond == nil || cond.Status != tc.status || cond.Reason != tc.reason {
			t.Errorf("%s: Accepted = %+v, want %s/%s", tc.name, cond, tc.status, tc.reason)
		}
		if !reflect.DeepEqual(ep.Status.Conflicts, tc.conflicts) {
			t.Errorf("%s: status.conflicts = %+v, want %+v", tc.name, ep.Status.Conflicts, tc.conflicts)
		}
	}
}

func TestGatewayReconcile_PartlyConflictedEndpointIsNotRewrittenOnTheNextPass(t *testing.T) {
	gw := reconciledGateway()
	older := testEndpoint("older", "/shared")
	older.CreationTimestamp = metav1.NewTime(testNow)
	newer := testEndpoint("newer", "/shared")
	newer.Spec.Endpoints = append(newer.Spec.Endpoints, v1alpha1.EndpointEntry{
		Endpoint: "/own", Method: "GET",
		Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: "/x"}},
	})
	newer.CreationTimestamp = metav1.NewTime(testNow.Add(time.Minute))
	var endpointWrites int
	c := fakeClientBuilder().WithObjects(gw, older, newer).
		WithStatusSubresource(gw, older, newer).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context, c client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption,
			) error {
				if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
					endpointWrites++
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if endpointWrites == 0 {
		t.Fatal("the first pass wrote no endpoint status; the test would prove nothing")
	}
	endpointWrites = 0
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if endpointWrites != 0 {
		t.Errorf("second reconcile wrote endpoint status %d times, want 0 in a steady state", endpointWrites)
	}
}

func TestGatewayReconcile_RejectedPassKeepsTheLiveConflicts(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	good := testEndpoint("good", "/a")
	bad := testEndpoint("bad", "/b")
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
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(bad), &got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAccepted); cond == nil ||
		cond.Reason != v1alpha1.ReasonGatewayConfigRejected {
		t.Fatalf("Accepted = %+v, want the rejection recorded", cond)
	}
	if !reflect.DeepEqual(got.Status.Conflicts, fresh) {
		t.Errorf("status.conflicts = %+v, want the live %+v kept", got.Status.Conflicts, fresh)
	}
}

func TestGatewayReconcile_NeverAppliedGatewayClearsLeftoverConflicts(t *testing.T) {
	gw := reconciledGateway() // recreated: no config has ever been applied
	stale := withAccepted(testEndpoint("stale", "/a"), metav1.ConditionTrue, v1alpha1.ReasonPartiallyAccepted)
	stale.Status.Conflicts = []v1alpha1.EndpointConflict{{Endpoint: "/a", Method: "GET", Winner: "default/old"}}
	bad := testEndpoint("bad", "/b")
	c := fakeClientBuilder().WithObjects(gw, stale, bad).WithStatusSubresource(gw, stale, bad).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}),
		&countingValidator{err: rejectedBy(badNamespaceVerdict)})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertNotAccepted(t, c, stale)
	var got v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Conflicts) != 0 {
		t.Errorf("status.conflicts = %+v, want cleared with the Accepted it belonged to", got.Status.Conflicts)
	}
}

// partlyConflictedPair returns an older endpoint and a newer one that shares
// only /shared with it, so the newer one is partly served.
func partlyConflictedPair() (older, newer *v1alpha1.KrakenDEndpoint) {
	older = testEndpoint("older", "/shared")
	older.CreationTimestamp = metav1.NewTime(testNow)
	newer = testEndpoint("newer", "/shared")
	newer.Spec.Endpoints = append(newer.Spec.Endpoints, v1alpha1.EndpointEntry{
		Endpoint: "/own", Method: "GET",
		Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: "/x"}},
	})
	newer.CreationTimestamp = metav1.NewTime(testNow.Add(time.Minute))
	return older, newer
}

func TestGatewayReconcile_WarnsWhenAnEndpointBecomesPartiallyAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		prev func(*v1alpha1.KrakenDEndpoint)
	}{
		{"from Accepted", func(ep *v1alpha1.KrakenDEndpoint) {
			withAccepted(ep, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
		}},
		{"from absent", func(*v1alpha1.KrakenDEndpoint) {}},
		{"from EndpointConflict", func(ep *v1alpha1.KrakenDEndpoint) {
			withAccepted(ep, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := reconciledGateway()
			older, newer := partlyConflictedPair()
			tc.prev(newer)
			c := fakeClientBuilder().WithObjects(gw, older, newer).WithStatusSubresource(gw, older, newer).Build()
			r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})

			if err := reconcileGateway(t, r, gw); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var warned int
			for _, e := range drainEvents(r.Recorder.(*record.FakeRecorder)) {
				if strings.HasPrefix(e, "Warning "+v1alpha1.ReasonPartiallyAccepted+" ") {
					warned++
				}
			}
			if warned != 1 {
				t.Errorf("PartiallyAccepted Warning events = %d, want 1", warned)
			}
		})
	}
}

func TestGatewayReconcile_PartiallyAcceptedEventsFireOnTransitionsOnly(t *testing.T) {
	gw := reconciledGateway()
	older, newer := partlyConflictedPair()
	c := fakeClientBuilder().WithObjects(gw, older, newer).WithStatusSubresource(gw, older, newer).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})
	rec := r.Recorder.(*record.FakeRecorder)
	count := func(prefix string) int {
		n := 0
		for _, e := range drainEvents(rec) {
			if strings.HasPrefix(e, prefix) {
				n++
			}
		}
		return n
	}

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	count("") // discard the first pass's events
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if n := count(""); n != 0 {
		t.Errorf("an unchanged PartiallyAccepted emitted %d events, want 0", n)
	}

	// The older endpoint goes away: newer is served whole again.
	if err := c.Delete(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if n := count("Normal " + v1alpha1.ReasonAccepted + " "); n != 1 {
		t.Errorf("Normal Accepted events after recovery = %d, want 1", n)
	}
}

// countEndpointStatusPatches wraps c so that n counts the KrakenDEndpoint
// status patches it receives.
func countEndpointStatusPatches(c client.WithWatch, n *int) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context, c client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption,
		) error {
			if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
				*n++
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}

func TestGatewayReconcile_ResolvedConflictClearsStatusWithOneWrite(t *testing.T) {
	gw := reconciledGateway()
	older, newer := partlyConflictedPair()
	base := fakeClientBuilder().WithObjects(gw, older, newer).WithStatusSubresource(gw, older, newer).Build()
	var writes int
	c := countEndpointStatusPatches(base, &writes)
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := c.Delete(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	writes = 0

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	var got v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(newer), &got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAccepted)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonAccepted {
		t.Errorf("Accepted = %+v, want True/Accepted", cond)
	}
	if len(got.Status.Conflicts) != 0 {
		t.Errorf("status.conflicts = %+v, want nil", got.Status.Conflicts)
	}
	if writes != 1 {
		t.Errorf("endpoint status writes = %d, want exactly 1", writes)
	}
}

func TestGatewayReconcile_LoserFollowsTheNewWinnerWhenTheOldestIsDeleted(t *testing.T) {
	gw := reconciledGateway()
	oldest := testEndpoint("oldest", "/shared")
	oldest.CreationTimestamp = metav1.NewTime(testNow)
	middle := testEndpoint("middle", "/shared")
	middle.CreationTimestamp = metav1.NewTime(testNow.Add(time.Minute))
	newest := testEndpoint("newest", "/shared")
	newest.CreationTimestamp = metav1.NewTime(testNow.Add(2 * time.Minute))
	c := fakeClientBuilder().WithObjects(gw, oldest, middle, newest).
		WithStatusSubresource(gw, oldest, middle, newest).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := c.Delete(context.Background(), oldest); err != nil {
		t.Fatal(err)
	}

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	var got v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(newest), &got); err != nil {
		t.Fatal(err)
	}
	want := []v1alpha1.EndpointConflict{{Endpoint: "/shared", Method: "GET", Winner: "default/middle"}}
	if !reflect.DeepEqual(got.Status.Conflicts, want) {
		t.Errorf("status.conflicts = %+v, want %+v", got.Status.Conflicts, want)
	}
}

// recordingValidator records the edition of every validation and returns err.
type recordingValidator struct {
	editions []v1alpha1.Edition
	err      error
}

func (v *recordingValidator) Validate(_ context.Context, _ []byte, edition v1alpha1.Edition) error {
	v.editions = append(v.editions, edition)
	return v.err
}

func TestGatewayReconcile_CEFallbackFlipRevalidatesTheSameRender(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true) // expired, falls back
	const config = `{"version":3,"name":"same-bytes-in-both-editions"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Status.ConfigEdition = v1alpha1.EditionEE
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	val := &recordingValidator{}
	r := newTestGatewayReconciler(c, renderOf(config), val)
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !slices.Equal(val.editions, []v1alpha1.Edition{v1alpha1.EditionCE}) {
		t.Fatalf("validations = %v, want one as CE: the same bytes applied as EE were never checked as CE",
			val.editions)
	}
	if got := getGateway(t, c, gw).Status.ConfigEdition; got != v1alpha1.EditionCE {
		t.Errorf("status.configEdition = %q, want CE once the CE check passed", got)
	}
}

func TestGatewayReconcile_StatusWithoutEditionAdoptsTheCurrentOne(t *testing.T) {
	gw := reconciledGateway()
	const config = `{"version":3,"name":"applied-before-the-upgrade"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	val := &recordingValidator{}
	r := newTestGatewayReconciler(c, renderOf(config), val)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(val.editions) != 0 {
		t.Errorf("validations = %v; an applied config from before configEdition existed must not be re-validated",
			val.editions)
	}
	if got := getGateway(t, c, gw).Status.ConfigEdition; got != v1alpha1.EditionCE {
		t.Errorf("status.configEdition = %q, want the CE gateway's edition recorded", got)
	}
}

func TestGatewayReconcile_RejectedCEFallbackKeepsEEImage(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	const config = `{"version":3,"name":"ee-only-until-stripped"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Status.ConfigEdition = v1alpha1.EditionEE
	// The applied config's ConfigMap exists (seeded from the pre-upgrade one),
	// so the Deployment is reconciled rather than held.
	c := fakeClientBuilder().WithObjects(gw, secret, legacyConfigMap(gw, config)).WithStatusSubresource(gw).Build()
	val := &recordingValidator{err: rejectedBy("ERROR testing the configuration file:\twildcards must be named")}
	r := newTestGatewayReconciler(c, renderOf(config), val)
	r.LicenseParser = parser

	image := func() string {
		var dep appsv1.Deployment
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
			t.Fatal(err)
		}
		return dep.Spec.Template.Spec.Containers[0].Image
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	eeImage := renderer.ResolveImage(gw, false)
	if got := image(); got != eeImage {
		t.Fatalf("image = %q while the CE render is rejected, want the EE image %q: "+
			"CE pods must never load a config validated only as EE", got, eeImage)
	}

	// The rejection is remembered for this render; fixing the input changes it.
	val.err = nil
	r.Renderer = renderOf(`{"version":3,"name":"ce-safe"}`)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got, ceImage := image(), renderer.ResolveImage(gw, true); got != ceImage {
		t.Errorf("image = %q after the CE render was applied, want the CE image %q", got, ceImage)
	}
}

// deployedImage is the image of the gateway Deployment's container.
func deployedImage(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway) string {
	t.Helper()
	var dep appsv1.Deployment
	getObject(t, c, gw, gw.Name, &dep)
	return dep.Spec.Template.Spec.Containers[0].Image
}

func TestGatewayReconcile_RejectedEditionFlipToCEKeepsTheEEImage(t *testing.T) {
	gw := reconciledGateway()
	const config = `{"version":3,"name":"applied-as-ee"}`
	gw.Spec.Edition = v1alpha1.EditionCE // flipped from EE
	gw.Spec.Image = "ce-custom:1"        // spec.image is read before the edition
	gw.Status.ActiveImage = "krakend/krakend-ee:2.7.0"
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Status.ConfigEdition = v1alpha1.EditionEE
	c := fakeClientBuilder().WithObjects(gw, legacyConfigMap(gw, config)).WithStatusSubresource(gw).Build()
	val := &recordingValidator{err: rejectedBy("ERROR testing the configuration file:\tnot a CE config")}
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rendered-as-ce"}`), val)

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got, want := deployedImage(t, c, gw), "krakend/krakend-ee:2.7.0"; got != want {
		t.Errorf("image = %q while the CE render is rejected, want the applied edition's image %q", got, want)
	}
}

func TestGatewayReconcile_RejectedEditionFlipToEEKeepsTheCEImage(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(365*24*time.Hour), true) // valid license
	const config = `{"version":3,"name":"applied-as-ce"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Status.ConfigEdition = v1alpha1.EditionCE
	gw.Spec.Image = "ee-custom:1"
	gw.Spec.CEImage = "ce-custom:1"
	gw.Status.ActiveImage = "deployed-ce:1" // differs from spec.ceImage, so only the deployed image can answer
	c := fakeClientBuilder().WithObjects(gw, secret, legacyConfigMap(gw, config)).WithStatusSubresource(gw).Build()
	val := &recordingValidator{err: rejectedBy("ERROR testing the configuration file:\tnot an EE config")}
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rendered-as-ee"}`), val)
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got, want := deployedImage(t, c, gw), "deployed-ce:1"; got != want {
		t.Errorf("image = %q while the EE render is rejected, want the applied edition's image %q", got, want)
	}
}

func TestGatewayReconcile_RejectedRenderStillRollsAPluginChange(t *testing.T) {
	gw := reconciledGateway()
	const applied = `{"version":3,"name":"applied-with-old-plugins"}`
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	val := &recordingValidator{}
	r := newTestGatewayReconciler(c, &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(applied), Checksum: hash.SHA256Hex([]byte(applied)), PluginChecksum: "plugins-old",
	}}, val)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	live := getGateway(t, c, gw)
	live.Status.Conditions = nil
	if err := c.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	// Plugins follow the spec, like the image: a rejected render does not hold them back.
	val.err = rejectedBy("- at '/endpoints/0/endpoint': bad")
	const rejected = `{"version":3,"name":"rejected-with-new-plugins"}`
	r.Renderer = &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(rejected), Checksum: hash.SHA256Hex([]byte(rejected)), PluginChecksum: "plugins-new",
	}}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var dep appsv1.Deployment
	getObject(t, c, gw, gw.Name, &dep)
	if got := dep.Spec.Template.Annotations[resources.PluginChecksumAnnotation]; got != "plugins-new" {
		t.Errorf("plugin annotation = %q, want plugins-new: the pods mount the spec's plugins", got)
	}
	got := getGateway(t, c, gw)
	if got.Status.PluginChecksum != "plugins-new" {
		t.Errorf("status.pluginChecksum = %q, want plugins-new", got.Status.PluginChecksum)
	}
	progressing := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Reason != "DeploymentUpdated" {
		t.Errorf("Progressing = %+v, want DeploymentUpdated for the plugin rollout", progressing)
	}
}

func TestGatewayReconcile_AdoptedEditionSurvivesARejectedFirstRender(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(365*24*time.Hour), true) // valid license
	const config = `{"version":3,"name":"applied-before-the-upgrade"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config)) // no configEdition: written by the previous operator
	c := fakeClientBuilder().WithObjects(gw, secret, legacyConfigMap(gw, config)).WithStatusSubresource(gw).Build()
	val := &recordingValidator{err: rejectedBy("ERROR testing the configuration file:\tbad")}
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"first-render-after-upgrade"}`), val)
	r.LicenseParser = parser
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The license expires and the CE fallback render is rejected too.
	parser.info.NotAfter = testNow.Add(-time.Minute)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got, want := deployedImage(t, c, gw), renderer.ResolveImage(gw, false); got != want {
		t.Errorf("image = %q, want the EE image %q the applied config was running", got, want)
	}
}

func TestGatewayReconcile_AdoptionReadsTheEditionFromTheActiveImage(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true) // falls back at the first evaluation
	const config = `{"version":3,"name":"applied-before-the-upgrade"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config)) // no configEdition
	gw.Status.ActiveImage = renderer.ResolveImage(gw, false)  // the pods run the EE image
	c := fakeClientBuilder().WithObjects(gw, secret, legacyConfigMap(gw, config)).WithStatusSubresource(gw).Build()
	val := &recordingValidator{err: rejectedBy("ERROR testing the configuration file:\tbad")}
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rendered-as-ce"}`), val)
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := getGateway(t, c, gw).Status.ConfigEdition; got != v1alpha1.EditionEE {
		t.Errorf("status.configEdition = %q, want EE: the pods run the EE image", got)
	}
}

func TestGatewayReconcile_AdoptionKeepsTheCurrentEditionForAnAmbiguousImage(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	const config = `{"version":3,"name":"applied-before-the-upgrade"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Status.ActiveImage = "registry.example/custom:1" // neither edition's image
	c := fakeClientBuilder().WithObjects(gw, secret, legacyConfigMap(gw, config)).WithStatusSubresource(gw).Build()
	val := &recordingValidator{err: rejectedBy("ERROR testing the configuration file:\tbad")}
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"rendered-as-ce"}`), val)
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := getGateway(t, c, gw).Status.ConfigEdition; got != v1alpha1.EditionCE {
		t.Errorf("status.configEdition = %q, want the current edition CE for an image that names neither", got)
	}
}

// fallbackEndpoints are two endpoints of the test gateway that use
// Enterprise-only features: one serves only an EE wildcard, the other
// protects a plain route with auth/api-keys.
func fallbackEndpoints() (wildcardOnly, withAPIKeys *v1alpha1.KrakenDEndpoint) {
	wildcardOnly = testEndpoint("wildcard-only", "/files/*")
	withAPIKeys = testEndpoint("with-api-keys", "/users")
	withAPIKeys.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(`{"auth/api-keys":{"roles":["admin"]}}`)}
	return wildcardOnly, withAPIKeys
}

func TestGatewayReconcile_CEFallbackListsWhatItRemovedFromEachEndpoint(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	wildcardOnly, withAPIKeys := fallbackEndpoints()
	c := fakeClientBuilder().WithObjects(gw, secret, wildcardOnly, withAPIKeys).
		WithStatusSubresource(gw, wildcardOnly, withAPIKeys).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, tc := range []struct {
		ep      *v1alpha1.KrakenDEndpoint
		status  metav1.ConditionStatus
		mention string
	}{
		{wildcardOnly, metav1.ConditionFalse, "GET /files/*: wildcard endpoint"},
		{withAPIKeys, metav1.ConditionTrue, "GET /users: extra_config auth/api-keys"},
	} {
		cond := storedAccepted(t, c, client.ObjectKeyFromObject(tc.ep))
		if cond == nil || cond.Status != tc.status || cond.Reason != v1alpha1.ReasonEEFeaturesStripped ||
			!strings.Contains(cond.Message, tc.mention) {
			t.Errorf("%s: Accepted = %+v, want %s/%s mentioning %q",
				tc.ep.Name, cond, tc.status, v1alpha1.ReasonEEFeaturesStripped, tc.mention)
		}
	}
}

func TestGatewayReconcile_CEFallbackAppliedListsTheRemovedFeatures(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	wildcardOnly, withAPIKeys := fallbackEndpoints()
	c := fakeClientBuilder().WithObjects(gw, secret, wildcardOnly, withAPIKeys).
		WithStatusSubresource(gw, wildcardOnly, withAPIKeys).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})
	r.LicenseParser = parser

	rec := r.Recorder.(*record.FakeRecorder)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// One Warning on the gateway (CEFallbackApplied turned True) and one on
	// wildcard-only (Accepted turned False); with-api-keys stays True.
	if n := eventsWithReason(rec, v1alpha1.ReasonEEFeaturesStripped); n != 2 {
		t.Errorf("EEFeaturesStripped events on the first reconcile = %d, want 2", n)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := eventsWithReason(rec, v1alpha1.ReasonEEFeaturesStripped); n != 0 {
		t.Errorf("EEFeaturesStripped events on an unchanged reconcile = %d, want 0", n)
	}
	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionCEFallbackApplied)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonEEFeaturesStripped ||
		!strings.Contains(cond.Message, "default/wildcard-only GET /files/*: wildcard endpoint") ||
		!strings.Contains(cond.Message, "default/with-api-keys GET /users: extra_config auth/api-keys") {
		t.Errorf("CEFallbackApplied = %+v, want True/%s listing both removed features", cond, v1alpha1.ReasonEEFeaturesStripped)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonEEFeaturesStripped ||
		got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Errorf("Ready = %+v, phase %s; want False/%s, Degraded", ready, got.Status.Phase, v1alpha1.ReasonEEFeaturesStripped)
	}
}

func TestGatewayReconcile_CEEditionDeploymentRunsWithoutTheOpenAPIExport(t *testing.T) {
	gw := reconciledGateway() // CE
	gw.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
		t.Fatalf("getting the Deployment: %v", err)
	}
	for _, ic := range dep.Spec.Template.Spec.InitContainers {
		if ic.Name == "openapi-export" {
			t.Fatal("a CE-edition gateway's Deployment runs the OpenAPI export, which the CE binary cannot run")
		}
	}
}

func TestGatewayReconcile_RejectedCEFallbackKeepsTheOpenAPIPieces(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	gw.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
	const config = `{"version":3,"name":"ee-only-until-stripped"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Status.ConfigEdition = v1alpha1.EditionEE
	c := fakeClientBuilder().WithObjects(gw, secret, legacyConfigMap(gw, config)).WithStatusSubresource(gw).Build()
	val := &recordingValidator{err: rejectedBy("ERROR testing the configuration file:\twildcards must be named")}
	r := newTestGatewayReconciler(c, renderOf(config), val)
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(dep.Spec.Template.Spec.InitContainers, func(c corev1.Container) bool {
		return c.Name == "openapi-export"
	}) {
		t.Error("the Deployment lost the OpenAPI export while the CE fallback render is rejected; " +
			"the EE pods keep serving, so the pod template must not change")
	}
	if cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions,
		v1alpha1.ConditionCEFallbackApplied); cond != nil {
		t.Errorf("CEFallbackApplied = %+v, want none: the applied config is still the EE one", cond)
	}
}

func TestReconcileCEFallbackCondition_NamesTheOpenAPIExport(t *testing.T) {
	stripped := []renderer.StrippedEEFeature{{Feature: "extra_config auth/api-keys"}}
	for _, tc := range []struct {
		name     string
		openapi  *v1alpha1.OpenAPIExportSpec
		stripped []renderer.StrippedEEFeature
		want     []string
		notWant  string
	}{
		{"nothing removed", nil, nil, []string{"the config uses no Enterprise-only features"}, "OpenAPI"},
		{"OpenAPI export disabled", &v1alpha1.OpenAPIExportSpec{Enabled: false}, nil,
			[]string{"the config uses no Enterprise-only features"}, "OpenAPI"},
		{"only the OpenAPI export", &v1alpha1.OpenAPIExportSpec{Enabled: true}, nil,
			[]string{"removed 1 Enterprise-only feature(s):\n" + openAPIFallbackNote}, "uses no Enterprise-only features"},
		{"features and the OpenAPI export", &v1alpha1.OpenAPIExportSpec{Enabled: true}, stripped,
			[]string{"removed 2 Enterprise-only feature(s):\n" + openAPIFallbackNote + "\ngateway: extra_config auth/api-keys"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, _, _ := licensedEEGateway(testNow.Add(-time.Minute), true)
			gw.Spec.OpenAPI = tc.openapi
			gw.Status.ConfigChecksum, gw.Status.ConfigEdition = "cs", v1alpha1.EditionCE
			r := newTestGatewayReconciler(fakeClientBuilder().Build(), &mockRenderer{}, &mockValidator{})

			r.reconcileCEFallbackCondition(gw,
				&renderer.RenderOutput{Checksum: "cs", StrippedEEFeatures: tc.stripped}, v1alpha1.EditionCE)

			cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionCEFallbackApplied)
			if cond == nil {
				t.Fatal("CEFallbackApplied is not set")
			}
			for _, w := range tc.want {
				if !strings.Contains(cond.Message, w) {
					t.Errorf("message %q does not contain %q", cond.Message, w)
				}
			}
			if tc.notWant != "" && strings.Contains(cond.Message, tc.notWant) {
				t.Errorf("message %q contains %q", cond.Message, tc.notWant)
			}
		})
	}
}

func TestGatewayReconcile_DocsOnlyFallbackKeepsEndpointsReady(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	gw.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
	generated := testEndpoint("generated", "/users")
	generated.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(
		`{"documentation/openapi":{"audience":["public"],"summary":"List users"}}`)}
	generated.Spec.ComponentSchemas = map[string]runtime.RawExtension{"User": {Raw: []byte(`{"type":"object"}`)}}
	c := fakeClientBuilder().WithObjects(gw, secret, generated).WithStatusSubresource(gw, generated).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	accepted := storedAccepted(t, c, client.ObjectKeyFromObject(generated))
	if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.Reason != v1alpha1.ReasonAccepted {
		t.Fatalf("Accepted = %+v, want True/%s: dropping docs changes nothing the endpoint serves",
			accepted, v1alpha1.ReasonAccepted)
	}
	resolved := metav1.Condition{Type: v1alpha1.ConditionResolvedRefs, Status: metav1.ConditionTrue,
		Reason: v1alpha1.ReasonRefsResolved, ObservedGeneration: accepted.ObservedGeneration}
	status, reason, _ := v1alpha1.EndpointReady([]metav1.Condition{resolved, *accepted})
	if status != metav1.ConditionTrue {
		t.Errorf("Ready = %s/%s, want True", status, reason)
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionCEFallbackApplied)
	if cond == nil || !strings.Contains(cond.Message, "gateway: extra_config documentation/openapi") ||
		!strings.Contains(cond.Message, openAPIFallbackNote) {
		t.Errorf("CEFallbackApplied = %+v, want it to say the docs and the OpenAPI export are off", cond)
	}
}

func TestEndpointAccepted_ConflictKeepsItsReasonAndNamesTheRemovedFeatures(t *testing.T) {
	gw := reconciledGateway()
	ep := testEndpoint("ep", "/users")
	key := client.ObjectKeyFromObject(ep)
	removed := []renderer.StrippedEEFeature{{Source: key, Method: "GET", Endpoint: "/users",
		Feature: "extra_config auth/api-keys"}}
	winner := types.NamespacedName{Namespace: "default", Name: "older"}
	for _, tc := range []struct {
		name       string
		twoEntries bool
		lost       []renderer.EntryConflict
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"every entry lost", false, []renderer.EntryConflict{{Endpoint: "/users", Method: "GET", Winner: winner}},
			metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict},
		{"some entries lost", true, []renderer.EntryConflict{{Endpoint: "/orders", Method: "GET", Winner: winner}},
			metav1.ConditionTrue, v1alpha1.ReasonPartiallyAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep := ep.DeepCopy()
			if tc.twoEntries {
				ep.Spec.Endpoints = append(ep.Spec.Endpoints, ep.Spec.Endpoints[0])
				ep.Spec.Endpoints[1].Endpoint = "/orders"
			}
			rv := renderVerdicts{
				conflicted: map[types.NamespacedName]struct{}{key: {}},
				lost:       map[types.NamespacedName][]renderer.EntryConflict{key: tc.lost},
				stripped:   map[types.NamespacedName][]renderer.StrippedEEFeature{key: removed},
			}

			cond := endpointAccepted(gw, ep, rv).condition

			if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason ||
				!strings.Contains(cond.Message, "CE fallback also removed:\ndefault/ep GET /users: extra_config auth/api-keys") {
				t.Errorf("Accepted = %+v, want %s/%s naming the removed feature", cond, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// manyStripped is n features of one endpoint, enough to pass the message cap.
func manyStripped(n int) []renderer.StrippedEEFeature {
	out := make([]renderer.StrippedEEFeature, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, renderer.StrippedEEFeature{
			Source: types.NamespacedName{Namespace: "default", Name: "big"}, Method: "GET",
			Endpoint: fmt.Sprintf("/route-%03d", i), Feature: "extra_config auth/api-keys",
		})
	}
	return out
}

// wantWholeEntries checks that a capped message keeps whole entries, one per
// line, and that its marker counts exactly the entries it dropped.
func wantWholeEntries(t *testing.T, msg string, features []renderer.StrippedEEFeature) {
	t.Helper()
	lines := strings.Split(msg, "\n")
	marker := lines[len(lines)-1]
	var dropped int
	if _, err := fmt.Sscanf(marker, "(output truncated, %d more lines)", &dropped); err != nil {
		t.Fatalf("last line %q is not the truncation marker", marker)
	}
	kept := lines[1 : len(lines)-1] // the first line is the header
	for i, line := range kept {
		if line != features[i].String() {
			t.Fatalf("line %d = %q, want the whole entry %q", i, line, features[i].String())
		}
	}
	if dropped != len(features)-len(kept) || dropped == 0 {
		t.Errorf("marker says %d dropped, but %d of %d entries were cut", dropped, len(features)-len(kept), len(features))
	}
}

func TestEEStripped_ATruncatedMessageKeepsWholeEntriesAndCountsTheRest(t *testing.T) {
	features := manyStripped(200)
	ep := testEndpoint("big", "/x")
	cond := &metav1.Condition{Status: metav1.ConditionTrue}

	eeStripped(cond, ep, features)

	wantWholeEntries(t, cond.Message, features)
}

func TestReconcileCEFallbackCondition_ATruncatedMessageKeepsTheOpenAPINoteAndWholeEntries(t *testing.T) {
	features := manyStripped(200)
	gw, _, _ := licensedEEGateway(testNow.Add(-time.Minute), true)
	gw.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
	gw.Status.ConfigChecksum, gw.Status.ConfigEdition = "cs", v1alpha1.EditionCE
	r := newTestGatewayReconciler(fakeClientBuilder().Build(), &mockRenderer{}, &mockValidator{})

	r.reconcileCEFallbackCondition(gw,
		&renderer.RenderOutput{Checksum: "cs", StrippedEEFeatures: features}, v1alpha1.EditionCE)

	msg := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionCEFallbackApplied).Message
	if !strings.Contains(msg, openAPIFallbackNote) {
		t.Errorf("the OpenAPI note was cut from %q", msg)
	}
	// The note is the first entry, so drop it before comparing with features.
	wantWholeEntries(t, strings.Replace(msg, openAPIFallbackNote+"\n", "", 1), features[:len(features)])
}

func TestEndpointAccepted_StrippedWildcardsAreNotCountedAsServed(t *testing.T) {
	gw := reconciledGateway()
	ep := testEndpoint("x", "/users")
	ep.Spec.Endpoints = append(ep.Spec.Endpoints, ep.Spec.Endpoints[0], ep.Spec.Endpoints[0])
	ep.Spec.Endpoints[1].Endpoint = "/files/*"
	ep.Spec.Endpoints[2].Endpoint = "/orders"
	key := client.ObjectKeyFromObject(ep)
	older := types.NamespacedName{Namespace: "default", Name: "older"}
	wildcard := renderer.StrippedEEFeature{Source: key, Method: "GET", Endpoint: "/files/*",
		Feature: renderer.FeatureWildcardEndpoint}
	for _, tc := range []struct {
		name       string
		entries    int
		wantStatus metav1.ConditionStatus
		wantReason string
		wantServed string
	}{
		{"nothing is left", 2, metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict, ""},
		{"one entry is left", 3, metav1.ConditionTrue, v1alpha1.ReasonPartiallyAccepted, "1 of 3 entries are served"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep := ep.DeepCopy()
			ep.Spec.Endpoints = ep.Spec.Endpoints[:tc.entries]
			rv := renderVerdicts{
				conflicted: map[types.NamespacedName]struct{}{key: {}},
				lost: map[types.NamespacedName][]renderer.EntryConflict{
					key: {{Endpoint: "/users", Method: "GET", Winner: older}}},
				stripped: map[types.NamespacedName][]renderer.StrippedEEFeature{key: {wildcard}},
			}

			cond := endpointAccepted(gw, ep, rv).condition

			if cond.Status != tc.wantStatus || cond.Reason != tc.wantReason ||
				!strings.Contains(cond.Message, tc.wantServed) {
				t.Errorf("Accepted = %+v, want %s/%s containing %q", cond, tc.wantStatus, tc.wantReason, tc.wantServed)
			}
		})
	}
}

func TestReconcileCEFallbackCondition_DescribesTheAppliedConfig(t *testing.T) {
	applied := func() *v1alpha1.KrakenDGateway {
		gw, _, _ := licensedEEGateway(testNow.Add(-time.Minute), true)
		gw.Status.ConfigChecksum, gw.Status.ConfigEdition = "fallback", v1alpha1.EditionCE
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionCEFallbackApplied, Status: metav1.ConditionTrue,
			Reason: v1alpha1.ReasonEEFeaturesStripped, Message: "what the applied render removed",
		})
		return gw
	}
	r := newTestGatewayReconciler(fakeClientBuilder().Build(), &mockRenderer{}, &mockValidator{})

	t.Run("kept while a newer render is pending", func(t *testing.T) {
		gw := applied()
		r.reconcileCEFallbackCondition(gw, &renderer.RenderOutput{Checksum: "newer"}, v1alpha1.EditionCE)
		cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionCEFallbackApplied)
		if cond == nil || cond.Message != "what the applied render removed" {
			t.Errorf("CEFallbackApplied = %+v, want it untouched: the applied config is still the fallback", cond)
		}
	})
	t.Run("removed once the applied config is not a fallback render", func(t *testing.T) {
		gw := applied()
		gw.Status.ConfigChecksum, gw.Status.ConfigEdition = "ee", v1alpha1.EditionEE
		r.reconcileCEFallbackCondition(gw, &renderer.RenderOutput{Checksum: "ee"}, v1alpha1.EditionEE)
		if cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionCEFallbackApplied); cond != nil {
			t.Errorf("CEFallbackApplied = %+v, want it removed", cond)
		}
	})
}

func TestGatewayReconcile_OpenAPIPiecesFollowTheAppliedRender(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	gw.Spec.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
	wildcardOnly, _ := fallbackEndpoints() // makes the EE and CE renders differ
	c := fakeClientBuilder().WithObjects(gw, secret, wildcardOnly).
		WithStatusSubresource(gw, wildcardOnly).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})
	r.LicenseParser = parser
	pieces := func() (export, serve bool) {
		var dep appsv1.Deployment
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
			t.Fatal(err)
		}
		spec := dep.Spec.Template.Spec
		return slices.ContainsFunc(spec.InitContainers, func(c corev1.Container) bool { return c.Name == "openapi-export" }),
			slices.ContainsFunc(spec.Containers, func(c corev1.Container) bool { return c.Name == "openapi-serve" })
	}

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if export, serve := pieces(); export || serve {
		t.Errorf("applied CE fallback: openapi-export %v, openapi-serve %v; want neither", export, serve)
	}
	if meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionCEFallbackApplied) == nil {
		t.Error("CEFallbackApplied is not set on the applied fallback render")
	}

	parser.info.NotAfter = testNow.Add(90 * 24 * time.Hour) // a renewed license
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if export, serve := pieces(); !export || !serve {
		t.Errorf("applied EE render: openapi-export %v, openapi-serve %v; want both", export, serve)
	}
	if cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions,
		v1alpha1.ConditionCEFallbackApplied); cond != nil {
		t.Errorf("CEFallbackApplied = %+v, want it removed once the EE render is applied", cond)
	}
}

func TestGatewayReconcile_MissingPluginConfigMapHoldsTheDeployment(t *testing.T) {
	gw := reconciledGateway()
	const config = `{"version":3,"name":"with-plugins"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionPluginsResolved)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonConfigMapNotFound ||
		!strings.Contains(cond.Message, "plugins-a") {
		t.Errorf("PluginsResolved = %+v, want False/%s naming plugins-a", cond, v1alpha1.ReasonConfigMapNotFound)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady); ready == nil ||
		ready.Reason != v1alpha1.ReasonConfigMapNotFound {
		t.Errorf("Ready = %+v, want reason %s", ready, v1alpha1.ReasonConfigMapNotFound)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); !apierrors.IsNotFound(err) {
		t.Fatalf("Deployment Get = %v; no pod template may mount a ConfigMap that does not exist", err)
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "plugins-a", Namespace: gw.Namespace},
		BinaryData: map[string][]byte{"auth.so": []byte("plugin")}}
	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions,
		v1alpha1.ConditionPluginsResolved); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("PluginsResolved = %+v, want True once the ConfigMap exists", cond)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
		t.Errorf("the Deployment must be created once the plugin ConfigMap exists: %v", err)
	}
}

func TestGatewayReconcile_HeldForPluginConfigMapReportsNoRollout(t *testing.T) {
	gw := reconciledGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"new"}`), &mockValidator{})
	rec := fakeRecorder()
	r.Recorder = rec

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing); condTrue(cond) {
		t.Errorf("Progressing = %+v, want no rollout reported while the Deployment is held", cond)
	}
	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonConfigDeployed) {
		t.Errorf("events = %q, want no ConfigDeployed while the Deployment is held", events)
	}
}

func TestGatewayReconcile_HeldForPluginConfigMapStillReconcilesTheRestOfTheInfrastructure(t *testing.T) {
	gw := reconciledGateway()
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: ptr.To(int32(2)), MaxReplicas: 4}
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	gw.Spec.License = &v1alpha1.LicenseConfig{ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{Enabled: true}}
	gw.Spec.Istio = &v1alpha1.IstioSpec{
		Enabled: true, Hosts: []string{"api.example.com"}, Gateways: []string{"istio-system/gw"},
	}
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	c := fakeClientBuilder().
		WithRESTMapper(optionalCRDMapper(dragonflyGVK, externalSecretGVK, virtualServiceGVK)).
		WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"held"}`), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &hpa); err != nil {
		t.Errorf("the HPA must still be reconciled while the Deployment is held: %v", err)
	}
	for gvk, name := range map[schema.GroupVersionKind]string{
		dragonflyGVK:      resources.DragonflyName(gw),
		externalSecretGVK: resources.ExternalSecretName(gw),
		virtualServiceGVK: gw.Name,
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: gw.Namespace, Name: name}, u); err != nil {
			t.Errorf("the %s must still be reconciled while the Deployment is held: %v", gvk.Kind, err)
		}
	}
}

func TestGatewayReconcile_PluginsResolvedIsAbsentWithoutConfigMapSources(t *testing.T) {
	gw := reconciledGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(`{"version":3,"name":"plugins"}`), &mockValidator{})
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionPluginsResolved) == nil {
		t.Fatal("PluginsResolved is not set while a ConfigMap plugin source is missing")
	}

	latest := getGateway(t, c, gw)
	latest.Spec.Plugins = nil
	if err := c.Update(context.Background(), latest); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions,
		v1alpha1.ConditionPluginsResolved); cond != nil {
		t.Errorf("PluginsResolved = %+v, want it removed once no ConfigMap plugin source remains", cond)
	}
}

func TestGatewayReconcile_HeldForPluginConfigMapReportsNoDeploymentUpdate(t *testing.T) {
	gw := servingGateway("applied", convergedImage)
	live := makeConvergedDeployment(gw, "applied")
	gw.Spec.Image = "img:v2"
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	c := fakeClientBuilder().WithObjects(gw, live).WithStatusSubresource(gw).Build()
	rend := &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "applied",
	}}
	r := newTestGatewayReconciler(c, rend, &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing); condTrue(cond) {
		t.Errorf("Progressing = %+v, want no rollout reported while the Deployment is held", cond)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
		t.Fatal(err)
	}
	if image := dep.Spec.Template.Spec.Containers[0].Image; image != convergedImage {
		t.Errorf("Deployment image = %s, want it left at %s while held", image, convergedImage)
	}
}

func TestGatewayReconcile_ReleasingThePluginHoldReportsTheDeferredConfigRolloutOnce(t *testing.T) {
	gw := servingGateway("A", convergedImage)
	live := makeConvergedDeployment(gw, "A")
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	var stale *appsv1.Deployment
	c := fakeClientBuilder().WithObjects(gw, live).WithStatusSubresource(gw).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if dep, ok := obj.(*appsv1.Deployment); ok {
				dep.Generation++ // the API server bumps it on a spec change
			}
			return c.Update(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption,
		) error {
			if dep, ok := obj.(*appsv1.Deployment); ok && stale != nil {
				stale.DeepCopyInto(dep)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	r := newTestGatewayReconciler(c, renderOutput("B"), &mockValidator{})
	rec := fakeRecorder()
	r.Recorder = rec

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile while held: %v", err)
	}
	if applied := getGateway(t, c, gw).Status.ConfigChecksum; applied != "B" {
		t.Fatalf("applied config = %q, want B applied while the Deployment is held", applied)
	}
	events := drainEvents(rec)

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "plugins-a", Namespace: gw.Namespace},
		BinaryData: map[string][]byte{"auth.so": []byte("plugin")}}
	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile releasing the hold: %v", err)
	}
	got := getGateway(t, c, gw)
	progressing := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue ||
		progressing.Reason != v1alpha1.ReasonConfigDeployed {
		t.Errorf("Progressing = %+v, want True/%s once the hold lifts and the applied config rolls out",
			progressing, v1alpha1.ReasonConfigDeployed)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady); ready == nil ||
		ready.Status != metav1.ConditionFalse || got.Status.Phase != v1alpha1.PhaseDeploying {
		t.Errorf("Ready = %+v, phase = %s, want False and %s while the new pods roll",
			ready, got.Status.Phase, v1alpha1.PhaseDeploying)
	}
	events = append(events, drainEvents(rec)...)

	var rolled appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &rolled); err != nil {
		t.Fatal(err)
	}
	stale = live.DeepCopy() // the cache still shows the Deployment from before the update
	stale.ResourceVersion = rolled.ResourceVersion
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile on a stale read: %v", err)
	}
	events = append(events, drainEvents(rec)...)
	n := 0
	for _, ev := range events {
		if strings.Contains(ev, v1alpha1.ReasonConfigDeployed) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("ConfigDeployed events = %d (%q), want exactly one, on the reconcile that lifts the hold", n, events)
	}
}

func TestGatewayReconcile_MissingPluginConfigMapIsNamedOnce(t *testing.T) {
	gw := reconciledGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "rate.so"}},
	}}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("new"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionPluginsResolved)
	if cond == nil {
		t.Fatal("PluginsResolved is not set")
	}
	if n := strings.Count(cond.Message, "plugins-a"); n != 1 {
		t.Errorf("PluginsResolved message names plugins-a %d times, want once: %q", n, cond.Message)
	}
}

func TestGatewayReconcile_ReleasingThePluginHoldWithAnUnchangedConfigReportsNoRollout(t *testing.T) {
	gw := servingGateway("A", convergedImage)
	live := makeConvergedDeployment(gw, "A")
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	c := fakeClientBuilder().WithObjects(gw, live).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("A"), &mockValidator{})
	rec := fakeRecorder()
	r.Recorder = rec

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile while held: %v", err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "plugins-a", Namespace: gw.Namespace},
		BinaryData: map[string][]byte{"auth.so": []byte("plugin")}}
	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile releasing the hold: %v", err)
	}
	got := getGateway(t, c, gw)
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionPluginsResolved); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Fatalf("PluginsResolved = %+v, want True: the hold must have lifted", cond)
	}
	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonConfigDeployed) {
		t.Errorf("events = %q, want no ConfigDeployed: the applied config is already deployed", events)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing); condTrue(cond) {
		t.Errorf("Progressing = %+v, want no rollout when the hold lifts on an unchanged config", cond)
	}
}

func TestGatewayReconcile_GatewayMetricsFollowTheSpec(t *testing.T) {
	gw := reconciledGateway()
	gw.Namespace = "metrics-follow"
	gw.Spec.Version = "2.12"
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	val := &countingValidator{}
	r := newTestGatewayReconciler(c, renderOutput("cs1"), val)
	t.Cleanup(func() { deleteGatewayMetrics(gw.Namespace, gw.Name) })

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(gatewayConfigValid.WithLabelValues(gw.Namespace, gw.Name)); got != 1 {
		t.Errorf("gateway_config_valid = %v after an applied config, want 1", got)
	}

	stored := getGateway(t, c, gw)
	stored.Spec.Version = "2.13"
	if err := c.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	r.Renderer = renderOutput("cs2")
	val.err = rejectedBy("- at '/endpoints/0/endpoint': bad")
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(gatewayConfigValid.WithLabelValues(gw.Namespace, gw.Name)); got != 0 {
		t.Errorf("gateway_config_valid = %v after a rejected config, want 0", got)
	}
	labels := prometheus.Labels{"namespace": gw.Namespace, "name": gw.Name}
	if n := gatewayInfo.DeletePartialMatch(labels); n != 1 {
		t.Errorf("gateway_info series for the gateway = %d, want 1 (the old version's series must go)", n)
	}
}
