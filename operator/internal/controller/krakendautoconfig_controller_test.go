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
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
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
	output   *autoconfig.GenerateOutput
	err      error
	gotInput *autoconfig.GenerateInput
}

func (m *mockGenerator) Generate(_ context.Context, in autoconfig.GenerateInput) (*autoconfig.GenerateOutput, error) {
	m.gotInput = &in
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
					{
						Endpoint: "/api/users",
						Method:   "GET",
						Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/api/users"}},
					},
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
							Endpoints: []v1alpha1.EndpointEntry{
								{
									Endpoint: "/api/users",
									Method:   "GET",
									Backends: []v1alpha1.BackendSpec{
										{Host: []string{"http://svc"}, URLPattern: "/api/users"},
									},
								},
							},
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

// drainEvents returns every event recorded so far, without blocking.
func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case ev := <-rec.Events:
			events = append(events, ev)
		default:
			return events
		}
	}
}

// syncedAutoConfig returns testAutoConfig already Synced at exactly the
// combined checksum a reconcile against defaultMocks and cm computes: its
// OpenAPI spec, CUE definitions and generation are unchanged since the last
// sync.
func syncedAutoConfig(cm *corev1.ConfigMap) *v1alpha1.KrakenDAutoConfig {
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
	fetchChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(`{"paths":{}}`)))
	ac.Status.SpecChecksum = autoConfigSpecChecksum(fetchChecksum, cm.ResourceVersion, ac.Generation)
	return ac
}

// ownedCopy returns a copy of the generated endpoint ep as
// reconcileEndpoints persists it: controlled by ac.
func ownedCopy(t *testing.T, ac *v1alpha1.KrakenDAutoConfig, ep *v1alpha1.KrakenDEndpoint) *v1alpha1.KrakenDEndpoint {
	t.Helper()
	owned := ep.DeepCopy()
	if err := controllerutil.SetControllerReference(ac, owned, testScheme()); err != nil {
		t.Fatalf("setting owner reference: %v", err)
	}
	return owned
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
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
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
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
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
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
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
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
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

func TestAutoConfigReconcile_SpecChangeTriggersReEvaluation(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
	// Stale checksum from generation 0; AC is now at generation 1
	// (simulating a spec edit like adding an override).
	ac.ObjectMeta.Generation = 1
	fetchChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(`{"paths":{}}`)))
	ac.Status.SpecChecksum = autoConfigSpecChecksum(fetchChecksum, cm.ResourceVersion, 0)
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
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced after re-evaluation, got %s", updated.Status.Phase)
	}
	// Checksum should now include the new generation
	if updated.Status.SpecChecksum != autoConfigSpecChecksum(fetchChecksum, cm.ResourceVersion, 1) {
		t.Errorf("expected checksum with generation 1, got %q", updated.Status.SpecChecksum)
	}
}

func TestAutoConfigReconcile_UnchangedChecksumStillFailsUnmatchedOverride(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
	// Checksum exactly as a previous operator wrote it for this spec, CUE
	// definitions and generation (fetchChecksum:cueDefsRV:generation) — the
	// same value this reconcile computes. That operator silently dropped
	// unmatched overrides; an unchanged checksum must not skip evaluation, so
	// the unmatched override now fails the sync.
	fetchChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(`{"paths":{}}`)))
	ac.Status.SpecChecksum = fmt.Sprintf("%s:%s:%d", fetchChecksum, cm.ResourceVersion, ac.Generation)
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err == nil {
		t.Error("expected error for OnChange trigger, got nil")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonUnmatchedOverride {
		t.Errorf("expected Synced condition with reason %s, got %+v", v1alpha1.ReasonUnmatchedOverride, cond)
	}
}

