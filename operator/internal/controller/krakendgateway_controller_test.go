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
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// mockRenderer implements renderer.Renderer for testing.
type mockRenderer struct {
	output *renderer.RenderOutput
	err    error
}

func (m *mockRenderer) Render(_ renderer.RenderInput) (*renderer.RenderOutput, error) {
	return m.output, m.err
}

// mockValidator implements renderer.Validator for testing.
type mockValidator struct {
	validateErr error
}

func (m *mockValidator) Validate(_ context.Context, _ []byte, _ v1alpha1.Edition) error {
	return m.validateErr
}

func testGateway() *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
}

func TestGatewayReconcile_NotFound(t *testing.T) {
	c := fakeClientBuilder().Build()
	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  &mockRenderer{},
		Validator: &mockValidator{},
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for missing resource")
	}
}

func TestGatewayReconcile_FirstReconcileWritesOnlyTheDerivedStatus(t *testing.T) {
	gw := testGateway()
	gw.Generation = 1
	writes := 0
	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDGateway](&writes)).
		Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
	})

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})
	if err != nil {
		t.Fatal(err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("result = %+v, want none: the first reconcile runs the whole pipeline", result)
	}
	if writes != 1 {
		t.Errorf("gateway status writes = %d, want 1 (no separate Pending write)", writes)
	}
	stored := getGateway(t, c, gw)
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ConfigDeployed" ||
		stored.Status.Phase != v1alpha1.PhaseDeploying || stored.Status.ObservedGeneration != 1 {
		t.Errorf("Ready = %+v, phase %q, observedGeneration %d; want False/ConfigDeployed, Deploying, 1",
			ready, stored.Status.Phase, stored.Status.ObservedGeneration)
	}
}

func TestGatewayReconcile_FullPipeline(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api/v1/test",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:8080"}, URLPattern: "/test"},
					},
				},
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(gw, ep).
		Build()

	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:     []byte(`{"version":3}`),
			Checksum: "newchecksum",
		},
	}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  mockRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify gateway status
	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ConfigChecksum != "newchecksum" {
		t.Errorf("expected checksum newchecksum, got %s", updated.Status.ConfigChecksum)
	}
	if want := renderer.ResolveImage(gw, false); updated.Status.ActiveImage != want {
		t.Errorf("active image = %s, want %s", updated.Status.ActiveImage, want)
	}
	if updated.Status.EndpointCount != 1 {
		t.Errorf("expected endpoint count 1, got %d", updated.Status.EndpointCount)
	}

	// Verify owned resources created
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
		t.Fatalf("deployment not created: %v", err)
	}
	var svc corev1.Service
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}
	var sa corev1.ServiceAccount
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &sa); err != nil {
		t.Fatalf("serviceaccount not created: %v", err)
	}
	var cm corev1.ConfigMap
	getObject(t, c, gw, resources.ConfigMapName(gw, "newchecksum"), &cm)
	if cm.Data["krakend.json"] != `{"version":3}` {
		t.Errorf("unexpected configmap data: %s", cm.Data["krakend.json"])
	}
	var pdb policyv1.PodDisruptionBudget
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &pdb); err != nil {
		t.Fatalf("pdb not created: %v", err)
	}
}

func TestGatewayReconcile_ChecksumUnchanged(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhaseRunning
	gw.Status.ConfigChecksum = "samechecksum"
	gw.Status.ActiveImage = renderer.ResolveImage(gw, false)
	now := metav1.Now()
	gw.Status.Conditions = []metav1.Condition{
		{Type: "ConfigValid", Status: metav1.ConditionTrue, Reason: "ConfigApplied",
			Message: "Configuration passed validation and is applied", LastTransitionTime: now},
		{Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentAvailable",
			Message: "All replicas are available", LastTransitionTime: now},
		{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "RolloutComplete",
			Message: "Deployment rollout completed successfully", LastTransitionTime: now},
	}

	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:     []byte(`{"version":3}`),
			Checksum: "samechecksum",
		},
	}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  mockRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &updated); err != nil {
		t.Fatal(err)
	}
	// Should stay Running when checksum unchanged
	if updated.Status.Phase != v1alpha1.PhaseRunning {
		t.Errorf("expected Running, got %s", updated.Status.Phase)
	}
}

func TestGatewayReconcile_ValidationFailure(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending

	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:     []byte(`{"version":3}`),
			Checksum: "newchecksum",
		},
	}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		APIReader: c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  mockRend,
		Validator: &mockValidator{
			validateErr: &renderer.ValidationError{
				Output: "invalid config line 5",
				Err:    fmt.Errorf("exit code 1"),
			},
		},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.PhaseError {
		t.Errorf("expected Error, got %s", updated.Status.Phase)
	}
}

func TestGatewayReconcile_RenderError(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending

	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  &mockRenderer{err: fmt.Errorf("render boom")},
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err == nil {
		t.Fatal("expected error from render failure")
	}
}

func TestGatewayMapper_EndpointToGateway(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	r := &KrakenDGatewayReconciler{}
	requests := r.endpointToGateway(context.Background(), ep)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "gw1" {
		t.Errorf("expected gw1, got %s", requests[0].Name)
	}
}

func TestGatewayMapper_PolicyToGateways(t *testing.T) {
	ep1 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/a",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://a"}, URLPattern: "/", PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					},
				},
			},
		},
	}
	ep2 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw2"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/b",
					Method:   "POST",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://b"}, URLPattern: "/", PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					},
				},
			},
		},
	}
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
	}
	c := fakeClientBuilder().WithObjects(ep1, ep2).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme()}

	requests := r.policyToGateways(context.Background(), policy)
	if len(requests) != 2 {
		t.Fatalf("expected 2 gateway requests, got %d", len(requests))
	}
	names := map[string]bool{}
	for _, req := range requests {
		names[req.Name] = true
	}
	if !names["gw1"] || !names["gw2"] {
		t.Errorf("expected gw1 and gw2, got %v", names)
	}
}

