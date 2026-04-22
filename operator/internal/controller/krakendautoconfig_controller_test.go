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
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// --- Mock Fetcher ---

type mockFetcher struct {
	result *autoconfig.FetchResult
	err    error
}

func (m *mockFetcher) Fetch(_ context.Context, _ autoconfig.FetchSource) (*autoconfig.FetchResult, error) {
	return m.result, m.err
}

// --- Mock CUEEvaluator ---

type mockCUEEvaluator struct {
	output *autoconfig.CUEOutput
	err    error
	called bool
}

func (m *mockCUEEvaluator) Evaluate(_ context.Context, _ autoconfig.CUEInput) (*autoconfig.CUEOutput, error) {
	m.called = true
	return m.output, m.err
}

// --- Mock Filter ---

type mockFilter struct {
	result []v1alpha1.EndpointEntry
}

func (m *mockFilter) Apply(
	entries []v1alpha1.EndpointEntry,
	_ map[string][]string,
	_ map[string]string,
	_ v1alpha1.FilterSpec,
) []v1alpha1.EndpointEntry {
	if m.result != nil {
		return m.result
	}
	return entries
}

// --- Mock Generator ---

type mockGenerator struct {
	output *autoconfig.GenerateOutput
	err    error
}

func (m *mockGenerator) Generate(_ context.Context, _ autoconfig.GenerateInput) (*autoconfig.GenerateOutput, error) {
	return m.output, m.err
}

// --- Test Helpers ---

func testAutoConfig() *v1alpha1.KrakenDAutoConfig {
	return &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ac", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			OpenAPI: v1alpha1.OpenAPISource{
				URL: "https://example.com/api.json",
			},
			Trigger: v1alpha1.TriggerOnChange,
		},
	}
}

func testCUEDefinitionsCM() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            defaultCUEDefinitionsConfigMap,
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Data: map[string]string{
			"main.cue": `_spec: _
endpoint: {}`,
		},
	}
}

func defaultMocks() (*mockFetcher, *mockCUEEvaluator, *mockFilter, *mockGenerator) {
	data := []byte(`{"paths":{}}`)
	return &mockFetcher{
			result: &autoconfig.FetchResult{
				Data:     data,
				Checksum: fmt.Sprintf("%x", sha256.Sum256(data)),
			},
		},
		&mockCUEEvaluator{
			output: &autoconfig.CUEOutput{
				Entries: []v1alpha1.EndpointEntry{
					{Endpoint: "/api/users", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/api/users"}}},
				},
				OperationIDs: map[string]string{"/api/users:GET": "listUsers"},
				Tags:         map[string][]string{},
			},
		},
		&mockFilter{},
		&mockGenerator{
			output: &autoconfig.GenerateOutput{
				Endpoints: []*v1alpha1.KrakenDEndpoint{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "test-ac-listusers",
							Namespace: "default",
							Labels: map[string]string{
								"gateway.krakend.io/autoconfig":     "test-ac",
								"gateway.krakend.io/auto-generated": "true",
							},
						},
						Spec: v1alpha1.KrakenDEndpointSpec{
							GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
							Endpoints: []v1alpha1.EndpointEntry{{
								Endpoint: "/api/users",
								Method:   "GET",
								Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/api/users"}},
							}},
						},
					},
				},
				SkippedOperations: 0,
			},
		}
}

func newACReconciler(
	c client.Client,
	fetcher *mockFetcher,
	cueEval *mockCUEEvaluator,
	filter *mockFilter,
	gen *mockGenerator,
) *KrakenDAutoConfigReconciler {
	return &KrakenDAutoConfigReconciler{
		Client:       c,
		Scheme:       testScheme(),
		Recorder:     fakeRecorder(),
		Fetcher:      fetcher,
		CUEEvaluator: cueEval,
		Filter:       filter,
		Generator:    gen,
	}
}

// --- Tests ---

func TestAutoConfigReconcile_NotFound(t *testing.T) {
	f, ce, fi, g := defaultMocks()
	c := fakeClientBuilder().Build()
	r := newACReconciler(c, f, ce, fi, g)

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

func TestAutoConfigReconcile_InitialPhase(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().
		WithObjects(ac).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Error("should requeue after setting initial phase")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhasePending {
		t.Errorf("expected phase Pending, got %s", updated.Status.Phase)
	}
}

func TestAutoConfigReconcile_FetchError(t *testing.T) {
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	f.result = nil
	f.err = fmt.Errorf("connection refused")
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}
}

func TestAutoConfigReconcile_CUEError(t *testing.T) {
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	cm := testCUEDefinitionsCM()
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output = nil
	ce.err = fmt.Errorf("CUE evaluation failed: type mismatch")
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}
}

