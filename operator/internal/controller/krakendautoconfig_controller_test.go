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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	called bool
	// byURL, when non-nil, overrides result/err for a Fetch call whose
	// source.URL matches a key. Used to make a second (external $ref) fetch
	// behave differently than the main spec fetch.
	byURL map[string]mockFetchOutcome
}

// mockFetchOutcome is one byURL entry: what mockFetcher.Fetch returns for a
// specific source URL.
type mockFetchOutcome struct {
	result *autoconfig.FetchResult
	err    error
}

func (m *mockFetcher) Fetch(_ context.Context, source autoconfig.FetchSource) (*autoconfig.FetchResult, error) {
	m.called = true
	if outcome, ok := m.byURL[source.URL]; ok {
		return outcome.result, outcome.err
	}
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

// eventReasonIndex returns the index of the first drained event with the
// given reason, or -1. FakeRecorder formats each event as
// "<type> <reason> <message>".
func eventReasonIndex(events []string, reason string) int {
	return slices.IndexFunc(events, func(ev string) bool {
		fields := strings.SplitN(ev, " ", 3)
		return len(fields) > 1 && fields[1] == reason
	})
}

// hasEventReason reports whether any of the drained events has the given
// reason.
func hasEventReason(events []string, reason string) bool {
	return eventReasonIndex(events, reason) >= 0
}

// inputWarningReasons are the warning events emitted only when a reconcile's
// inputs differ from the last successful sync's.
var inputWarningReasons = []string{
	v1alpha1.ReasonCUEEvaluationWarning,
	v1alpha1.ReasonDuplicateOperationId,
	v1alpha1.ReasonAdditionalEndpointOverride,
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

// writeCounts tallies the writes a fake client built with countWrites
// receives, by verb.
type writeCounts struct {
	creates, updates, deletes, statusUpdates int
}

// countWrites returns interceptor funcs that tally every create, update,
// delete and status update in counts, then pass it through to the fake client.
func countWrites(counts *writeCounts) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			counts.creates++
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			counts.updates++
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			counts.deletes++
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(
			ctx context.Context,
			c client.Client,
			subResource string,
			obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			counts.statusUpdates++
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
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

func TestAutoConfigReconcile_ExternalRefFetchFailureFailsClosed(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending

	// Pre-existing endpoint that must be left untouched: the sync must fail
	// before reconcileEndpoints ever runs.
	existingEP := &v1alpha1.KrakenDEndpoint{
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
		WithObjects(ac, cm, existingEP).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	f.result.Data = []byte(
		`{"paths":{"/a":{"get":{"responses":{"200":{"$ref":"https://schemas.example.com/frag.json#/A"}}}}}}`,
	)
	f.byURL = map[string]mockFetchOutcome{
		"https://schemas.example.com/frag.json": {err: fmt.Errorf("connection refused")},
	}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

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

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSpecAvailable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonSpecFetchFailed {
		t.Fatalf("expected SpecAvailable False with reason %s, got %+v", v1alpha1.ReasonSpecFetchFailed, cond)
	}
	wantPrefix := "resolving external $refs: "
	if !strings.HasPrefix(cond.Message, wantPrefix) {
		t.Errorf("expected message to start with %q, got %q", wantPrefix, cond.Message)
	}

	events := drainEvents(rec)
	if !hasEventReason(events, v1alpha1.ReasonSpecFetchFailed) {
		t.Errorf("expected a Warning %s event, got %v", v1alpha1.ReasonSpecFetchFailed, events)
	}

	if g.gotInput != nil {
		t.Errorf("expected generator not to be called, got input %+v", g.gotInput)
	}

	var ep v1alpha1.KrakenDEndpoint
	if e := c.Get(context.Background(), types.NamespacedName{
		Name: "test-ac-old-endpoint", Namespace: "default",
	}, &ep); e != nil {
		t.Fatalf("expected existing endpoint to be kept: %v", e)
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

func TestAutoConfigReconcile_SteadyStateWritesNothing(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	var writes writeCounts
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&writes)).
		Build()
	f, ce, fi, g := defaultMocks()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	// Converge first, so the generated endpoint is present exactly as
	// reconcileEndpoints writes it and status records that sync.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("converging reconcile: %v", err)
	}
	writes = writeCounts{}
	ce.called = false
	drainEvents(rec)

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}

	if !ce.called {
		t.Error("expected CUE evaluator to be called on every reconcile")
	}
	if writes.statusUpdates != 0 {
		t.Errorf("expected no status writes, got %d", writes.statusUpdates)
	}
	if writes.creates+writes.updates+writes.deletes != 0 {
		t.Errorf("expected no endpoint writes, got %d creates, %d updates, %d deletes",
			writes.creates, writes.updates, writes.deletes)
	}
	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonEndpointsGenerated) {
		t.Errorf("expected no %s event, got %v", v1alpha1.ReasonEndpointsGenerated, events)
	}
}

func TestAutoConfigReconcile_ChangedInputsWriteStatusOnce(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	// A spec edit since the last sync bumped the generation.
	ac.Generation = 1
	var writes writeCounts
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&writes)).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if writes.statusUpdates != 1 {
		t.Errorf("expected exactly one status write, got %d", writes.statusUpdates)
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
	fetchChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(`{"paths":{}}`)))
	if want := autoConfigSpecChecksum(fetchChecksum, cm.ResourceVersion, 1); updated.Status.SpecChecksum != want {
		t.Errorf("expected checksum %q, got %q", want, updated.Status.SpecChecksum)
	}
	if updated.Status.LastSyncTime == nil {
		t.Error("expected lastSyncTime to be set")
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

func TestAutoConfigReconcile_TerminatingAutoConfigDoesNotRecreateEndpoints(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	// Foreground deletion: the AutoConfig lingers with a deletionTimestamp
	// (the fake client needs a finalizer to accept one) while garbage
	// collection deletes its endpoints, so the generated endpoint is absent.
	now := metav1.Now()
	ac.DeletionTimestamp = &now
	ac.Finalizers = []string{"test.krakend.io/hold"}
	var writes writeCounts
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&writes)).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: "test-ac-listusers", Namespace: "default",
	}, &ep); !apierrors.IsNotFound(err) {
		t.Errorf("expected no endpoint for a terminating AutoConfig, got err %v", err)
	}
	if f.called || ce.called {
		t.Errorf("expected no fetch or CUE evaluation, got fetch=%t evaluate=%t", f.called, ce.called)
	}
	if writes.statusUpdates != 0 {
		t.Errorf("expected no status writes, got %d", writes.statusUpdates)
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

func TestAutoConfigReconcile_RestoresEndpointLabelsWhenInputsUnchanged(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.UID = "test-ac-uid"
	f, ce, fi, g := defaultMocks()
	// The generated endpoint's owner label was removed out of band; its spec
	// still matches what the generator produces.
	want := g.output.Endpoints[0]
	relabeled := ownedCopy(t, ac, want)
	relabeled.Labels = map[string]string{"gateway.krakend.io/auto-generated": "true"}
	c := fakeClientBuilder().
		WithObjects(ac, cm, relabeled).
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
	if !maps.Equal(ep.Labels, want.Labels) {
		t.Errorf("expected endpoint labels restored to %v, got %v", want.Labels, ep.Labels)
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

func TestAutoConfigReconcile_ReorderedExtraConfigKeysWriteNoEndpoint(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.UID = "test-ac-uid"
	f, ce, fi, g := defaultMocks()
	// The generator emits extraConfig keys in CUE declaration order; the API
	// server stores the same JSON re-encoded (sorted keys, other formatting).
	desired := g.output.Endpoints[0]
	desired.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"qos/ratelimit/router":{"max_rate":10,"every":"2s"},"documentation/openapi":{"summary":"<b>"}}`),
	}
	stored := ownedCopy(t, ac, desired)
	stored.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"documentation/openapi": {"summary": "<b>"}, ` +
			`"qos/ratelimit/router": {"every": "2s", "max_rate": 10.0}}`),
	}
	var writes writeCounts
	c := fakeClientBuilder().
		WithObjects(ac, cm, stored).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&writes)).
		Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if writes.updates != 0 {
		t.Errorf("expected no endpoint updates for semantically identical extraConfig, got %d", writes.updates)
	}
	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonEndpointsGenerated) {
		t.Errorf("expected no %s event, got %v", v1alpha1.ReasonEndpointsGenerated, events)
	}
}