func TestGatewayMapper_LicenseSecretToGateway_DirectRef(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionEE,
			License: &v1alpha1.LicenseConfig{
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-license"},
					Key:                  "key",
				},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-license", Namespace: "default"},
	}
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme()}

	requests := r.licenseSecretToGateway(context.Background(), secret)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "gw1" {
		t.Errorf("expected gw1, got %s", requests[0].Name)
	}
}

func TestGatewayMapper_LicenseSecretToGateway_ExternalSecret(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionEE,
			License: &v1alpha1.LicenseConfig{
				ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{Enabled: true},
			},
		},
	}
	// ExternalSecret convention: {gateway-name}-license
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1-license", Namespace: "default"},
	}
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme()}

	requests := r.licenseSecretToGateway(context.Background(), secret)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
}

func TestGatewayMapper_LicenseSecretToGateway_NoMatch(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
	}
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme()}

	requests := r.licenseSecretToGateway(context.Background(), secret)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests, got %d", len(requests))
	}
}

func TestGatewayMapper_PluginConfigMapToGateway_Match(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
			Plugins: &v1alpha1.PluginsSpec{
				Sources: []v1alpha1.PluginSource{
					{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "my-plugin"}},
				},
			},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-plugin", Namespace: "default"},
	}
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme()}

	requests := r.pluginConfigMapToGateway(context.Background(), cm)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "gw1" {
		t.Errorf("expected gw1, got %s", requests[0].Name)
	}
}

func TestGatewayMapper_PluginConfigMapToGateway_NoMatch(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
			Plugins: &v1alpha1.PluginsSpec{
				Sources: []v1alpha1.PluginSource{
					{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "other-plugin"}},
				},
			},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
	}
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme()}

	requests := r.pluginConfigMapToGateway(context.Background(), cm)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests, got %d", len(requests))
	}
}

func TestGatewayMapper_PluginConfigMapToGateway_NoPlugins(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "something", Namespace: "default"},
	}
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme()}

	requests := r.pluginConfigMapToGateway(context.Background(), cm)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests, got %d", len(requests))
	}
}

func TestGatewayReconcile_GathersPolicies(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://svc:8080"},
							URLPattern: "/",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "pol1"},
						},
					},
				},
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep, policy).
		WithStatusSubresource(gw, ep).
		Build()

	var capturedInput *renderer.RenderInput
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:     []byte(`{"version":3}`),
			Checksum: "newcs",
		},
	}
	// Wrap with capturing renderer
	capturingRend := &capturingRenderer{delegate: mockRend, captured: &capturedInput}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  capturingRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedInput == nil {
		t.Fatal("renderer was not called")
	}
	if len((*capturedInput).Policies) != 1 {
		t.Errorf("expected 1 policy in render input, got %d", len((*capturedInput).Policies))
	}
	if _, ok := (*capturedInput).Policies["default/pol1"]; !ok {
		t.Error("expected pol1 in policies map")
	}
	if len((*capturedInput).Endpoints) != 1 {
		t.Errorf("expected 1 endpoint in render input, got %d", len((*capturedInput).Endpoints))
	}
}

// capturingRenderer wraps a renderer and captures the input.
type capturingRenderer struct {
	delegate renderer.Renderer
	captured **renderer.RenderInput
}

func (cr *capturingRenderer) Render(input renderer.RenderInput) (*renderer.RenderOutput, error) {
	*cr.captured = &input
	return cr.delegate.Render(input)
}

func TestGatewayReconcile_WithPluginConfigMaps(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugin-cm", Key: "plugin.so"}},
		},
	}
	pluginCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "plugin-cm", Namespace: "default"},
		Data:       map[string]string{"plugin.so": "binary-data"},
	}
	c := fakeClientBuilder().
		WithObjects(gw, pluginCM).
		WithStatusSubresource(gw).
		Build()

	var capturedInput *renderer.RenderInput
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON: []byte(`{}`), Checksum: "cs",
		},
	}
	capturingRend := &capturingRenderer{delegate: mockRend, captured: &capturedInput}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  capturingRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedInput == nil {
		t.Fatal("renderer was not called")
	}
	if len((*capturedInput).PluginConfigMaps) != 1 {
		t.Errorf("expected 1 plugin configmap, got %d", len((*capturedInput).PluginConfigMaps))
	}
}

func TestGatewayReconcile_WithHPA(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		MaxReplicas: 10,
	}

	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON: []byte(`{}`), Checksum: "cs",
		},
	}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  mockRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify HPA was created
	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &hpa); err != nil {
		t.Fatalf("HPA not created: %v", err)
	}
	if hpa.Spec.MaxReplicas != 10 {
		t.Errorf("expected maxReplicas 10, got %d", hpa.Spec.MaxReplicas)
	}
}

func TestGatewayReconcile_MissingPolicySkipped(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://svc:8080"},
							URLPattern: "/",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "nonexistent-policy"},
						},
					},
				},
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(gw, ep).
		Build()

	var capturedInput *renderer.RenderInput
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON: []byte(`{}`), Checksum: "cs",
		},
	}
	capturingRend := &capturingRenderer{delegate: mockRend, captured: &capturedInput}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  capturingRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Missing policy should not be in the map (renderer handles the invalidity)
	if capturedInput == nil {
		t.Fatal("renderer was not called")
	}
	if len((*capturedInput).Policies) != 0 {
		t.Errorf("expected 0 policies for missing ref, got %d", len((*capturedInput).Policies))
	}
}

func TestDetectDragonflyState_NotEnabled(t *testing.T) {
	gw := testGateway()
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
	}

	state := r.detectDragonflyState(context.Background(), gw)
	if state != nil {
		t.Error("expected nil state when Dragonfly is not enabled")
	}
}