func TestAutoConfigReconcile_FullPipeline(t *testing.T) {
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	cm := testCUEDefinitionsCM()
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced, got %s", updated.Status.Phase)
	}
	if updated.Status.GeneratedEndpoints != 1 {
		t.Errorf("expected 1 generated endpoint, got %d", updated.Status.GeneratedEndpoints)
	}
	if updated.Status.SpecChecksum == "" {
		t.Error("expected specChecksum to be set")
	}
	if updated.Status.LastSyncTime == nil {
		t.Error("expected lastSyncTime to be set")
	}

	// Verify the endpoint was created
	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "test-ac-listusers", Namespace: "default",
	}, &ep); err != nil {
		t.Fatalf("expected generated endpoint to exist: %v", err)
	}
	if len(ep.Spec.Endpoints) == 0 || ep.Spec.Endpoints[0].Endpoint != "/api/users" {
		t.Errorf("expected endpoint /api/users, got %v", ep.Spec.Endpoints)
	}
}

func TestAutoConfigReconcile_NoChangeSkipsReEvaluation(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
	// Checksum format: fetchChecksum:cueDefsRV:generation
	expectedChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(`{"paths":{}}`)))
	ac.Status.SpecChecksum = expectedChecksum + ":" + cm.ResourceVersion + ":0"
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced (no change), got %s", updated.Status.Phase)
	}
}

func TestAutoConfigReconcile_SpecChangeTriggersReEvaluation(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
	// Stale checksum from generation 0; AC is now at generation 1
	// (simulating a spec edit like adding an override).
	ac.ObjectMeta.Generation = 1
	expectedChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(`{"paths":{}}`)))
	ac.Status.SpecChecksum = expectedChecksum + ":" + cm.ResourceVersion + ":0"
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// CUE evaluator should have been called (not skipped)
	if !ce.called {
		t.Error("expected CUE evaluator to be called on generation change")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced after re-evaluation, got %s", updated.Status.Phase)
	}
	// Checksum should now include the new generation
	if updated.Status.SpecChecksum != expectedChecksum+":"+cm.ResourceVersion+":1" {
		t.Errorf("expected checksum with generation 1, got %q", updated.Status.SpecChecksum)
	}
}

func TestAutoConfigReconcile_PeriodicRequeue(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Spec.Trigger = v1alpha1.TriggerPeriodic
	ac.Spec.Periodic = &v1alpha1.PeriodicSpec{
		Interval: metav1.Duration{Duration: 5 * time.Minute},
	}
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m requeue, got %v", result.RequeueAfter)
	}
}

func TestAutoConfigReconcile_DeleteStaleEndpoints(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending

	// Pre-existing stale endpoint
	staleEP := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ac-old-endpoint",
			Namespace: "default",
			Labels: map[string]string{
				"gateway.krakend.io/autoconfig": "test-ac",
			},
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/api/old",
				Method:   "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/old"}},
			}},
		},
	}

	c := fakeClientBuilder().
		WithObjects(ac, cm, staleEP).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Stale endpoint should be deleted
	var ep v1alpha1.KrakenDEndpoint
	err = c.Get(context.Background(), types.NamespacedName{
		Name: "test-ac-old-endpoint", Namespace: "default",
	}, &ep)
	if err == nil {
		t.Error("expected stale endpoint to be deleted")
	}
}

func TestAutoConfigReconcile_WithFilter(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.Filter = &v1alpha1.FilterSpec{
		IncludePaths: []string{"/api/users"},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	// Filter returns a subset
	fi.result = []v1alpha1.EndpointEntry{
		{Endpoint: "/api/users", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/api/users"}}},
	}
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced, got %s", updated.Status.Phase)
	}
}

func TestAutoConfigReconcile_GeneratorError(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	g.output = nil
	g.err = fmt.Errorf("generator failed")
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}
}

func TestCueConfigMapToAutoConfig_DefaultCM(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultCUEDefinitionsConfigMap,
			Namespace: "default",
		},
	}

	requests := r.cueConfigMapToAutoConfig(context.Background(), cm)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "test-ac" {
		t.Errorf("expected request for test-ac, got %s", requests[0].Name)
	}
}

func TestCueConfigMapToAutoConfig_CustomCM(t *testing.T) {
	ac := testAutoConfig()
	ac.Spec.CUE = &v1alpha1.CUESpec{
		DefinitionsConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "custom-defs"},
	}
	c := fakeClientBuilder().WithObjects(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "custom-defs",
			Namespace: "default",
		},
	}

	requests := r.cueConfigMapToAutoConfig(context.Background(), cm)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
}

func TestCueConfigMapToAutoConfig_UnrelatedCM(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated-configmap",
			Namespace: "default",
		},
	}

	requests := r.cueConfigMapToAutoConfig(context.Background(), cm)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for unrelated ConfigMap, got %d", len(requests))
	}
}

func TestAutoConfigReconcile_FallbackToEmbeddedCUE(t *testing.T) {
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// No CUE definitions ConfigMap — controller should fall back to embedded defs
	c := fakeClientBuilder().
		WithObjects(ac).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the CUE evaluator was still called (using embedded defs)
	if !ce.called {
		t.Error("expected CUE evaluator to be called with embedded defs")
	}
}