// rawJSONSpec returns an endpoint spec whose endpoint extraConfig, backend
// extraConfig and component schema carry the given raw JSON.
func rawJSONSpec(endpointExtra, backendExtra, schema string) v1alpha1.KrakenDEndpointSpec {
	return v1alpha1.KrakenDEndpointSpec{
		GatewayRef:       v1alpha1.GatewayRef{Name: "test-gw"},
		ComponentSchemas: map[string]runtime.RawExtension{"User": {Raw: []byte(schema)}},
		Endpoints: []v1alpha1.EndpointEntry{{
			Endpoint:    "/api/users",
			Method:      "GET",
			ExtraConfig: &runtime.RawExtension{Raw: []byte(endpointExtra)},
			Backends: []v1alpha1.BackendSpec{{
				Host:        []string{"http://svc"},
				URLPattern:  "/api/users",
				ExtraConfig: &runtime.RawExtension{Raw: []byte(backendExtra)},
			}},
		}},
	}
}

func TestEndpointSpecEqual_IgnoresRawJSONFormatting(t *testing.T) {
	a := rawJSONSpec(
		`{"b":{"y":1,"x":"\u003cb\u003e"},"a":[1,2]}`,
		`{"z":true,"m":null}`,
		`{"type":"object","properties":{"name":{"type":"string"}}}`,
	)
	b := rawJSONSpec(
		`{ "a": [1.0, 2], "b": { "x": "<b>", "y": 1 } }`,
		`{"m": null, "z": true}`,
		`{"properties": {"name": {"type": "string"}}, "type": "object"}`,
	)
	if !endpointSpecEqual(a, b) {
		t.Error("expected specs differing only in raw JSON key order, whitespace, escaping and " +
			"number format to be equal")
	}
}