func TestDetectDragonflyState_CRDNotInstalled(t *testing.T) {
	gw := testGateway()
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
	}

	state := r.detectDragonflyState(context.Background(), gw)
	if state != nil {
		t.Error("expected nil state when Dragonfly CRD is not installed")
	}
}

func TestDetectDragonflyState_Disabled(t *testing.T) {
	gw := testGateway()
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: false}
	c := fakeClientBuilder().Build()
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
	}

	state := r.detectDragonflyState(context.Background(), gw)
	if state != nil {
		t.Error("expected nil state when Dragonfly is disabled")
	}
}

func TestGatewayReconcile_WithDragonflyEnabled(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhaseRunning
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	var capturedInput *renderer.RenderInput
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON: []byte(`{}`), Checksum: "cs",
		},
	}
	capturingRend := &capturingRenderer{delegate: mockRend, captured: &capturedInput}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  capturingRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedInput == nil {
		t.Fatal("renderer was not called")
	}
	if (*capturedInput).Dragonfly != nil {
		t.Error("expected nil DragonflyState when CRD is not installed")
	}
}

func TestInspectDeploymentStatus_ProgressDeadlineExceeded(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhaseDeploying

	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			Replicas:          3,
			ReadyReplicas:     1,
			UpdatedReplicas:   2,
			AvailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentProgressing,
					Status: corev1.ConditionFalse,
					Reason: "ProgressDeadlineExceeded",
				},
			},
		},
	}

	c := fakeClientBuilder().
		WithObjects(gw, dep).
		WithStatusSubresource(gw).
		Build()

	r := &KrakenDGatewayReconciler{
		Client:   c,
		Scheme:   testScheme(),
		Recorder: fakeRecorder(),
	}

	r.inspectDeploymentStatus(context.Background(), gw, convergedInputs(gw.Status.ConfigChecksum))

	if a := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionAvailable); a == nil ||
		a.Status != metav1.ConditionFalse || a.Reason != v1alpha1.ReasonRolloutFailed {
		t.Errorf("Available = %+v, want False/RolloutFailed", a)
	}
	if gw.Status.Replicas != 3 {
		t.Errorf("expected Replicas=3, got %d", gw.Status.Replicas)
	}
	if gw.Status.ReadyReplicas != 1 {
		t.Errorf("expected ReadyReplicas=1, got %d", gw.Status.ReadyReplicas)
	}
}

func TestInspectDeploymentStatus_RolloutConverged(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhaseDeploying

	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			Replicas:          3,
			ReadyReplicas:     3,
			UpdatedReplicas:   3,
			AvailableReplicas: 3,
			Conditions: []appsv1.DeploymentCondition{
				{
					Type:   appsv1.DeploymentProgressing,
					Status: corev1.ConditionTrue,
					Reason: "NewReplicaSetAvailable",
				},
			},
		},
	}

	c := fakeClientBuilder().
		WithObjects(gw, dep).
		WithStatusSubresource(gw).
		Build()

	r := &KrakenDGatewayReconciler{
		Client:   c,
		Scheme:   testScheme(),
		Recorder: fakeRecorder(),
	}

	r.inspectDeploymentStatus(context.Background(), gw, convergedInputs(gw.Status.ConfigChecksum))

	if gw.Status.Replicas != 3 {
		t.Errorf("expected Replicas=3, got %d", gw.Status.Replicas)
	}
	if gw.Status.ReadyReplicas != 3 {
		t.Errorf("expected ReadyReplicas=3, got %d", gw.Status.ReadyReplicas)
	}
}

func TestInspectDeploymentStatus_DeploymentNotFound(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhaseDeploying
	gw.Status.Replicas = 0
	gw.Status.ReadyReplicas = 0

	c := fakeClientBuilder().Build()

	r := &KrakenDGatewayReconciler{
		Client:   c,
		Scheme:   testScheme(),
		Recorder: fakeRecorder(),
	}

	r.inspectDeploymentStatus(context.Background(), gw, convergedInputs(gw.Status.ConfigChecksum))

	if gw.Status.Replicas != 0 {
		t.Errorf("expected Replicas=0 (unchanged), got %d", gw.Status.Replicas)
	}
	if gw.Status.Phase != v1alpha1.PhaseDeploying {
		t.Errorf("expected phase unchanged at Deploying, got %s", gw.Status.Phase)
	}
}

func TestGatewayReconcile_CrossNamespaceEndpoints(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending

	// Endpoint in same namespace (implicit gatewayRef.namespace)
	epSame := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-same", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/same", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:8080"}, URLPattern: "/same"},
				}},
			},
		},
	}
	// Endpoint in different namespace with explicit gatewayRef.namespace
	epCross := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-cross", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw", Namespace: "default"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/cross", Method: "POST", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://other:8080"}, URLPattern: "/cross"},
				}},
			},
		},
	}

	c := fakeClientBuilder().
		WithObjects(gw, epSame, epCross).
		WithStatusSubresource(gw, epSame, epCross).
		Build()

	var capturedInput *renderer.RenderInput
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:     []byte(`{"version":3}`),
			Checksum: "crossns",
		},
	}
	capturingRend := &capturingRenderer{delegate: mockRend, captured: &capturedInput}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  capturingRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedInput == nil {
		t.Fatal("renderer was not called")
	}
	if len((*capturedInput).Endpoints) != 2 {
		t.Errorf("expected 2 endpoints (same-ns + cross-ns), got %d", len((*capturedInput).Endpoints))
	}
}