func TestAutoConfigReconcile_RecreatesDeletedEndpointWhenInputsUnchanged(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	// The generated endpoint was deleted out of band, so it is not seeded.
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "test-ac-listusers", Namespace: "default",
	}, &ep); err != nil {
		t.Fatalf("expected deleted endpoint to be recreated: %v", err)
	}
	wantEvent := "Normal EndpointsGenerated Generated 1 endpoints (1 created, 0 updated, 0 deleted, 0 skipped)"
	if events := drainEvents(rec); !slices.Contains(events, wantEvent) {
		t.Errorf("expected event %q, got %v", wantEvent, events)
	}
}

func TestAutoConfigReconcile_RevertsModifiedEndpointWhenInputsUnchanged(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.UID = "test-ac-uid"
	f, ce, fi, g := defaultMocks()
	// The generated endpoint was edited out of band: everything matches what
	// the generator produces except its timeout.
	want := g.output.Endpoints[0]
	modified := ownedCopy(t, ac, want)
	editedTimeout := metav1.Duration{Duration: 99 * time.Second}
	modified.Spec.Endpoints[0].Timeout = &editedTimeout
	c := fakeClientBuilder().
		WithObjects(ac, cm, modified).
		WithStatusSubresource(ac).
		Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: want.Name, Namespace: want.Namespace,
	}, &ep); err != nil {
		t.Fatalf("getting endpoint: %v", err)
	}
	if !equality.Semantic.DeepEqual(ep.Spec, want.Spec) {
		t.Errorf("expected endpoint spec restored to %+v, got %+v", want.Spec, ep.Spec)
	}
	wantEvent := "Normal EndpointsGenerated Generated 1 endpoints (0 created, 1 updated, 0 deleted, 0 skipped)"
	if events := drainEvents(rec); !slices.Contains(events, wantEvent) {
		t.Errorf("expected event %q, got %v", wantEvent, events)
	}
}

func TestAutoConfigReconcile_DeletesStrayEndpointWhenInputsUnchanged(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.UID = "test-ac-uid"
	f, ce, fi, g := defaultMocks()
	// The generated endpoint is present exactly as the generator produces it.
	current := ownedCopy(t, ac, g.output.Endpoints[0])
	// An endpoint carrying this AutoConfig's label that the generator no
	// longer produces (e.g. recreated from an old manifest) must be removed.
	stray := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ac-stray",
			Namespace: "default",
			Labels:    map[string]string{"gateway.krakend.io/autoconfig": "test-ac"},
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/api/stray",
				Method:   "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/stray"}},
			}},
		},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm, current, stray).
		WithStatusSubresource(ac).
		Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: stray.Name, Namespace: stray.Namespace,
	}, &ep); !apierrors.IsNotFound(err) {
		t.Errorf("expected stray endpoint to be deleted, got err %v", err)
	}
	wantEvent := "Normal EndpointsGenerated Generated 1 endpoints (0 created, 0 updated, 1 deleted, 0 skipped)"
	if events := drainEvents(rec); !slices.Contains(events, wantEvent) {
		t.Errorf("expected event %q, got %v", wantEvent, events)
	}
}

func TestAutoConfigSpecChecksum_IsSpecCUEDefinitionsAndGeneration(t *testing.T) {
	if got, want := autoConfigSpecChecksum("abc123", "7:9", 3), "abc123:7:9:3"; got != want {
		t.Errorf("expected checksum %q, got %q", want, got)
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
		{
			Endpoint: "/api/users",
			Method:   "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/api/users"}},
		},
	}
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
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
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}
}