func TestEndpointSpecEqual_DetectsValueChanges(t *testing.T) {
	base := func() v1alpha1.KrakenDEndpointSpec {
		return rawJSONSpec(`{"a":1}`, `{"b":2}`, `{"type":"object"}`)
	}
	tests := []struct {
		name string
		// setup, if set, is applied to both specs before mutate changes one.
		setup  func(spec *v1alpha1.KrakenDEndpointSpec)
		mutate func(spec *v1alpha1.KrakenDEndpointSpec)
	}{
		{
			name: "endpoint extraConfig value",
			mutate: func(spec *v1alpha1.KrakenDEndpointSpec) {
				spec.Endpoints[0].ExtraConfig.Raw = []byte(`{"a":2}`)
			},
		},
		{
			name: "endpoint extraConfig added key",
			mutate: func(spec *v1alpha1.KrakenDEndpointSpec) {
				spec.Endpoints[0].ExtraConfig.Raw = []byte(`{"a":1,"b":2}`)
			},
		},
		{
			name: "endpoint extraConfig nil vs present",
			setup: func(spec *v1alpha1.KrakenDEndpointSpec) {
				spec.Endpoints[0].ExtraConfig = nil
			},
			mutate: func(spec *v1alpha1.KrakenDEndpointSpec) {
				spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(`{}`)}
			},
		},
		{
			name: "backend extraConfig value",
			mutate: func(spec *v1alpha1.KrakenDEndpointSpec) {
				spec.Endpoints[0].Backends[0].ExtraConfig.Raw = []byte(`{"b":3}`)
			},
		},
		{
			name: "component schema value",
			mutate: func(spec *v1alpha1.KrakenDEndpointSpec) {
				spec.ComponentSchemas["User"] = runtime.RawExtension{Raw: []byte(`{"type":"array"}`)}
			},
		},
		{
			name: "typed field",
			mutate: func(spec *v1alpha1.KrakenDEndpointSpec) {
				timeout := metav1.Duration{Duration: 5 * time.Second}
				spec.Endpoints[0].Timeout = &timeout
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig, changed := base(), base()
			if tt.setup != nil {
				tt.setup(&orig)
				tt.setup(&changed)
			}
			tt.mutate(&changed)
			if endpointSpecEqual(orig, changed) {
				t.Errorf("expected specs to be unequal after changing the %s", tt.name)
			}
		})
	}
}