func TestGatewayReconcile_CrossNamespacePolicies(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending

	// Endpoint in different namespace referencing a policy in its own namespace
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "cross-pol", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 50},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw", Namespace: "default"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{
						Host:       []string{"http://svc:8080"},
						URLPattern: "/",
						PolicyRef:  &v1alpha1.PolicyRef{Name: "cross-pol"},
					},
				}},
			},
		},
	}

	c := fakeClientBuilder().
		WithObjects(gw, ep, policy).
		WithStatusSubresource(gw, ep).
		Build()

	var capturedInput *renderer.RenderInput
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:     []byte(`{"version":3}`),
			Checksum: "crossnspol",
		},
	}
	capturingRend := &capturingRenderer{delegate: mockRend, captured: &capturedInput}

	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  capturingRend,
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedInput == nil {
		t.Fatal("renderer was not called")
	}
	if len((*capturedInput).Policies) != 1 {
		t.Errorf("expected 1 policy from cross-ns endpoint, got %d", len((*capturedInput).Policies))
	}
	if _, ok := (*capturedInput).Policies["app-ns/cross-pol"]; !ok {
		t.Error("expected cross-pol in policies map")
	}
}

func TestGatewayMapper_EndpointToGatewayCrossNamespace(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1", Namespace: "operator-ns"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	r := &KrakenDGatewayReconciler{}
	requests := r.endpointToGateway(context.Background(), ep)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "gw1" {
		t.Errorf("expected gw1, got %s", requests[0].Name)
	}
	if requests[0].Namespace != "operator-ns" {
		t.Errorf("expected namespace operator-ns, got %s", requests[0].Namespace)
	}
}

func TestGatewayReconcile_AutoscaledReplicasAreNotReset(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhaseRunning
	gw.Spec.Replicas = ptr.To(int32(2))
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: ptr.To(int32(2)), MaxReplicas: 10}
	scaled := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(7))}, // chosen by the HPA
	}
	c := fakeClientBuilder().WithObjects(gw, scaled).WithStatusSubresource(gw).Build()
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: &mockRenderer{output: &renderer.RenderOutput{
			JSON: []byte(`{"version":3}`), Checksum: "cs",
		}},
		Validator: &mockValidator{},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
		t.Fatal(err)
	}
	if got := ptr.Deref(dep.Spec.Replicas, -1); got != 7 {
		t.Errorf("replicas = %d, want the HPA's 7 kept", got)
	}
}

// reconciledGateway is testGateway as stored after an earlier reconcile.
// These tests do not rely on an initial Pending write, which the controller
// no longer makes: the stored phase only guards against that write if it
// were still there.
func reconciledGateway() *v1alpha1.KrakenDGateway {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending
	return gw
}

// gatewayEndpoint returns an empty endpoint of test-gw with the given name
// and generation.
func gatewayEndpoint(name string, generation int64) *v1alpha1.KrakenDEndpoint {
	return &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: generation},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
}

// acceptanceReconciler returns a gateway reconciler whose renderer returns
// output and whose validator passes.
func acceptanceReconciler(
	c client.Client, rec record.EventRecorder, output *renderer.RenderOutput,
) *KrakenDGatewayReconciler {
	return &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: rec,
		Renderer: &mockRenderer{output: output}, Validator: &mockValidator{}, APIReader: c,
	}
}

// storedAccepted returns the Accepted condition of the stored endpoint key.
func storedAccepted(t *testing.T, c client.Client, key types.NamespacedName) *metav1.Condition {
	t.Helper()
	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), key, &ep); err != nil {
		t.Fatalf("getting endpoint %s: %v", key, err)
	}
	return meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted)
}

func TestGatewayReconcile_WritesAcceptedOnEveryEndpoint(t *testing.T) {
	gw := reconciledGateway()
	included := gatewayEndpoint("ep-included", 3)
	conflicted := gatewayEndpoint("ep-conflicted", 2)
	unresolved := gatewayEndpoint("ep-unresolved", 1)
	// An earlier render accepted ep-unresolved; its policy has since been deleted.
	unresolved.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonAccepted,
		Message: "Included in the configuration of gateway default/test-gw", ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	c := fakeClientBuilder().
		WithObjects(gw, included, conflicted, unresolved).
		WithStatusSubresource(gw, included, conflicted, unresolved).
		Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
		ConflictedEndpoints: []types.NamespacedName{client.ObjectKeyFromObject(conflicted)},
		InvalidEndpoints:    []types.NamespacedName{client.ObjectKeyFromObject(unresolved)},
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := storedAccepted(t, c, client.ObjectKeyFromObject(included))
	if got == nil || got.Status != metav1.ConditionTrue || got.Reason != "Accepted" || got.ObservedGeneration != 3 {
		t.Errorf("included endpoint: Accepted = %+v, want True/Accepted at generation 3", got)
	}
	got = storedAccepted(t, c, client.ObjectKeyFromObject(conflicted))
	if got == nil || got.Status != metav1.ConditionFalse || got.Reason != "EndpointConflict" || got.ObservedGeneration != 2 {
		t.Errorf("conflicted endpoint: Accepted = %+v, want False/EndpointConflict at generation 2", got)
	}
	if got = storedAccepted(t, c, client.ObjectKeyFromObject(unresolved)); got != nil {
		t.Errorf("endpoint with a missing policy: Accepted = %+v, want no Accepted condition", got)
	}
	for _, ep := range []*v1alpha1.KrakenDEndpoint{included, conflicted, unresolved} {
		var stored v1alpha1.KrakenDEndpoint
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &stored); err != nil {
			t.Fatal(err)
		}
		if stored.Status.Phase != "" ||
			meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAvailable) != nil {
			t.Errorf("%s: the gateway wrote phase %q or Available; both belong to the endpoint controller",
				ep.Name, stored.Status.Phase)
		}
	}
}