func TestAutoConfigReconcile_UnmatchedOverrideFailsSync(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Status.SpecChecksum = "stale-checksum"
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.UnmatchedOverrides = []string{"WebhookStatus", "WebhookDocuments"}
	rec := fakeRecorder()
	r := &KrakenDAutoConfigReconciler{
		Client: c, Scheme: testScheme(), Recorder: rec,
		Fetcher: f, CUEEvaluator: ce, Filter: fi, Generator: g,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if e := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); e != nil {
		t.Fatalf("getting updated autoconfig: %v", e)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil {
		t.Fatal("expected Synced condition to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected Synced condition False, got %s", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonUnmatchedOverride {
		t.Errorf("expected reason %s, got %s", v1alpha1.ReasonUnmatchedOverride, cond.Reason)
	}
	wantMsg := "spec.overrides reference operationIds not present in the OpenAPI spec: WebhookStatus, WebhookDocuments"
	if cond.Message != wantMsg {
		t.Errorf("expected message %q, got %q", wantMsg, cond.Message)
	}

	if updated.Status.SpecChecksum != "stale-checksum" {
		t.Errorf("expected SpecChecksum unchanged, got %q", updated.Status.SpecChecksum)
	}

	wantEvent := "Warning UnmatchedOverride " + wantMsg
	if events := drainEvents(rec); !slices.Contains(events, wantEvent) {
		t.Fatalf("expected event %q, got %v", wantEvent, events)
	}

	if g.gotInput != nil {
		t.Errorf("expected generator not to be called, got input %+v", g.gotInput)
	}
}

func TestAutoConfigReconcile_UnmatchedOverrideKeepsExistingEndpoints(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending

	// Pre-existing endpoint that would be considered stale (and deleted) if
	// the pipeline reached reconcileEndpoints — it must not.
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
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "test-ac-old-endpoint", Namespace: "default",
	}, &ep); err != nil {
		t.Fatalf("expected existing endpoint to be kept: %v", err)
	}
}

func TestAutoConfigReconcile_UnmatchedOverridePeriodicRequeues(t *testing.T) {
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
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
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

	var updated v1alpha1.KrakenDAutoConfig
	if e := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); e != nil {
		t.Fatalf("getting updated autoconfig: %v", e)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}
}

func TestAutoConfigReconcile_UnmatchedOverrideRecoversWhenResolved(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	r := newACReconciler(c, f, ce, fi, g)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error while the override is unmatched, got nil")
	}
	var failed v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), req.NamespacedName, &failed); err != nil {
		t.Fatalf("getting failed autoconfig: %v", err)
	}
	if failed.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Fatalf("expected phase Error while unmatched, got %s", failed.Status.Phase)
	}

	// The override is fixed: every override now matches. Every reconcile
	// re-evaluates, so the next one picks up the fix.
	ce.output.UnmatchedOverrides = nil
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile after fix: %v", err)
	}

	var recovered v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), req.NamespacedName, &recovered); err != nil {
		t.Fatalf("getting recovered autoconfig: %v", err)
	}
	if recovered.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced after fix, got %s", recovered.Status.Phase)
	}
	cond := meta.FindStatusCondition(recovered.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Synced condition True after fix, got %+v", cond)
	}
}

func TestAutoConfigReconcile_OverrideOnFilteredOutOperationDoesNotFailSync(t *testing.T) {
	// Overrides are matched against every operation the spec declares before
	// spec.filter runs, so an override on an operation the filter excludes
	// matches and must not fail the sync. Real evaluator, filter and
	// generator; no CUE definitions ConfigMap, so the embedded ones are used.
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	timeout := metav1.Duration{Duration: 30 * time.Second}
	ac.Spec.Overrides = []v1alpha1.OperationOverride{{OperationID: "WebhookStatus", Timeout: &timeout}}
	ac.Spec.Filter = &v1alpha1.FilterSpec{ExcludeOperationIds: []string{"WebhookStatus"}}
	c := fakeClientBuilder().
		WithObjects(ac).
		WithStatusSubresource(ac).
		Build()
	spec := []byte(`{"paths": {
		"/api/users": {"get": {"operationId": "listUsers", "responses": {"200": {"description": "OK"}}}},
		"/api/webhooks/status": {"post": {"operationId": "WebhookStatus", "responses": {"202": {"description": "OK"}}}}
	}}`)
	r := &KrakenDAutoConfigReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Fetcher:      &mockFetcher{result: &autoconfig.FetchResult{Data: spec}},
		CUEEvaluator: autoconfig.NewCUEEvaluator(),
		Filter:       autoconfig.NewFilter(),
		Generator:    autoconfig.NewGenerator(),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced, got %s", updated.Status.Phase)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Synced condition True, got %+v", cond)
	}
	if updated.Status.GeneratedEndpoints != 1 {
		t.Errorf("expected 1 generated endpoint (WebhookStatus filtered out), got %d",
			updated.Status.GeneratedEndpoints)
	}
}