func TestAutoConfigReconcile_RecoveryWithUnchangedInputsWritesStatusWithoutEvent(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.UID = "test-ac-uid"
	// A transient failure (e.g. loading CUE definitions) failed the last
	// reconcile after an earlier sync; inputs and endpoints are unchanged.
	ac.Status.Phase = v1alpha1.AutoConfigPhaseError
	ac.Status.Conditions = []metav1.Condition{{
		Type:               v1alpha1.ConditionSynced,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonCUEEvaluationFailed,
		Message:            "transient",
		LastTransitionTime: metav1.Now(),
	}}
	f, ce, fi, g := defaultMocks()
	c := fakeClientBuilder().
		WithObjects(ac, cm, ownedCopy(t, ac, g.output.Endpoints[0])).
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
	if updated.Status.LastSyncTime != nil {
		t.Errorf("expected lastSyncTime untouched, got %v", updated.Status.LastSyncTime)
	}
	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonEndpointsGenerated) {
		t.Errorf("expected no %s event, got %v", v1alpha1.ReasonEndpointsGenerated, events)
	}
}

func TestAutoConfigSpecChecksum_IsSpecCUEDefinitionsAndGeneration(t *testing.T) {
	if got, want := autoConfigSpecChecksum("abc123", "7:9", 3), "abc123:7:9:3"; got != want {
		t.Errorf("expected checksum %q, got %q", want, got)
	}
}

func TestAutoConfigReconcile_OnChangeRequeuesAfterResyncInterval(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
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
	if result.RequeueAfter != defaultResyncInterval {
		t.Errorf("expected %v requeue, got %v", defaultResyncInterval, result.RequeueAfter)
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

func TestAutoConfigReconcile_EndpointReconcileFailureFailsSync(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.Status.Conditions = []metav1.Condition{{
		Type:               v1alpha1.ConditionSynced,
		Status:             metav1.ConditionTrue,
		Reason:             "Synced",
		Message:            "Generated 1 endpoints",
		LastTransitionTime: metav1.Now(),
	}}
	// The generated endpoint is missing and the API server rejects the
	// create (e.g. an admission webhook).
	rejectEndpointCreate := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
				return fmt.Errorf("simulated webhook rejection")
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(rejectEndpointCreate).
		Build()
	f, ce, fi, g := defaultMocks()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

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
	wantMsg := "reconciling endpoints: upserting endpoint test-ac-listusers: simulated webhook rejection"
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != v1alpha1.ReasonEndpointReconcileFailed || cond.Message != wantMsg {
		t.Errorf("expected Synced condition False with reason %s and message %q, got %+v",
			v1alpha1.ReasonEndpointReconcileFailed, wantMsg, cond)
	}
	wantEvent := "Warning " + v1alpha1.ReasonEndpointReconcileFailed + " " + wantMsg
	if events := drainEvents(rec); !slices.Contains(events, wantEvent) {
		t.Errorf("expected event %q, got %v", wantEvent, events)
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

// conflictError returns the Conflict the API server answers a write with
// when the writer's copy of the object is stale.
func conflictError(resource, name string) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: resource},
		name, errors.New("the object has been modified; please apply your changes to the latest version"))
}

// conflictStatusWrites returns interceptor funcs that reject every status
// write with a Conflict, as for a reconcile that read a stale cached copy.
func conflictStatusWrites() interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(
			_ context.Context,
			_ client.Client,
			_ string,
			obj client.Object,
			_ ...client.SubResourceUpdateOption,
		) error {
			return conflictError("krakendautoconfigs", obj.GetName())
		},
	}
}