func TestGatewayReconcile_AcceptedWrittenOnlyOnChange(t *testing.T) {
	gw := reconciledGateway()
	included := gatewayEndpoint("ep-included", 1)
	conflicted := gatewayEndpoint("ep-conflicted", 1)
	writes := 0
	c := fakeClientBuilder().
		WithObjects(gw, included, conflicted).
		WithStatusSubresource(gw, included, conflicted).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDEndpoint](&writes)).
		Build()
	rec := fakeRecorder()
	r := acceptanceReconciler(c, rec, &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
		ConflictedEndpoints: []types.NamespacedName{client.ObjectKeyFromObject(conflicted)},
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Errorf("first reconcile: endpoint status writes = %d, want 2", writes)
	}
	events := drainEvents(rec)
	conflicts := 0
	for _, ev := range events {
		if strings.HasPrefix(ev, "Warning EndpointConflict ") {
			conflicts++
		}
	}
	if conflicts != 1 || hasEventReason(events, v1alpha1.ReasonAccepted) {
		t.Errorf("first reconcile events = %q, want one EndpointConflict Warning and no Accepted event", events)
	}

	writes = 0
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Errorf("unchanged verdicts: endpoint status writes = %d, want 0", writes)
	}
	if ev := drainEvents(rec); hasEventReason(ev, v1alpha1.ReasonEndpointConflict) {
		t.Errorf("unchanged verdicts: events = %q, want no EndpointConflict", ev)
	}
}

func TestGatewayReconcile_AcceptedEventWhenConflictResolves(t *testing.T) {
	gw := reconciledGateway()
	ep := gatewayEndpoint("ep-b", 2)
	ep.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonEndpointConflict,
		Message: "conflict", ObservedGeneration: 2, LastTransitionTime: metav1.Now(),
	}}
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(gw, ep).Build()
	rec := fakeRecorder()
	r := acceptanceReconciler(c, rec, &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	if got := storedAccepted(t, c, client.ObjectKeyFromObject(ep)); got == nil || got.Status != metav1.ConditionTrue {
		t.Fatalf("Accepted = %+v, want True once the conflict is gone", got)
	}
	want := "Normal Accepted Included in the configuration of gateway default/test-gw"
	if events := drainEvents(rec); !slices.Contains(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
}

func TestGatewayReconcile_AcceptedNotWrittenWhenValidationFails(t *testing.T) {
	gw := reconciledGateway()
	conflicted := gatewayEndpoint("ep-conflicted", 1)
	writes := 0
	c := fakeClientBuilder().
		WithObjects(gw, conflicted).
		WithStatusSubresource(gw, conflicted).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDEndpoint](&writes)).
		Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs-rejected",
		ConflictedEndpoints: []types.NamespacedName{client.ObjectKeyFromObject(conflicted)},
	})
	// Build the failing validator the same way TestGatewayReconcile_ValidationFailure
	// does: a *renderer.ValidationError is an "invalid config" verdict.
	r.Validator = &mockValidator{validateErr: &renderer.ValidationError{
		Output: "invalid config line 5", Err: fmt.Errorf("exit code 1"),
	}}

	// Other tests cover the gateway's own status on a rejection; only the endpoint side is checked here.
	_ = reconcileGateway(t, r, gw)

	if writes != 0 {
		t.Errorf("endpoint status writes after a rejected render = %d, want 0", writes)
	}
	if got := storedAccepted(t, c, client.ObjectKeyFromObject(conflicted)); got != nil {
		t.Errorf("Accepted = %+v after a rejected render, want none", got)
	}
}

// renderFunc adapts a function to renderer.Renderer.
type renderFunc func(renderer.RenderInput) (*renderer.RenderOutput, error)

func (f renderFunc) Render(in renderer.RenderInput) (*renderer.RenderOutput, error) { return f(in) }

func TestGatewayReconcile_AcceptedSkipsEndpointReplacedSinceRender(t *testing.T) {
	gw := reconciledGateway()
	ep := gatewayEndpoint("ep-a", 1)
	ep.UID = "old-uid"
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(gw, ep).Build()
	key := client.ObjectKeyFromObject(ep)
	r := acceptanceReconciler(c, fakeRecorder(), nil)
	// While the gateway renders, the endpoint is deleted and created again.
	r.Renderer = renderFunc(func(renderer.RenderInput) (*renderer.RenderOutput, error) {
		if err := c.Delete(context.Background(), ep.DeepCopy()); err != nil {
			return nil, err
		}
		replacement := gatewayEndpoint("ep-a", 1)
		replacement.UID = "new-uid"
		if err := c.Create(context.Background(), replacement); err != nil {
			return nil, err
		}
		return &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"}, nil
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	if got := storedAccepted(t, c, key); got != nil {
		t.Errorf("replacement endpoint: Accepted = %+v, want none (the render saw the object it replaced)", got)
	}
}

func TestGatewayReconcile_AcceptedRecordsRenderedGeneration(t *testing.T) {
	gw := reconciledGateway()
	ep := gatewayEndpoint("ep-a", 1)
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(gw, ep).Build()
	key := client.ObjectKeyFromObject(ep)
	r := acceptanceReconciler(c, fakeRecorder(), nil)
	// The endpoint's spec changes while the gateway renders generation 1.
	r.Renderer = renderFunc(func(renderer.RenderInput) (*renderer.RenderOutput, error) {
		var stored v1alpha1.KrakenDEndpoint
		if err := c.Get(context.Background(), key, &stored); err != nil {
			return nil, err
		}
		stored.Generation = 2
		if err := c.Update(context.Background(), &stored); err != nil {
			return nil, err
		}
		return &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"}, nil
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	if got := storedAccepted(t, c, key); got == nil || got.ObservedGeneration != 1 {
		t.Errorf("Accepted = %+v, want observedGeneration 1: generation 2 was never rendered", got)
	}
}

func TestGatewayReconcile_AcceptedRetriesAfterConflictWithoutClobbering(t *testing.T) {
	gw := reconciledGateway()
	ep := gatewayEndpoint("ep-a", 1)
	endpointGets, patches := 0, 0
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(gw, ep).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, k client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if err := cl.Get(ctx, k, obj, opts...); err != nil {
					return err
				}
				stored, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				endpointGets++
				if endpointGets > 1 {
					return nil
				}
				// The endpoint controller writes ResolvedRefs after the gateway's read.
				fresh := stored.DeepCopy()
				meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
					Type: v1alpha1.ConditionResolvedRefs, Status: metav1.ConditionTrue,
					Reason: v1alpha1.ReasonRefsResolved, Message: "resolved", ObservedGeneration: 1,
				})
				return cl.Status().Update(ctx, fresh)
			},
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
					patches++
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patches != 2 {
		t.Errorf("endpoint status patches = %d, want 2: one rejected as stale, one retried", patches)
	}
	var stored v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &stored); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionResolvedRefs) == nil {
		t.Error("the gateway's write removed the endpoint controller's ResolvedRefs")
	}
	if a := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAccepted); a == nil ||
		a.Status != metav1.ConditionTrue {
		t.Errorf("Accepted = %+v, want True after the retry", a)
	}
}