func TestAutoConfigReconcile_SyncedFailureStatusUpdateErrorNamesReason(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// Fail only the status write that records the Synced failure.
	failErrorStatus := interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context,
			c client.Client,
			subResource string,
			obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			if a, ok := obj.(*v1alpha1.KrakenDAutoConfig); ok && a.Status.Phase == v1alpha1.AutoConfigPhaseError {
				return fmt.Errorf("simulated conflict")
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(failErrorStatus).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	want := "updating UnmatchedOverride status: simulated conflict"
	if err == nil || err.Error() != want {
		t.Errorf("expected error %q, got %v", want, err)
	}
}

func TestAutoConfigReconcile_EvaluatorWarningsEmitEvents(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.Warnings = []string{"skipping /x:GET: boom"}
	rec := fakeRecorder()
	r := &KrakenDAutoConfigReconciler{
		Client: c, Scheme: testScheme(), Recorder: rec,
		Fetcher: f, CUEEvaluator: ce, Filter: fi, Generator: g,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated,
	); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
		t.Errorf("expected phase Synced, got %s", updated.Status.Phase)
	}

	wantEvent := "Warning CUEEvaluationWarning skipping /x:GET: boom"
	if events := drainEvents(rec); !slices.Contains(events, wantEvent) {
		t.Fatalf("expected event %q, got %v", wantEvent, events)
	}
}

func TestAutoConfigReconcile_EvaluatorWarningsEmittedBeforeUnmatchedFailure(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.Warnings = []string{"skipping /x:GET: boom"}
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	rec := fakeRecorder()
	r := &KrakenDAutoConfigReconciler{
		Client: c, Scheme: testScheme(), Recorder: rec,
		Fetcher: f, CUEEvaluator: ce, Filter: fi, Generator: g,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	wantWarningEvent := "Warning CUEEvaluationWarning skipping /x:GET: boom"
	wantUnmatchedEvent := "Warning UnmatchedOverride " +
		"spec.overrides reference operationIds not present in the OpenAPI spec: WebhookStatus"
	events := drainEvents(rec)
	warningIdx := slices.Index(events, wantWarningEvent)
	unmatchedIdx := slices.Index(events, wantUnmatchedEvent)
	if warningIdx < 0 {
		t.Errorf("expected event %q, got %v", wantWarningEvent, events)
	}
	if unmatchedIdx < 0 {
		t.Errorf("expected event %q, got %v", wantUnmatchedEvent, events)
	}
	if warningIdx > unmatchedIdx {
		t.Errorf("expected %q before %q, got %v", wantWarningEvent, wantUnmatchedEvent, events)
	}
}

func TestAutoConfigPredicate_IgnoresStatusOnlyUpdate(t *testing.T) {
	old := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-ac",
			Namespace:   "default",
			Generation:  1,
			Labels:      map[string]string{"team": "payments"},
			Annotations: map[string]string{"gateway.krakend.io/reconcile": "1"},
		},
		Status: v1alpha1.KrakenDAutoConfigStatus{Phase: v1alpha1.AutoConfigPhaseFetching},
	}
	newObj := old.DeepCopy()
	newObj.Status.Phase = v1alpha1.AutoConfigPhaseSynced

	if autoConfigPredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newObj}) {
		t.Error("expected status-only update to be ignored")
	}
}

