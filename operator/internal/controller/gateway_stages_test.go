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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestGatewayReconcile_RejectedRenderWithADeploymentWritesStatusOnce(t *testing.T) {
	gw := servingGateway("applied", "img:v1")
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
	gw := rollingGateway("applied", "img:v1")
	dep := makeConvergedDeployment(gw, "applied")
	dep.Spec.Template.Annotations[resources.ImageAnnotation] = "img:v2"
	dep.Spec.Template.Spec.Containers[0].Image = "img:v2"
	c := fakeClientBuilder().WithObjects(gw, dep).WithStatusSubresource(gw).Build()
	rend := &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "applied", DesiredImage: "img:v2",
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

// virtualServiceTestGVK is the Istio VirtualService kind the gateway creates.
var virtualServiceTestGVK = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "VirtualService"}

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
			c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(virtualServiceTestGVK)).
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
		JSON: []byte(config), Checksum: hash.SHA256Hex([]byte(config)), DesiredImage: "krakend:2.7.0",
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
	gw := servingGateway(hash.SHA256Hex([]byte(`{"version":3,"name":"gone"}`)), "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
	liveGW := servingGateway("applied", "img:v1")
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
	gw := servingGateway("applied", "img:v1")
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