func TestGatewayReconcile_EndpointStatusFailureDoesNotBlockOwnedResources(t *testing.T) {
	gw := reconciledGateway()
	epA := gatewayEndpoint("ep-a", 1)
	epB := gatewayEndpoint("ep-b", 1)
	failing := true
	c := fakeClientBuilder().
		WithObjects(gw, epA, epB).
		WithStatusSubresource(gw, epA, epB).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok && failing && obj.GetName() == "ep-a" {
					return apierrors.NewInternalError(fmt.Errorf("etcd timeout"))
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
	})

	err := reconcileGateway(t, r, gw)
	if err == nil || !strings.Contains(err.Error(), "ep-a") {
		t.Fatalf("Reconcile error = %v, want one naming ep-a so the reconcile is retried", err)
	}
	if got := storedAccepted(t, c, client.ObjectKeyFromObject(epB)); got == nil || got.Status != metav1.ConditionTrue {
		t.Errorf("ep-b: Accepted = %+v, want True: one failing endpoint must not stop the others", got)
	}
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &dep); err != nil {
		t.Errorf("Deployment not reconciled after an endpoint status failure: %v", err)
	}
	if got := getGateway(t, c, gw); got.Status.ConfigChecksum != "cs1" {
		t.Errorf("gateway status.configChecksum = %q, want cs1 (the gateway status write must still happen)",
			got.Status.ConfigChecksum)
	}

	// The retry finds the config already applied and still writes the verdict.
	failing = false
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("retry after the failure cleared: %v", err)
	}
	if got := storedAccepted(t, c, client.ObjectKeyFromObject(epA)); got == nil || got.Status != metav1.ConditionTrue {
		t.Errorf("ep-a after the retry: Accepted = %+v, want True", got)
	}
}

func TestGatewayReconcile_AcceptedWrittenWhenConfigUnchangedSinceApplied(t *testing.T) {
	gw := reconciledGateway()
	gw.Status.ConfigChecksum = "cs1"
	// An endpoint written by an earlier release: a phase and Available, no Accepted.
	ep := gatewayEndpoint("ep-a", 4)
	ep.Status.Phase = v1alpha1.EndpointPhaseActive
	ep.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: "ReferencesValid",
		Message: "ok", ObservedGeneration: 4, LastTransitionTime: metav1.Now(),
	}}
	c := fakeClientBuilder().WithObjects(gw, ep).WithStatusSubresource(gw, ep).Build()
	rec := fakeRecorder()
	r := acceptanceReconciler(c, rec, &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	got := storedAccepted(t, c, client.ObjectKeyFromObject(ep))
	if got == nil || got.Status != metav1.ConditionTrue || got.ObservedGeneration != 4 {
		t.Errorf("Accepted = %+v, want True at generation 4 for a config unchanged since it was applied", got)
	}
	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonConfigDeployed) {
		t.Errorf("events = %q, want no ConfigDeployed: the config did not change", events)
	}
}