func TestAutoConfigPredicate_AcceptsGenerationLabelAndAnnotationChanges(t *testing.T) {
	base := func() *v1alpha1.KrakenDAutoConfig {
		return &v1alpha1.KrakenDAutoConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-ac",
				Namespace:   "default",
				Generation:  1,
				Labels:      map[string]string{"team": "payments"},
				Annotations: map[string]string{"gateway.krakend.io/reconcile": "1"},
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(ac *v1alpha1.KrakenDAutoConfig)
	}{
		{
			name:   "generation bump",
			mutate: func(ac *v1alpha1.KrakenDAutoConfig) { ac.Generation = 2 },
		},
		{
			name:   "label change",
			mutate: func(ac *v1alpha1.KrakenDAutoConfig) { ac.Labels["team"] = "platform" },
		},
		{
			name:   "annotation change",
			mutate: func(ac *v1alpha1.KrakenDAutoConfig) { ac.Annotations["gateway.krakend.io/reconcile"] = "2" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := base()
			newObj := old.DeepCopy()
			tt.mutate(newObj)

			if !autoConfigPredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newObj}) {
				t.Errorf("expected %s to trigger reconcile", tt.name)
			}
		})
	}
}

func TestOwnedEndpointPredicate_IgnoresStatusOnlyUpdateAcceptsSpecAndDelete(t *testing.T) {
	old := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ac-listusers", Namespace: "default", Generation: 1},
		Status:     v1alpha1.KrakenDEndpointStatus{Phase: v1alpha1.EndpointPhasePending},
	}

	statusOnly := old.DeepCopy()
	statusOnly.Status.Phase = v1alpha1.EndpointPhaseActive
	if ownedEndpointPredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: statusOnly}) {
		t.Error("expected status-only endpoint update to be ignored")
	}

	specChange := old.DeepCopy()
	specChange.Generation = 2
	if !ownedEndpointPredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: specChange}) {
		t.Error("expected generation change to trigger reconcile")
	}

	if !ownedEndpointPredicate().Delete(event.DeleteEvent{Object: old}) {
		t.Error("expected delete to trigger reconcile")
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

	requests := r.configMapToAutoConfigs(context.Background(), cm)
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

	requests := r.configMapToAutoConfigs(context.Background(), cm)
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

	requests := r.configMapToAutoConfigs(context.Background(), cm)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for unrelated ConfigMap, got %d", len(requests))
	}
}

func TestConfigMapToAutoConfigs_SpecConfigMap(t *testing.T) {
	ac := testAutoConfig()
	ac.Name = "users-ac"
	ac.Spec.OpenAPI = v1alpha1.OpenAPISource{
		ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "users-spec"},
	}
	c := fakeClientBuilder().WithObjects(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "users-spec",
			Namespace: "default",
		},
	}

	requests := r.configMapToAutoConfigs(context.Background(), cm)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "users-ac" || requests[0].Namespace != "default" {
		t.Errorf("expected request for default/users-ac, got %s/%s", requests[0].Namespace, requests[0].Name)
	}

	// Same-named ConfigMap in another namespace must not match the
	// default-namespace AutoConfig.
	otherNsCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "users-spec",
			Namespace: "other",
		},
	}
	requests = r.configMapToAutoConfigs(context.Background(), otherNsCM)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for cross-namespace ConfigMap, got %d", len(requests))
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

func TestAutoConfigReconcile_AdditionalEndpointsReachGenerator(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/liveness", Encoding: "no-op"},
	}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if g.gotInput == nil {
		t.Fatal("generator was not called")
	}
	// defaultMocks CUE output yields /api/users → derived base is /api.
	// Additional endpoint /liveness is scoped to /api/liveness.
	var found bool
	for _, e := range g.gotInput.Entries {
		if e.Endpoint == "/api/liveness" && e.Method == "GET" {
			found = true
		}
	}
	if !found {
		t.Fatalf("/api/liveness not passed to generator: %+v", g.gotInput.Entries)
	}
}