// failEndpointCreates returns interceptor funcs that fail every
// KrakenDEndpoint create with the error errFor returns for its name.
func failEndpointCreates(errFor func(endpointName string) error) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
				return errFor(obj.GetName())
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// assertQuietRequeue checks that a reconcile which lost a write race
// requeued after conflictRequeueDelay with no error and no event.
func assertQuietRequeue(t *testing.T, result ctrl.Result, err error, rec *record.FakeRecorder) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected no error for a lost write race, got %v", err)
	}
	if result.RequeueAfter != conflictRequeueDelay {
		t.Errorf("expected requeue after %v, got %v", conflictRequeueDelay, result.RequeueAfter)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("expected no events, got %v", events)
	}
}

func TestAutoConfigReconcile_InitialPhaseStatusConflictRequeuesQuietly(t *testing.T) {
	ac := testAutoConfig()
	c := fakeClientBuilder().
		WithObjects(ac).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(conflictStatusWrites()).
		Build()
	f, ce, fi, g := defaultMocks()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	assertQuietRequeue(t, result, err, rec)
}

func TestAutoConfigReconcile_FetchErrorStatusConflictRequeuesQuietly(t *testing.T) {
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(conflictStatusWrites()).
		Build()
	f, ce, fi, g := defaultMocks()
	f.result = nil
	f.err = fmt.Errorf("connection refused")
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	assertQuietRequeue(t, result, err, rec)
}

func TestAutoConfigReconcile_SyncStatusConflictRequeuesQuietly(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// Inputs changed, and each input warning applies: none may be recorded
	// by a reconcile whose status write then conflicts.
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/api/users", Method: "GET", Host: "http://override"},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(conflictStatusWrites()).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.Warnings = []string{"skipping /x:GET: boom"}
	g.output.Duplicates = []string{"listUsers"}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	assertQuietRequeue(t, result, err, rec)
}

func TestAutoConfigReconcile_EndpointWriteRaceRequeuesQuietly(t *testing.T) {
	tests := []struct {
		name string
		err  func(endpointName string) error
	}{
		{
			name: "conflict",
			err: func(endpointName string) error {
				return conflictError("krakendendpoints", endpointName)
			},
		},
		{
			// The cache had not seen an endpoint that already exists.
			name: "already exists",
			err: func(endpointName string) error {
				return apierrors.NewAlreadyExists(
					schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "krakendendpoints"},
					endpointName)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := testCUEDefinitionsCM()
			ac := syncedAutoConfig(cm)
			c := fakeClientBuilder().
				WithObjects(ac, cm).
				WithStatusSubresource(ac).
				WithInterceptorFuncs(failEndpointCreates(tt.err)).
				Build()
			f, ce, fi, g := defaultMocks()
			rec := fakeRecorder()
			r := newACReconciler(c, f, ce, fi, g)
			r.Recorder = rec

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
			})
			assertQuietRequeue(t, result, err, rec)
			var updated v1alpha1.KrakenDAutoConfig
			if e := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
				&updated); e != nil {
				t.Fatalf("getting updated autoconfig: %v", e)
			}
			if updated.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
				t.Errorf("expected phase to stay Synced, got %s", updated.Status.Phase)
			}
		})
	}
}

func TestAutoConfigReconcile_PeriodicEndpointReconcileFailureRetriesWithBackoff(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.Spec.Trigger = v1alpha1.TriggerPeriodic
	ac.Spec.Periodic = &v1alpha1.PeriodicSpec{Interval: metav1.Duration{Duration: 5 * time.Minute}}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(failEndpointCreates(func(string) error {
			return fmt.Errorf("simulated webhook rejection")
		})).
		Build()
	f, ce, fi, g := defaultMocks()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	// An error, not the interval requeue, so controller-runtime retries the
	// endpoint write with backoff; it ignores the result alongside an error.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatalf("expected an error for the endpoint write failure, got nil with result %+v", result)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected an empty result alongside the error, got %+v", result)
	}

	var updated v1alpha1.KrakenDAutoConfig
	if e := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated); e != nil {
		t.Fatalf("getting updated autoconfig: %v", e)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if updated.Status.Phase != v1alpha1.AutoConfigPhaseError || cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonEndpointReconcileFailed {
		t.Errorf("expected phase Error and Synced False with reason %s, got phase %s and condition %+v",
			v1alpha1.ReasonEndpointReconcileFailed, updated.Status.Phase, cond)
	}
	if events := drainEvents(rec); !hasEventReason(events, v1alpha1.ReasonEndpointReconcileFailed) {
		t.Errorf("expected a Warning %s event, got %v", v1alpha1.ReasonEndpointReconcileFailed, events)
	}
}