func TestGatewayReadinessFor(t *testing.T) {
	c := func(typ string, status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: status, Reason: reason, Message: reason + " message"}
	}
	valid := c("ConfigValid", metav1.ConditionTrue, "ConfigApplied")
	available := c("Available", metav1.ConditionTrue, "DeploymentAvailable")
	settled := c("Progressing", metav1.ConditionFalse, "RolloutComplete")
	expired := c("LicenseExpired", metav1.ConditionTrue, "LicenseExpired")
	tests := []struct {
		name       string
		conds      []metav1.Condition
		wantStatus metav1.ConditionStatus
		wantReason string
		wantPhase  v1alpha1.GatewayPhase
	}{
		{"nothing observed yet", nil, metav1.ConditionUnknown, "Pending", v1alpha1.PhasePending},
		{"ready", []metav1.Condition{valid, available, settled},
			metav1.ConditionTrue, "Ready", v1alpha1.PhaseRunning},
		{"configuration rejected",
			[]metav1.Condition{c("ConfigValid", metav1.ConditionFalse, "ConfigValidationFailed"), available, settled},
			metav1.ConditionFalse, "ConfigValidationFailed", v1alpha1.PhaseError},
		{"license expired without fallback", []metav1.Condition{valid, available, settled, expired},
			metav1.ConditionFalse, "LicenseExpiredNoFallback", v1alpha1.PhaseError},
		{"rollout failed",
			[]metav1.Condition{valid, c("Available", metav1.ConditionFalse, "RolloutFailed"),
				c("Progressing", metav1.ConditionFalse, "RolloutFailed")},
			metav1.ConditionFalse, "RolloutFailed", v1alpha1.PhaseError},
		{"CE fallback",
			[]metav1.Condition{valid, available, settled, expired,
				c("LicenseDegraded", metav1.ConditionTrue, "LicenseFallbackCE")},
			metav1.ConditionFalse, "LicenseFallbackCE", v1alpha1.PhaseDegraded},
		{"CE fallback applied",
			[]metav1.Condition{valid, available, settled,
				c("LicenseDegraded", metav1.ConditionTrue, "LicenseFallbackCE"),
				c("CEFallbackApplied", metav1.ConditionTrue, "EEFeaturesStripped")},
			metav1.ConditionFalse, "EEFeaturesStripped", v1alpha1.PhaseDegraded},
		{"rolling out", []metav1.Condition{valid, available, c("Progressing", metav1.ConditionTrue, "ConfigDeployed")},
			metav1.ConditionFalse, "ConfigDeployed", v1alpha1.PhaseDeploying},
		{"plugin ConfigMap missing", []metav1.Condition{
			valid, available, settled,
			c("PluginsResolved", metav1.ConditionFalse, "ConfigMapNotFound"),
		}, metav1.ConditionFalse, "ConfigMapNotFound", v1alpha1.PhaseError},
		{"deployment not available yet", []metav1.Condition{valid, settled},
			metav1.ConditionFalse, "AwaitingAvailability", v1alpha1.PhaseDeploying},
		{"validator unavailable while serving",
			[]metav1.Condition{c("ConfigValid", metav1.ConditionUnknown, "ValidatorUnavailable"), available, settled},
			metav1.ConditionUnknown, "ValidatorUnavailable", v1alpha1.PhaseRunning},
		{"validator unavailable during a rollout",
			[]metav1.Condition{c("ConfigValid", metav1.ConditionUnknown, "ValidatorUnavailable"), available,
				c("Progressing", metav1.ConditionTrue, "ConfigDeployed")},
			metav1.ConditionUnknown, "ValidatorUnavailable", v1alpha1.PhaseDeploying},
		{"validator unavailable before anything was deployed",
			[]metav1.Condition{c("ConfigValid", metav1.ConditionUnknown, "ValidatorUnavailable")},
			metav1.ConditionUnknown, "ValidatorUnavailable", v1alpha1.PhasePending},
		{"validator unavailable and the deployment not available",
			[]metav1.Condition{c("ConfigValid", metav1.ConditionUnknown, "ValidatorUnavailable"), settled},
			metav1.ConditionUnknown, "ValidatorUnavailable", v1alpha1.PhaseDeploying},
		{"a failed rollout outranks an unavailable validator",
			[]metav1.Condition{c("ConfigValid", metav1.ConditionUnknown, "ValidatorUnavailable"),
				c("Available", metav1.ConditionFalse, "RolloutFailed")},
			metav1.ConditionFalse, "RolloutFailed", v1alpha1.PhaseError},
		{"ready after license recovery",
			[]metav1.Condition{valid, available, settled,
				c("LicenseExpired", metav1.ConditionFalse, "LicenseRestored"),
				c("LicenseDegraded", metav1.ConditionFalse, "LicenseRestored")},
			metav1.ConditionTrue, "Ready", v1alpha1.PhaseRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gatewayReadinessFor(tt.conds)
			if got.status != tt.wantStatus || got.reason != tt.wantReason || got.phase != tt.wantPhase {
				t.Errorf("gatewayReadinessFor() = %+v, want %s/%s phase %s",
					got, tt.wantStatus, tt.wantReason, tt.wantPhase)
			}
		})
	}
}

func TestGatewayReconcile_RecoveredRolloutClearsError(t *testing.T) {
	gw := testGateway()
	gw.Generation = 1
	now := metav1.Now()
	gw.Status = v1alpha1.KrakenDGatewayStatus{
		Phase: v1alpha1.PhaseError, ConfigChecksum: "cs1", ActiveImage: convergedImage,
		Conditions: []metav1.Condition{
			{Type: "ConfigValid", Status: metav1.ConditionTrue, Reason: "ConfigApplied",
				Message: "Configuration passed validation and is applied", ObservedGeneration: 1, LastTransitionTime: now},
			{Type: "Available", Status: metav1.ConditionFalse, Reason: "RolloutFailed",
				Message: "Deployment exceeded its progress deadline", ObservedGeneration: 1, LastTransitionTime: now},
			{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "RolloutFailed",
				Message: "Deployment exceeded its progress deadline", ObservedGeneration: 1, LastTransitionTime: now},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, makeConvergedDeployment(gw, "")).
		WithStatusSubresource(gw).
		Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	stored := getGateway(t, c, gw)
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || stored.Status.Phase != v1alpha1.PhaseRunning {
		t.Errorf("after the rollout recovered: Ready = %+v, phase %q; want True and Running", ready, stored.Status.Phase)
	}
}

func TestGatewayReconcile_RevertToAppliedConfigClearsRejection(t *testing.T) {
	gw := testGateway()
	gw.Generation = 3
	now := metav1.Now()
	gw.Status = v1alpha1.KrakenDGatewayStatus{
		Phase: v1alpha1.PhaseError, ConfigChecksum: "good", ActiveImage: convergedImage, ObservedGeneration: 2,
		Conditions: []metav1.Condition{
			{Type: "ConfigValid", Status: metav1.ConditionFalse, Reason: "ConfigValidationFailed",
				Message: "bad config", ObservedGeneration: 2, LastTransitionTime: now},
			{Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentAvailable",
				Message: "All replicas are available", ObservedGeneration: 2, LastTransitionTime: now},
			{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "RolloutComplete",
				Message: "Deployment rollout completed successfully", ObservedGeneration: 2, LastTransitionTime: now},
		},
	}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	// The spec was reverted: the render equals the configuration that is still applied.
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "good",
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	stored := getGateway(t, c, gw)
	cv := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionConfigValid)
	if cv == nil || cv.Status != metav1.ConditionTrue || cv.Reason != "ConfigApplied" {
		t.Errorf("ConfigValid = %+v, want True/ConfigApplied after the revert", cv)
	}
	if stored.Status.Phase != v1alpha1.PhaseRunning {
		t.Errorf("phase = %q, want Running", stored.Status.Phase)
	}
}