func TestAutoConfigReconcile_AdditionalEndpointOverrideEmitsWarning(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// defaultMocks CUE output yields /api/users:GET — collide with it.
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/api/users", Method: "GET", Host: "http://override"},
	}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	rec := fakeRecorder()
	r := &KrakenDAutoConfigReconciler{
		Client: c, Scheme: testScheme(), Recorder: rec,
		Fetcher: f, CUEEvaluator: ce, Filter: fi, Generator: g,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	events := drainEvents(rec)
	if !slices.ContainsFunc(events, func(ev string) bool {
		return strings.Contains(ev, "AdditionalEndpointOverride")
	}) {
		t.Fatalf("expected AdditionalEndpointOverride warning event, got %v", events)
	}
	// And the override won.
	for _, e := range g.gotInput.Entries {
		if e.Endpoint == "/api/users" && e.Backends[0].Host[0] != "http://override" {
			t.Fatalf("override did not win: %+v", e.Backends[0])
		}
	}
}

func TestAutoConfigReconcile_AdditionalEndpointsRespectURLTransform(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.URLTransform = &v1alpha1.URLTransformSpec{AddPathPrefix: "/api/v1/quote"}
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/liveness", Encoding: "no-op"},
	}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var found bool
	for _, e := range g.gotInput.Entries {
		if e.Endpoint == "/api/v1/quote/liveness" && e.Method == "GET" {
			found = true
		}
	}
	if !found {
		t.Fatalf("urlTransform not applied to additional endpoint: %+v", g.gotInput.Entries)
	}
}

func TestAutoConfigReconcile_AdditionalEndpointsScopedToDerivedBase(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// defaultMocks CUE output yields /api/users:GET → parent /api → base "/api".
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness", Encoding: "no-op"}}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ep *v1alpha1.EndpointEntry
	for i := range g.gotInput.Entries {
		if g.gotInput.Entries[i].Endpoint == "/api/liveness" {
			ep = &g.gotInput.Entries[i]
		}
	}
	if ep == nil {
		t.Fatalf("expected scoped /api/liveness, got %+v", g.gotInput.Entries)
	}
	if ep.Backends[0].URLPattern != "/liveness" {
		t.Fatalf("backend urlPattern must stay /liveness, got %q", ep.Backends[0].URLPattern)
	}
}

func TestAutoConfigReconcile_AdditionalEndpointsManualBasePath(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.AdditionalEndpointsBasePath = "/custom/base"
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var found bool
	for _, e := range g.gotInput.Entries {
		if e.Endpoint == "/custom/base/liveness" {
			found = true
		}
	}
	if !found {
		t.Fatalf("manual base path not applied: %+v", g.gotInput.Entries)
	}
}

func TestAutoConfigReconcile_AdditionalEndpointsAddPathPrefixNoDoubleScope(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.URLTransform = &v1alpha1.URLTransformSpec{AddPathPrefix: "/api/v1/quote"}
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, e := range g.gotInput.Entries {
		if e.Endpoint == "/api/v1/quote/api/v1/quote/liveness" {
			t.Fatalf("double-prefixed: %q", e.Endpoint)
		}
	}
	var found bool
	for _, e := range g.gotInput.Entries {
		if e.Endpoint == "/api/v1/quote/liveness" {
			found = true
		}
	}
	if !found {
		t.Fatalf("addPathPrefix path missing: %+v", g.gotInput.Entries)
	}
}

func TestAutoConfigReconcile_AdditionalEndpointsIndeterminateBaseErrors(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	// Root-level generated endpoint → no common parent → indeterminate base.
	ce.output.Entries = []v1alpha1.EndpointEntry{
		{Endpoint: "/health", Method: "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/health"}}},
	}
	ce.output.OperationIDs = map[string]string{}
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatal("expected error for indeterminate base path")
	}
	var updated v1alpha1.KrakenDAutoConfig
	if e := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}, &updated); e != nil {
		t.Fatalf("get: %v", e)
	}
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Fatalf("expected phase Error, got %s", updated.Status.Phase)
	}
}
