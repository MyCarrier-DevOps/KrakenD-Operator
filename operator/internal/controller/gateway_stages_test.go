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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	c := fakeClientBuilder().WithObjects(gw, legacyConfigMap(gw, lastGood)).WithStatusSubresource(gw).Build()
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