func TestGatewayReconcile_ValidationFailureAdvancesObservedGeneration(t *testing.T) {
	gw := testGateway()
	gw.Generation = 4
	now := metav1.Now()
	gw.Status = v1alpha1.KrakenDGatewayStatus{
		Phase: v1alpha1.PhaseRunning, ConfigChecksum: "old", ActiveImage: convergedImage, ObservedGeneration: 3,
		Conditions: []metav1.Condition{
			{Type: "ConfigValid", Status: metav1.ConditionTrue, Reason: "ConfigApplied", Message: "applied",
				ObservedGeneration: 3, LastTransitionTime: now},
			{Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentAvailable", Message: "ok",
				ObservedGeneration: 3, LastTransitionTime: now},
		},
	}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "new",
	})
	r.Validator = &mockValidator{validateErr: &renderer.ValidationError{
		Output: "invalid config line 5", Err: fmt.Errorf("exit code 1"),
	}}

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("a rejection is persistent and must not be retried: %v", err)
	}

	stored := getGateway(t, c, gw)
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if stored.Status.ObservedGeneration != 4 || ready == nil || ready.Status != metav1.ConditionFalse ||
		ready.Reason != "ConfigValidationFailed" || stored.Status.Phase != v1alpha1.PhaseError {
		t.Errorf("observedGeneration %d, Ready %+v, phase %q; want 4, False/ConfigValidationFailed, Error",
			stored.Status.ObservedGeneration, ready, stored.Status.Phase)
	}
}

// convergedGatewayAt returns a gateway whose applied configuration is
// checksum, rolled out and Ready.
func convergedGatewayAt(checksum string) *v1alpha1.KrakenDGateway {
	gw := testGateway()
	gw.Generation = 1
	now := metav1.Now()
	gw.Status = v1alpha1.KrakenDGatewayStatus{
		Phase: v1alpha1.PhaseRunning, ConfigChecksum: checksum, ActiveImage: convergedImage, ObservedGeneration: 1,
		Conditions: []metav1.Condition{
			{Type: "ConfigValid", Status: metav1.ConditionTrue, Reason: "ConfigApplied",
				Message: "applied", ObservedGeneration: 1, LastTransitionTime: now},
			{Type: "Available", Status: metav1.ConditionTrue, Reason: "DeploymentAvailable",
				Message: "All replicas are available", ObservedGeneration: 1, LastTransitionTime: now},
			{Type: "Progressing", Status: metav1.ConditionFalse, Reason: "RolloutComplete",
				Message: "Deployment rollout completed successfully", ObservedGeneration: 1, LastTransitionTime: now},
		},
	}
	return gw
}

func TestGatewayReconcile_ConfigChangeNotReadyWhileOldStatusLingers(t *testing.T) {
	gw := convergedGatewayAt("cs-old")
	// The update just applied bumped the Deployment's generation, but its
	// status still describes the old ReplicaSet.
	dep := makeConvergedDeployment(gw, "cs-old")
	dep.Generation = 2
	dep.Status.ObservedGeneration = 1
	c := fakeClientBuilder().WithObjects(gw, dep).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs-new",
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	stored := getGateway(t, c, gw)
	progressing := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason != "ConfigDeployed" {
		t.Errorf("Progressing = %+v, want True/ConfigDeployed until the new revision rolls out", progressing)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status == metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want not True during the rollout", ready)
	}
}

func TestInspectDeploymentStatus_StalePodTemplateIsNotConverged(t *testing.T) {
	gw := convergedGatewayAt("cs-new")
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConfigDeployed,
		Message: "rolling out",
	})
	// A cached Deployment from before the update: its template still carries
	// the previous config checksum, and its status is internally consistent.
	dep := makeConvergedDeployment(gw, "cs-old")
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	r.inspectDeploymentStatus(context.Background(), gw, convergedInputs(gw.Status.ConfigChecksum))

	progressing := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue {
		t.Errorf("Progressing = %+v, want True: the Deployment still runs the previous config", progressing)
	}
}

func TestInspectDeploymentStatus_SurplusOldReplicasAreNotConverged(t *testing.T) {
	gw := convergedGatewayAt("cs1")
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonConfigDeployed,
		Message: "rolling out",
	})
	// Every new replica is available, but an old one has not terminated yet.
	dep := makeConvergedDeployment(gw, "cs1")
	dep.Status.Replicas = 2
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	r.inspectDeploymentStatus(context.Background(), gw, convergedInputs(gw.Status.ConfigChecksum))

	progressing := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue {
		t.Errorf("Progressing = %+v, want True while an old replica is still running", progressing)
	}
}

func TestGatewayReconcile_NotReadyWhenDeploymentLosesAvailability(t *testing.T) {
	gw := convergedGatewayAt("cs1")
	// Every replica is crash-looping: the rollout is long finished, but the
	// Deployment no longer has its minimum available replicas.
	dep := makeConvergedDeployment(gw, "cs1")
	dep.Status.AvailableReplicas = 0
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse,
		Reason: "MinimumReplicasUnavailable", Message: "Deployment does not have minimum availability.",
	}}
	c := fakeClientBuilder().WithObjects(gw, dep).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{
		JSON: []byte(`{"version":3}`), Checksum: "cs1",
	})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	stored := getGateway(t, c, gw)
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "MinimumReplicasUnavailable" {
		t.Errorf("Ready = %+v, want False/MinimumReplicasUnavailable", ready)
	}
}
