/*
Copyright 2026.

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
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func (m *mockValidator) Validate(_ context.Context, _ []byte) error {
	return m.validateErr
}

func (m *mockValidator) PrepareValidationCopy(jsonData []byte, _ bool) ([]byte, error) {
	return jsonData, nil
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

func TestGatewayReconcile_InitialPhase(t *testing.T) {
	gw := testGateway()
	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()
	r := &KrakenDGatewayReconciler{
		Client:    c,
		Scheme:    testScheme(),
		Recorder:  fakeRecorder(),
		Renderer:  &mockRenderer{},
		Validator: &mockValidator{},
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after initial phase")
	}

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.PhasePending {
		t.Errorf("expected Pending, got %s", updated.Status.Phase)
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
			JSON:         []byte(`{"version":3}`),
			Checksum:     "newchecksum",
			DesiredImage: "krakend/krakend-ce:2.7.0",
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
	if updated.Status.ActiveImage != "krakend/krakend-ce:2.7.0" {
		t.Errorf("expected active image, got %s", updated.Status.ActiveImage)
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
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &cm); err != nil {
		t.Fatalf("configmap not created: %v", err)
	}
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
	gw.Status.ActiveImage = "krakend/krakend-ce:2.7.0"

	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:         []byte(`{"version":3}`),
			Checksum:     "samechecksum",
			DesiredImage: "krakend/krakend-ce:2.7.0",
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
			JSON:         []byte(`{"version":3}`),
			Checksum:     "newchecksum",
			DesiredImage: "krakend/krakend-ce:2.7.0",
		},
	}

	r := &KrakenDGatewayReconciler{
		Client:   c,
		Scheme:   testScheme(),
		Recorder: fakeRecorder(),
		Renderer: mockRend,
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

func TestGatewayReconcile_ConflictedEndpoints(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending

	ep1 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-conflict", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}

	c := fakeClientBuilder().
		WithObjects(gw, ep1).
		WithStatusSubresource(gw, ep1).
		Build()

	conflicted := types.NamespacedName{Name: "ep-conflict", Namespace: "default"}
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:                []byte(`{"version":3}`),
			Checksum:            "cs1",
			DesiredImage:        "img:v1",
			ConflictedEndpoints: []types.NamespacedName{conflicted},
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

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), conflicted, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseConflicted {
		t.Errorf("expected Conflicted, got %s", updated.Status.Phase)
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
			JSON:         []byte(`{"version":3}`),
			Checksum:     "newcs",
			DesiredImage: "img:v1",
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
	if _, ok := (*capturedInput).Policies["pol1"]; !ok {
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
			JSON: []byte(`{}`), Checksum: "cs", DesiredImage: "img:v1",
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

func TestGatewayReconcile_InvalidEndpoints(t *testing.T) {
	gw := testGateway()
	gw.Status.Phase = v1alpha1.PhasePending

	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-invalid", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}

	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(gw, ep).
		Build()

	invalid := types.NamespacedName{Name: "ep-invalid", Namespace: "default"}
	mockRend := &mockRenderer{
		output: &renderer.RenderOutput{
			JSON:             []byte(`{}`),
			Checksum:         "cs1",
			DesiredImage:     "img:v1",
			InvalidEndpoints: []types.NamespacedName{invalid},
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

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), invalid, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseInvalid {
		t.Errorf("expected Invalid, got %s", updated.Status.Phase)
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
			JSON: []byte(`{}`), Checksum: "cs", DesiredImage: "img:v1",
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
			JSON: []byte(`{}`), Checksum: "cs", DesiredImage: "img:v1",
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
	if state == nil {
		t.Fatal("expected non-nil state")
	}
	if !state.Enabled {
		t.Error("expected Enabled=true")
	}
	if state.ServiceDNS == "" {
		t.Error("expected non-empty ServiceDNS")
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
			JSON: []byte(`{}`), Checksum: "cs", DesiredImage: "img:v1",
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
	if (*capturedInput).Dragonfly == nil {
		t.Error("expected DragonflyState to be set in RenderInput")
	}
	if !(*capturedInput).Dragonfly.Enabled {
		t.Error("expected DragonflyState.Enabled=true")
	}
}

func TestGatewayReconcile_ExternalSecretSkippedWhenCRDMissing(t *testing.T) {
	gw := testGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.License = &v1alpha1.LicenseConfig{
		ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{
			Enabled: true,
			SecretStoreRef: v1alpha1.SecretStoreRef{
				Name: "vault", Kind: "ClusterSecretStore",
			},
			RemoteRef: v1alpha1.ExternalRemoteRef{Key: "krakend/license"},
		},
	}
	gw.Status.Phase = v1alpha1.PhaseRunning
	rec := fakeRecorder()
	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	r := &KrakenDGatewayReconciler{
		Client:   c,
		Scheme:   testScheme(),
		Recorder: rec,
		Renderer: &mockRenderer{
			output: &renderer.RenderOutput{
				JSON: []byte(`{}`), Checksum: "cs", DesiredImage: "img:v1",
			},
		},
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify warning event was emitted about missing CRD.
	found := false
	for len(rec.Events) > 0 {
		e := <-rec.Events
		if strings.Contains(e, "CRDNotInstalled") && strings.Contains(e, "external-secrets.io") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected CRDNotInstalled warning event for ExternalSecret")
	}
}

func TestGatewayReconcile_VirtualServiceSkippedWhenCRDMissing(t *testing.T) {
	gw := testGateway()
	gw.Spec.Istio = &v1alpha1.IstioSpec{
		Enabled:  true,
		Hosts:    []string{"api.example.com"},
		Gateways: []string{"istio-system/gateway"},
	}
	gw.Status.Phase = v1alpha1.PhaseRunning
	rec := fakeRecorder()
	c := fakeClientBuilder().
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()

	r := &KrakenDGatewayReconciler{
		Client:   c,
		Scheme:   testScheme(),
		Recorder: rec,
		Renderer: &mockRenderer{
			output: &renderer.RenderOutput{
				JSON: []byte(`{}`), Checksum: "cs", DesiredImage: "img:v1",
			},
		},
		Validator: &mockValidator{},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(gw),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify warning event was emitted about missing CRD.
	found := false
	for len(rec.Events) > 0 {
		e := <-rec.Events
		if strings.Contains(e, "CRDNotInstalled") && strings.Contains(e, "networking.istio.io") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected CRDNotInstalled warning event for VirtualService")
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

	r.inspectDeploymentStatus(context.Background(), gw)

	if gw.Status.Phase != v1alpha1.PhaseError {
		t.Errorf("expected phase Error, got %s", gw.Status.Phase)
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

	r.inspectDeploymentStatus(context.Background(), gw)

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

	r.inspectDeploymentStatus(context.Background(), gw)

	if gw.Status.Replicas != 0 {
		t.Errorf("expected Replicas=0 (unchanged), got %d", gw.Status.Replicas)
	}
	if gw.Status.Phase != v1alpha1.PhaseDeploying {
		t.Errorf("expected phase unchanged at Deploying, got %s", gw.Status.Phase)
	}
}