func TestAutoConfigReconcile_SyncedFailureStatusConflictRequeuesQuietly(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(conflictStatusWrites()).
		Build()
	f, ce, fi, g := defaultMocks()
	// Inputs changed and the evaluator warns: the warning must not be
	// recorded by a reconcile whose status write then conflicts.
	ce.output.Warnings = []string{"skipping /x:GET: boom"}
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	assertQuietRequeue(t, result, err, rec)
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

func TestAutoConfigReconcile_SteadyStateSuppressesInputWarningEvents(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	// Collide with the generated /api/users:GET entry so an override would
	// fire AdditionalEndpointOverride if inputs were (wrongly) treated as
	// changed.
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/api/users", Method: "GET", Host: "http://override"},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.Warnings = []string{"skipping /x:GET: boom"}
	g.output.Duplicates = []string{"listUsers"}
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
	for _, reason := range inputWarningReasons {
		if hasEventReason(events, reason) {
			t.Errorf("expected no %s event when inputs are unchanged, got %v", reason, events)
		}
	}
}

func TestAutoConfigReconcile_ChangedInputsEmitsInputWarningEvents(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/api/users", Method: "GET", Host: "http://override"},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.Warnings = []string{"skipping /x:GET: boom"}
	g.output.Duplicates = []string{"listUsers"}
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
	for _, reason := range inputWarningReasons {
		if !hasEventReason(events, reason) {
			t.Errorf("expected %s event when inputs changed, got %v", reason, events)
		}
	}
}

func TestAutoConfigReconcile_InputWarningEventsPrecedeEndpointsGenerated(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/api/users", Method: "GET", Host: "http://override"},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.Warnings = []string{"skipping /x:GET: boom"}
	g.output.Duplicates = []string{"listUsers"}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	events := drainEvents(rec)
	generatedIdx := eventReasonIndex(events, v1alpha1.ReasonEndpointsGenerated)
	if generatedIdx < 0 {
		t.Fatalf("expected %s event, got %v", v1alpha1.ReasonEndpointsGenerated, events)
	}
	for _, reason := range inputWarningReasons {
		idx := eventReasonIndex(events, reason)
		if idx < 0 {
			t.Errorf("expected %s event, got %v", reason, events)
		} else if idx > generatedIdx {
			t.Errorf("expected %s before %s, got %v", reason, v1alpha1.ReasonEndpointsGenerated, events)
		}
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

func TestConfigMapToAutoConfigs_DefaultCM(t *testing.T) {
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

func TestConfigMapToAutoConfigs_CustomCM(t *testing.T) {
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

func TestConfigMapToAutoConfigs_UnrelatedCM(t *testing.T) {
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

	if events := drainEvents(rec); !hasEventReason(events, v1alpha1.ReasonAdditionalEndpointOverride) {
		t.Fatalf("expected %s warning event, got %v", v1alpha1.ReasonAdditionalEndpointOverride, events)
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

// --- Synced gauge ---

func TestAutoConfigReconcile_SyncedGaugeSetOnSuccess(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Name = "metrics-success-ac"
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 1 {
		t.Errorf("expected synced gauge 1 after a successful sync, got %v", got)
	}
}

func TestAutoConfigReconcile_SyncedGaugeZeroOnUnmatchedOverride(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Name = "metrics-unmatched-ac"
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	// First reconcile succeeds, so the gauge starts at 1.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 1 {
		t.Fatalf("expected synced gauge 1 after success, got %v", got)
	}

	// Second reconcile fails closed on an unmatched override.
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 0 {
		t.Errorf("expected synced gauge 0 after an unmatched-override failure, got %v", got)
	}
}

func TestAutoConfigReconcile_SyncedGaugeZeroOnFetchError(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Name = "metrics-fetcherror-ac"
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	// First reconcile succeeds, so the gauge starts at 1.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 1 {
		t.Fatalf("expected synced gauge 1 after success, got %v", got)
	}

	// Second reconcile fails to fetch the spec.
	f.result = nil
	f.err = fmt.Errorf("connection refused")
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 0 {
		t.Errorf("expected synced gauge 0 after a fetch failure, got %v", got)
	}
}

func TestAutoConfigReconcile_SyncedGaugeDeletedOnNotFound(t *testing.T) {
	// Isolate this test's count of the package-level gauge's series from
	// every other test's, which persist for the life of the test binary.
	autoConfigSynced.Reset()

	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Name = "metrics-notfound-ac"
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	// First reconcile succeeds, so the gauge starts at 1.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 1 {
		t.Fatalf("expected synced gauge 1 after success, got %v", got)
	}

	// The AutoConfig is gone (no finalizers, so Delete removes it outright).
	if err := c.Delete(context.Background(), ac); err != nil {
		t.Fatalf("deleting autoconfig: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}

	if n := testutil.CollectAndCount(autoConfigSynced); n != 0 {
		t.Errorf("expected the synced gauge series to be deleted, got %d series", n)
	}
}

func TestAutoConfigReconcile_SyncedGaugeUnchangedOnQuietConflict(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Name = "metrics-conflict-ac"
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// Let the first (successful) status write through; conflict every write
	// after that, as a reconcile reading a stale cached copy would hit.
	var writeCount int
	conflictAfterFirstWrite := interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context,
			c client.Client,
			subResource string,
			obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			writeCount++
			if writeCount > 1 {
				return conflictError("krakendautoconfigs", obj.GetName())
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
	}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(conflictAfterFirstWrite).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	// First reconcile succeeds, so the gauge starts at 1.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 1 {
		t.Fatalf("expected synced gauge 1 after success, got %v", got)
	}

	// A spec edit (generation bump) changes the combined checksum, so the
	// second reconcile attempts a status write — which this time conflicts.
	var current v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("getting autoconfig: %v", err)
	}
	current.Generation = 1
	if err := c.Update(context.Background(), &current); err != nil {
		t.Fatalf("updating autoconfig: %v", err)
	}

	rec := fakeRecorder()
	r.Recorder = rec
	result, err := r.Reconcile(context.Background(), req)
	assertQuietRequeue(t, result, err, rec)

	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 1 {
		t.Errorf("expected synced gauge to stay 1 across a quiet conflict requeue, got %v", got)
	}
}

func TestAutoConfigReconcile_SyncedGaugeDeletedOnTerminating(t *testing.T) {
	// Isolate this test's count of the package-level gauge's series from
	// every other test's, which persist for the life of the test binary.
	autoConfigSynced.Reset()

	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Name = "metrics-terminating-ac"
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// A finalizer so the fake client's Delete marks the object terminating
	// (sets DeletionTimestamp) instead of removing it outright.
	ac.Finalizers = []string{"test.krakend.io/hold"}
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	// First reconcile succeeds, so the gauge starts at 1.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 1 {
		t.Fatalf("expected synced gauge 1 after success, got %v", got)
	}

	// Foreground deletion: the finalizer keeps the object around with a
	// DeletionTimestamp set, so the next reconcile takes the deletion guard.
	var current v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("getting autoconfig: %v", err)
	}
	if err := c.Delete(context.Background(), &current); err != nil {
		t.Fatalf("deleting autoconfig: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}

	if n := testutil.CollectAndCount(autoConfigSynced); n != 0 {
		t.Errorf("expected the synced gauge series to be deleted, got %d series", n)
	}
}
