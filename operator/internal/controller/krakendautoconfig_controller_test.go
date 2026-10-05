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

	"github.com/go-logr/logr/funcr"
	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
	// gotInput is the input of the last Evaluate call.
	gotInput *autoconfig.CUEInput
}

func (m *mockCUEEvaluator) Evaluate(_ context.Context, input autoconfig.CUEInput) (*autoconfig.CUEOutput, error) {
	m.called = true
	m.gotInput = &input
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
		ObjectMeta: metav1.ObjectMeta{Name: "test-ac", Namespace: "default", UID: "test-ac-uid"},
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
			},
		}
}

// duplicateListUsers is a generator skip: a second operation claiming
// listUsers.
func duplicateListUsers() autoconfig.OperationIssue {
	return autoconfig.OperationIssue{
		Operation: autoconfig.Operation{Method: "GET", Path: "/v2/users", OperationID: "listUsers"},
		Reason:    v1alpha1.ReasonDuplicateOperationId,
		Message:   `operationId "listUsers" is already used by GET /api/users`,
	}
}

// reconcileAC runs one reconcile of ac with r.
func reconcileAC(r *KrakenDAutoConfigReconciler, ac *v1alpha1.KrakenDAutoConfig) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ac)})
}

// getAC returns the stored copy of ac.
func getAC(t *testing.T, c client.Client, ac *v1alpha1.KrakenDAutoConfig) *v1alpha1.KrakenDAutoConfig {
	t.Helper()
	var cur v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ac), &cur); err != nil {
		t.Fatalf("getting autoconfig: %v", err)
	}
	return &cur
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
		Checker:      &fakeChecker{},
	}
}

// fakeChecker is an AutoConfigChecker that returns verdicts in order, then
// passes; err, when set, is returned by every call, and judge, when set,
// decides every call's verdict from its replace set. calls records each
// call's replace set.
type fakeChecker struct {
	verdicts []configcheck.Verdict
	err      error
	judge    func(replace []v1alpha1.KrakenDEndpoint) configcheck.Verdict
	calls    [][]v1alpha1.KrakenDEndpoint
}

func (f *fakeChecker) CheckGateway(
	_ context.Context,
	_ *v1alpha1.KrakenDGateway,
	replace []v1alpha1.KrakenDEndpoint,
) (configcheck.Verdict, error) {
	f.calls = append(f.calls, replace)
	if f.err != nil {
		return configcheck.Verdict{}, f.err
	}
	if f.judge != nil {
		return f.judge(replace), nil
	}
	if len(f.verdicts) == 0 {
		return configcheck.Verdict{OK: true}, nil
	}
	v := f.verdicts[0]
	f.verdicts = f.verdicts[1:]
	return v, nil
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

func TestAutoConfigReconcile_FirstReconcileWritesOnlyTheSyncedStatus(t *testing.T) {
	ac := testAutoConfig()
	ac.Generation = 1
	counts := &writeCounts{}
	c := fakeClientBuilder().
		WithObjects(ac).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(counts)).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != (ctrl.Result{RequeueAfter: defaultResyncInterval}) {
		t.Errorf("result = %+v, want only the resync requeue", result)
	}
	if counts.statusUpdates != 1 {
		t.Errorf("status writes = %d, want 1 (no separate Pending write)", counts.statusUpdates)
	}
	var stored v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ac), &stored); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if stored.Status.Phase != v1alpha1.AutoConfigPhaseSynced || stored.Status.ObservedGeneration != 1 ||
		ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonEndpointsNotReady ||
		ready.ObservedGeneration != 1 {
		t.Errorf("phase %q, observedGeneration %d, Ready %+v; want Synced, 1, False/EndpointsNotReady at generation 1",
			stored.Status.Phase, stored.Status.ObservedGeneration, ready)
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

func TestAutoConfigReconcile_FetchErrorMarksSyncedFalse(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	r := newACReconciler(c, f, ce, fi, g)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace}}

	// First reconcile succeeds: Synced=True.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Second reconcile fails to fetch the spec: the Synced condition must
	// agree with the phase and the gauge instead of keeping the last
	// successful sync's True.
	f.result = nil
	f.err = fmt.Errorf("connection refused")
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), req.NamespacedName, &updated); err != nil {
		t.Fatalf("getting updated autoconfig: %v", err)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonSpecFetchFailed ||
		cond.Message != "connection refused" {
		t.Errorf("expected Synced False with reason %s and the fetch error as message, got %+v",
			v1alpha1.ReasonSpecFetchFailed, cond)
	}
}

func TestAutoConfigReconcile_ExternalRefFetchFailureFailsClosed(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending

	// Pre-existing endpoint that must be left untouched: the sync must fail
	// before reconcileEndpoints ever runs.
	existingEP := ownedCopy(t, ac, &v1alpha1.KrakenDEndpoint{
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
	})

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

func TestAutoConfigReconcile_UndecodableSpecFailsWithoutRefPrefix(t *testing.T) {
	// A URL spec that is not a JSON object or YAML map fails before any
	// external $ref is looked at: the error is the spec's own, not an
	// external $ref failure.
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		Build()
	f, ce, fi, g := defaultMocks()
	f.result.Data = []byte(`[1,2,3]`)
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	if err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}

	var updated v1alpha1.KrakenDAutoConfig
	if e := c.Get(context.Background(), types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		&updated); e != nil {
		t.Fatalf("getting updated autoconfig: %v", e)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSpecAvailable)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonSpecFetchFailed {
		t.Fatalf("expected SpecAvailable False with reason %s, got %+v", v1alpha1.ReasonSpecFetchFailed, cond)
	}
	if strings.Contains(cond.Message, "$ref") || !strings.HasPrefix(cond.Message, "decoding spec: ") {
		t.Errorf("expected the spec's own decode error without the $ref prefix, got %q", cond.Message)
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
	f, ce, fi, g := defaultMocks()
	// The generated endpoint is present exactly as the generator produces it.
	current := ownedCopy(t, ac, g.output.Endpoints[0])
	// An endpoint carrying both managed labels that the generator no longer
	// produces (e.g. recreated from an old manifest) is adopted, then removed.
	stray := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ac-stray",
			Namespace: "default",
			Labels: map[string]string{
				autoconfig.LabelAutoConfig:    "test-ac",
				autoconfig.LabelAutoGenerated: "true",
			},
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
	staleEP := ownedCopy(t, ac, &v1alpha1.KrakenDEndpoint{
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
	})

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
	wantMsg := "spec.overrides reference operationIds or backend indexes not present in the OpenAPI spec: WebhookStatus; WebhookDocuments"
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

func TestAutoConfigReconcile_AmbiguousOverrideFailsClosed(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	// An endpoint this AutoConfig owns that the sync would delete as stale
	// if it got that far: it must be kept.
	staleEP := ownedCopy(t, ac, &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ac-old-endpoint", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/api/old", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/old"}},
			}},
		},
	})
	f, ce, fi, g := defaultMocks()
	ce.output.AmbiguousOverrides = []string{"getUsers"}
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm, staleEP).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("expected an error for an OnChange trigger")
	}
	if counts.creates+counts.updates+counts.deletes != 0 {
		t.Errorf("expected no endpoint writes, got %+v", counts)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(staleEP), &v1alpha1.KrakenDEndpoint{}); err != nil {
		t.Errorf("expected the owned endpoint to be kept: %v", err)
	}
	cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonAmbiguousOverride || !strings.Contains(cond.Message, "getUsers") {
		t.Errorf("expected Synced False/AmbiguousOverride naming getUsers, got %+v", cond)
	}
}

func TestAutoConfigReconcile_UnmatchedOverrideMessageNamesAtMostFive(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	ce.output.UnmatchedOverrides = []string{"a", "b", "c", "d", "e", "f", "g"}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("expected an error for an OnChange trigger")
	}
	cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	want := "spec.overrides reference operationIds or backend indexes not present in the OpenAPI spec: " +
		"a; b; c; d; e; and 2 more"
	if cond == nil || cond.Message != want {
		t.Errorf("Synced condition = %+v, want message %q", cond, want)
	}
}

func TestAutoConfigReconcile_UnmatchedOverrideKeepsExistingEndpoints(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Status.Phase = v1alpha1.AutoConfigPhasePending

	// Pre-existing endpoint that would be considered stale (and deleted) if
	// the pipeline reached reconcileEndpoints — it must not.
	staleEP := ownedCopy(t, ac, &v1alpha1.KrakenDEndpoint{
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
	})

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
		Checker:      &fakeChecker{},
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
	c := fakeClientBuilder().
		WithObjects(ac, cm).
		WithStatusSubresource(ac).
		WithInterceptorFuncs(failFailureStatusWrites()).
		Build()
	f, ce, fi, g := defaultMocks()
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	r := newACReconciler(c, f, ce, fi, g)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	})
	want := "updating UnmatchedOverride status: simulated server error"
	if err == nil || err.Error() != want {
		t.Errorf("expected error %q, got %v", want, err)
	}
}

// failFailureStatusWrites returns interceptor funcs that fail, with a plain
// (non-conflict) server error, every status write recording a failed sync
// (phase Error), and pass every other status write through.
func failFailureStatusWrites() interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context,
			c client.Client,
			subResource string,
			obj client.Object,
			opts ...client.SubResourceUpdateOption,
		) error {
			if a, ok := obj.(*v1alpha1.KrakenDAutoConfig); ok && a.Status.Phase == v1alpha1.AutoConfigPhaseError {
				return fmt.Errorf("simulated server error")
			}
			return c.SubResource(subResource).Update(ctx, obj, opts...)
		},
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
	g.output.Skipped = []autoconfig.OperationIssue{duplicateListUsers()}
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

func TestAutoConfigReconcile_FailureStatusConflictKeepsFailureResult(t *testing.T) {
	// A failed sync whose status write then conflicts still failed: it must
	// return the failure's own result, not the quiet conflict requeue. A
	// RequeueAfter result makes controller-runtime forget the item and reset
	// its exponential backoff, so a stale cache would turn the backoff of a
	// persistent failure into a conflictRequeueDelay loop.
	periodic := func(ac *v1alpha1.KrakenDAutoConfig) {
		ac.Spec.Trigger = v1alpha1.TriggerPeriodic
		ac.Spec.Periodic = &v1alpha1.PeriodicSpec{Interval: metav1.Duration{Duration: 5 * time.Minute}}
	}
	failFetch := func(f *mockFetcher, _ *mockCUEEvaluator) {
		f.result = nil
		f.err = fmt.Errorf("connection refused")
	}
	failSync := func(_ *mockFetcher, ce *mockCUEEvaluator) {
		ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	}
	tests := []struct {
		name          string
		trigger       func(ac *v1alpha1.KrakenDAutoConfig)
		fail          func(f *mockFetcher, ce *mockCUEEvaluator)
		failEndpoints bool
		wantErr       string        // substring; "" means no error
		wantRequeue   time.Duration // result.RequeueAfter
	}{
		{name: "OnChange fetch failure", fail: failFetch, wantErr: "connection refused"},
		{name: "OnChange synced failure", fail: failSync, wantErr: "spec.overrides reference operationIds"},
		{name: "OnChange endpoint write failure", failEndpoints: true, wantErr: "reconciling endpoints"},
		{name: "Periodic fetch failure", trigger: periodic, fail: failFetch, wantRequeue: 5 * time.Minute},
		{name: "Periodic synced failure", trigger: periodic, fail: failSync, wantRequeue: 5 * time.Minute},
		{
			name: "Periodic endpoint write failure", trigger: periodic, failEndpoints: true,
			wantErr: "reconciling endpoints",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := testCUEDefinitionsCM()
			// Never synced, so the inputs differ from the (empty) recorded
			// checksum and the input warnings below are buffered.
			ac := testAutoConfig()
			ac.Status.Phase = v1alpha1.AutoConfigPhasePending
			if tt.trigger != nil {
				tt.trigger(ac)
			}
			funcs := conflictStatusWrites()
			if tt.failEndpoints {
				funcs.Create = failEndpointCreates(func(string) error {
					return fmt.Errorf("simulated webhook rejection")
				}).Create
			}
			c := fakeClientBuilder().
				WithObjects(ac, cm).
				WithStatusSubresource(ac).
				WithInterceptorFuncs(funcs).
				Build()
			f, ce, fi, g := defaultMocks()
			// The resolver warns about a missing external pointer, a warning
			// buffered before every failure below but the fetch's, and the
			// inputs changed: it must not be recorded by a reconcile whose
			// status write then conflicts.
			f.result = &autoconfig.FetchResult{
				Data: []byte(`{"paths":{"/x":{"get":{"responses":{"200":{"$ref":"common.json#/Missing"}}}}}}`),
			}
			f.byURL = map[string]mockFetchOutcome{
				"https://example.com/common.json": {result: &autoconfig.FetchResult{Data: []byte(`{}`)}},
			}
			if tt.fail != nil {
				tt.fail(f, ce)
			}
			rec := fakeRecorder()
			r := newACReconciler(c, f, ce, fi, g)
			r.Recorder = rec

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
			})
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("expected no error, got %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("expected the sync error containing %q, got %v", tt.wantErr, err)
			}
			if result.RequeueAfter != tt.wantRequeue {
				t.Errorf("expected requeue after %v, got %v", tt.wantRequeue, result.RequeueAfter)
			}
			if events := drainEvents(rec); len(events) != 0 {
				t.Errorf("expected no events, got %v", events)
			}
		})
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
	g.output.Skipped = []autoconfig.OperationIssue{duplicateListUsers()}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

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
	g.output.Skipped = []autoconfig.OperationIssue{duplicateListUsers()}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

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
	g.output.Skipped = []autoconfig.OperationIssue{duplicateListUsers()}
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

func TestAutoConfigReconcile_RecordsSkippedOperationsAndNotes(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	// An additional endpoint on the generated route replaces it.
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/api/users", Method: "GET", Host: "http://override"},
	}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	g.output.Skipped = []autoconfig.OperationIssue{duplicateListUsers()}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := getAC(t, c, ac)
	wantSkipped := []v1alpha1.OperationStatus{{
		Method: "GET", Path: "/v2/users", OperationID: "listUsers",
		Reason:  v1alpha1.ReasonDuplicateOperationId,
		Message: `operationId "listUsers" is already used by GET /api/users`,
	}}
	if !slices.Equal(updated.Status.Skipped, wantSkipped) || updated.Status.SkippedOperations != 1 {
		t.Errorf("skipped = %+v (count %d), want %+v", updated.Status.Skipped,
			updated.Status.SkippedOperations, wantSkipped)
	}
	wantWarnings := []string{`Additional endpoint "/api/users:GET" overrides a spec-derived endpoint`}
	if !slices.Equal(updated.Status.Warnings, wantWarnings) {
		t.Errorf("warnings = %q, want %q", updated.Status.Warnings, wantWarnings)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	wantMsg := "Generated 1 endpoints; 1 operations skipped (see status.skipped); 1 spec warnings (see status.warnings)"
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Message != wantMsg {
		t.Errorf("expected Synced True %q, got %+v", wantMsg, cond)
	}
}

// skippedAndWarnedAutoConfig is an AutoConfig whose sync lists one skipped
// operation and one warning, with the mocks that produce them.
func skippedAndWarnedAutoConfig() (
	*v1alpha1.KrakenDAutoConfig, *mockFetcher, *mockCUEEvaluator, *mockFilter, *mockGenerator,
) {
	ac := testAutoConfig()
	ac.Spec.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{
		{Endpoint: "/api/users", Method: "GET", Host: "http://override"},
	}
	f, ce, fi, g := defaultMocks()
	g.output.Skipped = []autoconfig.OperationIssue{duplicateListUsers()}
	return ac, f, ce, fi, g
}

func TestAutoConfigReconcile_ListContentAloneRewritesStatus(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac, f, ce, fi, g := skippedAndWarnedAutoConfig()
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	r := newACReconciler(c, f, ce, fi, g)
	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	synced := getAC(t, c, ac)

	// Change what the lists say but not how many entries they hold, so the
	// Synced message, the counts and the phase stay as they were.
	stale := synced.DeepCopy()
	stale.Status.Skipped[0].Message = "stale"
	stale.Status.Warnings[0] = "stale"
	if err := c.Status().Update(context.Background(), stale); err != nil {
		t.Fatalf("tampering with status: %v", err)
	}
	counts = writeCounts{}

	if _, err := reconcileAC(r, stale); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if counts.statusUpdates != 1 {
		t.Errorf("status updates = %d, want 1 to restore the lists", counts.statusUpdates)
	}
	got := getAC(t, c, ac)
	if !slices.Equal(got.Status.Skipped, synced.Status.Skipped) ||
		!slices.Equal(got.Status.Warnings, synced.Status.Warnings) {
		t.Errorf("lists = %+v / %q, want %+v / %q", got.Status.Skipped, got.Status.Warnings,
			synced.Status.Skipped, synced.Status.Warnings)
	}
}

func TestAutoConfigReconcile_ResolverWarningsPersistAndEmitOnce(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	// The external document lacks the referenced pointer: a resolver warning.
	spec := []byte(`{"paths":{"/x":{"get":{"responses":{"200":{"$ref":"common.json#/Missing"}}}}}}`)
	f.result = &autoconfig.FetchResult{Data: spec}
	f.byURL = map[string]mockFetchOutcome{
		"https://example.com/common.json": {result: &autoconfig.FetchResult{Data: []byte(`{}`)}},
	}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := `failed to resolve external $ref "common.json#/Missing": pointer segment "Missing" not found`
	updated := getAC(t, c, ac)
	if !slices.Equal(updated.Status.Warnings, []string{want}) {
		t.Errorf("warnings = %q, want [%q]", updated.Status.Warnings, want)
	}
	if events := drainEvents(rec); !slices.Contains(events, "Warning "+v1alpha1.ReasonSpecWarning+" "+want) {
		t.Errorf("expected a SpecWarning event, got %v", events)
	}

	// Unchanged inputs: the warning stays in status, no new event.
	if _, err := reconcileAC(r, updated); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonSpecWarning) {
		t.Errorf("expected no SpecWarning event for unchanged inputs, got %v", events)
	}
	if got := getAC(t, c, ac).Status.Warnings; !slices.Equal(got, []string{want}) {
		t.Errorf("warnings after resync = %q", got)
	}
}

func TestAutoConfigReconcile_IdenticalSecondPassWritesNothing(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac, f, ce, fi, g := skippedAndWarnedAutoConfig()
	// A second warning, from the resolver, so the list has more than one
	// entry to order.
	f.result = &autoconfig.FetchResult{
		Data: []byte(`{"paths":{"/x":{"get":{"responses":{"200":{"$ref":"common.json#/Missing"}}}}}}`),
	}
	f.byURL = map[string]mockFetchOutcome{
		"https://example.com/common.json": {result: &autoconfig.FetchResult{Data: []byte(`{}`)}},
	}
	g.output.Skipped = append(g.output.Skipped, autoconfig.OperationIssue{
		Operation: autoconfig.Operation{Method: "GET", Path: "/v1/users", OperationID: "listUsers"},
		Reason:    v1alpha1.ReasonDuplicateOperationId,
		Message:   `operationId "listUsers" is already used by GET /api/users`,
	})
	// Unsupported methods come from the evaluator, not the generator.
	ce.output.Skipped = []autoconfig.OperationIssue{{
		Operation: autoconfig.Operation{Method: "HEAD", Path: "/api/users", OperationID: "headUsers"},
		Reason:    v1alpha1.ReasonUnsupportedMethod,
		Message:   "KrakenDEndpoint supports only GET, POST, PUT, PATCH, DELETE",
	}}
	// Two in-scope failed operations: the sync holds them, and an identical
	// pass must not rewrite the status or repeat the event.
	ce.output.Failed = []autoconfig.OperationIssue{{
		Operation: autoconfig.Operation{Method: "GET", Path: "/a", OperationID: "getA"},
		Reason:    v1alpha1.ReasonCUEEvaluationFailed,
		Message:   "boom",
	}, failedGetB()}
	// A third endpoint the gateway config check holds on every pass, for two
	// reasons that arrive in the opposite order on the second pass.
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getC", "/c"))
	reasons := []configcheck.Finding{{
		Endpoint: types.NamespacedName{Namespace: "default", Name: "test-ac-getc"}, Index: 0, Message: "first reason",
	}, {
		Endpoint: types.NamespacedName{Namespace: "default", Name: "test-ac-getc"}, Index: 0, Message: "second reason",
	}}
	checker := &fakeChecker{judge: func(replace []v1alpha1.KrakenDEndpoint) configcheck.Verdict {
		if slices.Contains(endpointNames(replace), "test-ac-getc") {
			return configcheck.Verdict{Findings: slices.Clone(reasons)}
		}
		return configcheck.Verdict{OK: true}
	}}
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first := getAC(t, c, ac)
	drainEvents(rec)
	if len(first.Status.Skipped) != 3 || len(first.Status.Warnings) != 2 {
		t.Fatalf("first pass skipped = %+v, warnings = %q; want 3 skipped and 2 warnings", first.Status.Skipped, first.Status.Warnings)
	}
	if len(first.Status.FailedOperations) != 3 {
		t.Fatalf("first pass failedOperations = %+v, want 3", first.Status.FailedOperations)
	}
	// The same issues arrive in the opposite order.
	slices.Reverse(ce.output.Skipped)
	slices.Reverse(ce.output.Failed)
	slices.Reverse(g.output.Skipped)
	slices.Reverse(reasons)
	counts = writeCounts{}

	if _, err := reconcileAC(r, first); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if counts != (writeCounts{}) {
		t.Errorf("an identical second pass wrote %+v, want no writes", counts)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("an identical second pass emitted %v, want no events", events)
	}
}

func TestAutoConfigReconcile_SharedBrokenRefEmitsOneSpecWarning(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	// Two operations reference the same missing pointer.
	spec := []byte(`{"paths":{` +
		`"/x":{"get":{"responses":{"200":{"$ref":"common.json#/Missing"}}}},` +
		`"/y":{"get":{"responses":{"200":{"$ref":"common.json#/Missing"}}}}}}`)
	f.result = &autoconfig.FetchResult{Data: spec}
	f.byURL = map[string]mockFetchOutcome{
		"https://example.com/common.json": {result: &autoconfig.FetchResult{Data: []byte(`{}`)}},
	}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	n := 0
	for _, ev := range drainEvents(rec) {
		if strings.HasPrefix(ev, "Warning "+v1alpha1.ReasonSpecWarning+" ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("SpecWarning events = %d, want 1 for one distinct warning", n)
	}
}

func TestAutoConfigReconcile_SyncedMessageCountsEveryDistinctWarning(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	// 45 operations, each referencing its own missing pointer.
	var paths []string
	for i := range 45 {
		paths = append(paths, fmt.Sprintf(
			`"/p%d":{"get":{"responses":{"200":{"$ref":"common.json#/Missing%d"}}}}`, i, i))
	}
	f.result = &autoconfig.FetchResult{Data: []byte(`{"paths":{` + strings.Join(paths, ",") + `}}`)}
	f.byURL = map[string]mockFetchOutcome{
		"https://example.com/common.json": {result: &autoconfig.FetchResult{Data: []byte(`{}`)}},
	}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	updated := getAC(t, c, ac)
	if len(updated.Status.Warnings) != maxStatusListLen {
		t.Errorf("listed warnings = %d, want the %d cap", len(updated.Status.Warnings), maxStatusListLen)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	want := "Generated 1 endpoints; 45 spec warnings (see status.warnings)"
	if cond == nil || cond.Message != want {
		t.Errorf("Synced = %+v, want message %q", cond, want)
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
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

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

func TestAutoConfigReconcile_SyncedGaugeZeroWhenFailureStatusWriteFails(t *testing.T) {
	// The failure is certain before its status write is attempted, so a
	// failed write (other than a Conflict) must not leave the gauge at the
	// last successful sync's 1.
	tests := []struct {
		name string
		fail func(f *mockFetcher, ce *mockCUEEvaluator)
	}{
		{
			name: "fetch failure",
			fail: func(f *mockFetcher, _ *mockCUEEvaluator) {
				f.result = nil
				f.err = fmt.Errorf("connection refused")
			},
		},
		{
			name: "synced failure",
			fail: func(_ *mockFetcher, ce *mockCUEEvaluator) {
				ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := testCUEDefinitionsCM()
			ac := testAutoConfig()
			ac.Name = fmt.Sprintf("metrics-failurewrite-%d-ac", i)
			ac.Status.Phase = v1alpha1.AutoConfigPhasePending
			c := fakeClientBuilder().
				WithObjects(ac, cm).
				WithStatusSubresource(ac).
				WithInterceptorFuncs(failFailureStatusWrites()).
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

			// Second reconcile fails, and so does its failure-status write.
			tt.fail(f, ce)
			if _, err := r.Reconcile(context.Background(), req); err == nil ||
				!strings.Contains(err.Error(), "simulated server error") {
				t.Fatalf("expected the failure-status write error, got %v", err)
			}

			if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 0 {
				t.Errorf("expected synced gauge 0 after a failed sync whose status write failed, got %v", got)
			}
		})
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

func TestAutoConfigReadiness(t *testing.T) {
	c := func(typ string, status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: status, Reason: reason, Message: reason + " message"}
	}
	tests := []struct {
		name       string
		conds      []metav1.Condition
		wantStatus metav1.ConditionStatus
		wantReason string
		wantPhase  v1alpha1.AutoConfigPhase
	}{
		{"not synced yet", nil, metav1.ConditionUnknown, "Pending", v1alpha1.AutoConfigPhasePending},
		{"spec fetch failed",
			[]metav1.Condition{c("SpecAvailable", metav1.ConditionFalse, "SpecFetchFailed"),
				c("Synced", metav1.ConditionFalse, "SpecFetchFailed")},
			metav1.ConditionFalse, "SpecFetchFailed", v1alpha1.AutoConfigPhaseError},
		{"sync failed after the fetch",
			[]metav1.Condition{c("SpecAvailable", metav1.ConditionTrue, "SpecFetched"),
				c("Synced", metav1.ConditionFalse, "UnmatchedOverride")},
			metav1.ConditionFalse, "UnmatchedOverride", v1alpha1.AutoConfigPhaseError},
		{"synced",
			[]metav1.Condition{c("SpecAvailable", metav1.ConditionTrue, "SpecFetched"),
				c("Synced", metav1.ConditionTrue, "Synced"),
				c("EndpointsReady", metav1.ConditionTrue, "AllEndpointsReady")},
			metav1.ConditionTrue, "Ready", v1alpha1.AutoConfigPhaseSynced},
		{"endpoints not ready",
			[]metav1.Condition{c("SpecAvailable", metav1.ConditionTrue, "SpecFetched"),
				c("Synced", metav1.ConditionTrue, "Synced"),
				c("EndpointsReady", metav1.ConditionFalse, "EndpointsNotReady")},
			metav1.ConditionFalse, "EndpointsNotReady", v1alpha1.AutoConfigPhaseSynced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, _ := autoConfigReady(tt.conds)
			if phase := autoConfigPhase(tt.conds); status != tt.wantStatus || reason != tt.wantReason ||
				phase != tt.wantPhase {
				t.Errorf("got %s/%s phase %s, want %s/%s phase %s",
					status, reason, phase, tt.wantStatus, tt.wantReason, tt.wantPhase)
			}
		})
	}
}

func TestAutoConfigReconcile_FailedSyncReportsReadyFalse(t *testing.T) {
	ac := testAutoConfig()
	ac.Generation = 2
	c := fakeClientBuilder().WithObjects(ac).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	f.result = nil
	f.err = fmt.Errorf("connection refused")
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
	}); err == nil {
		t.Fatal("expected the fetch error for an OnChange trigger")
	}
	var stored v1alpha1.KrakenDAutoConfig
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ac), &stored); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if stored.Status.Phase != v1alpha1.AutoConfigPhaseError || stored.Status.ObservedGeneration != 2 ||
		ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonSpecFetchFailed {
		t.Errorf("phase %q, observedGeneration %d, Ready %+v; want Error, 2, False/SpecFetchFailed",
			stored.Status.Phase, stored.Status.ObservedGeneration, ready)
	}
}

func TestNewAutoConfigRateLimiter_CapsBackoffAtResyncInterval(t *testing.T) {
	limiter := newAutoConfigRateLimiter()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-ac"}}
	if first := limiter.When(req); first != 5*time.Millisecond {
		t.Fatalf("first delay = %v, want 5ms", first)
	}
	var delay time.Duration
	for range 30 {
		delay = limiter.When(req)
	}
	if delay != defaultResyncInterval {
		t.Fatalf("delay after 30 failures = %v, want the %v cap", delay, defaultResyncInterval)
	}
}

func TestAutoConfigReconcile_ReportsUnsupportedMethodsInScope(t *testing.T) {
	head := autoconfig.OperationIssue{
		Operation: autoconfig.Operation{
			Method: "HEAD", Path: "/api/users", OperationID: "headUsers", Tags: []string{"internal"},
		},
		Reason:  v1alpha1.ReasonUnsupportedMethod,
		Message: "KrakenDEndpoint supports only GET, POST, PUT, PATCH, DELETE",
	}
	for name, tc := range map[string]struct {
		filter *v1alpha1.FilterSpec
		want   int
	}{
		"reported":               {filter: nil, want: 1},
		"excluded by the filter": {filter: &v1alpha1.FilterSpec{IncludeMethods: []string{"GET"}}, want: 0},
		"excluded by tag":        {filter: &v1alpha1.FilterSpec{ExcludeTags: []string{"internal"}}, want: 0},
		"excluded by operationId": {
			filter: &v1alpha1.FilterSpec{ExcludeOperationIds: []string{"headUsers"}}, want: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cm := testCUEDefinitionsCM()
			ac := testAutoConfig()
			ac.Spec.Filter = tc.filter
			c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
			f, ce, _, g := defaultMocks()
			ce.output.Skipped = []autoconfig.OperationIssue{head}
			r := newACReconciler(c, f, ce, &mockFilter{}, g)
			r.Filter = autoconfig.NewFilter()

			if _, err := reconcileAC(r, ac); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := getAC(t, c, ac).Status.Skipped; len(got) != tc.want {
				t.Errorf("skipped = %+v, want %d entries", got, tc.want)
			}
		})
	}
}

func TestAutoConfigReconcile_InScopeJudgesEachOperationOnItsOwn(t *testing.T) {
	// After a stripPathPrefix two operations can share a path and method.
	ac := testAutoConfig()
	ac.Spec.Filter = &v1alpha1.FilterSpec{ExcludeOperationIds: []string{"headZ"}}
	var issues []autoconfig.OperationIssue
	for _, id := range []string{"headZ", "headV1"} {
		issues = append(issues, autoconfig.OperationIssue{
			Operation: autoconfig.Operation{Method: "HEAD", Path: "/z", OperationID: id},
			Reason:    v1alpha1.ReasonUnsupportedMethod,
		})
	}
	r := &KrakenDAutoConfigReconciler{Filter: autoconfig.NewFilter()}

	got := r.inScope(ac, issues)

	if len(got) != 1 || got[0].OperationID != "headV1" {
		t.Errorf("in scope = %+v, want only headV1", got)
	}
}

// failedGetB is an evaluator failure for operation getB.
func failedGetB() autoconfig.OperationIssue {
	return autoconfig.OperationIssue{
		Operation: autoconfig.Operation{Method: "GET", Path: "/b", OperationID: "getB"},
		Reason:    v1alpha1.ReasonCUEEvaluationFailed,
		Message:   `time: missing unit in duration "30"`,
	}
}

// staleOwnedEndpoint returns an endpoint ac controls that the generator no
// longer produces: a sync that proceeds deletes it.
func staleOwnedEndpoint(t *testing.T, ac *v1alpha1.KrakenDAutoConfig, g *mockGenerator) *v1alpha1.KrakenDEndpoint {
	t.Helper()
	stale := ownedCopy(t, ac, g.output.Endpoints[0])
	stale.Name = "test-ac-stale"
	return stale
}

// assertEndpointKept fails unless the endpoint named name still exists.
func assertEndpointKept(t *testing.T, c client.Client, name string) {
	t.Helper()
	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &ep); err != nil {
		t.Errorf("expected endpoint %s kept, got %v", name, err)
	}
}

func TestAutoConfigReconcile_FailedOperationHoldsItsEndpointAndStaleEndpoints(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	lastGood := ownedCopy(t, ac, generatedEndpoint("getB", "/b"))
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	f, ce, fi, g := defaultMocks()
	ce.output.Failed = []autoconfig.OperationIssue{failedGetB()}
	c := fakeClientBuilder().WithObjects(ac, cm, lastGood, stale).WithStatusSubresource(ac).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	result, err := reconcileAC(r, ac)

	if err != nil || result.RequeueAfter != defaultResyncInterval {
		t.Fatalf("expected no error and the resync requeue, got %v, %+v", err, result)
	}
	for _, name := range []string{"test-ac-getb", "test-ac-old", "test-ac-listusers"} {
		if !endpointExists(t, c, name) {
			t.Errorf("expected endpoint %s to exist", name)
		}
	}
	want := []v1alpha1.OperationStatus{{
		Method: "GET", Path: "/b", OperationID: "getB",
		Reason: v1alpha1.ReasonCUEEvaluationFailed, Message: `time: missing unit in duration "30"`,
	}}
	updated := getAC(t, c, ac)
	if !slices.Equal(updated.Status.FailedOperations, want) {
		t.Errorf("failedOperations = %+v, want %+v", updated.Status.FailedOperations, want)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonOperationsFailed ||
		!strings.Contains(cond.Message, "GET /b (getB): CUEEvaluationFailed") {
		t.Errorf("expected Synced False/OperationsFailed naming GET /b, got %+v", cond)
	}
	if cond != nil && strings.Contains(cond.Message, "missing unit") {
		t.Errorf("the condition carries CUE error text: %q", cond.Message)
	}
	if ready := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionReady); ready == nil ||
		ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonOperationsFailed ||
		updated.Status.Phase != v1alpha1.AutoConfigPhaseError {
		t.Errorf("expected Ready False/OperationsFailed and phase Error, got %+v, %q", ready, updated.Status.Phase)
	}
	if got := testutil.ToFloat64(autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name)); got != 0 {
		t.Errorf("synced gauge = %v, want 0", got)
	}
	if !hasEventReason(drainEvents(rec), v1alpha1.ReasonOperationsFailed) {
		t.Error("expected an OperationsFailed event")
	}

	// The same failure again: no status change, no event, still no backoff.
	result, err = reconcileAC(r, updated)
	if err != nil || result.RequeueAfter != defaultResyncInterval {
		t.Fatalf("second reconcile: %v, %+v", err, result)
	}
	if hasEventReason(drainEvents(rec), v1alpha1.ReasonOperationsFailed) {
		t.Error("expected no OperationsFailed event for an unchanged failure")
	}
}

func TestAutoConfigReconcile_FailedOperationOutsideFilterDoesNotFail(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Spec.Filter = &v1alpha1.FilterSpec{ExcludePaths: []string{"/b"}}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, _, g := defaultMocks()
	ce.output.Failed = []autoconfig.OperationIssue{failedGetB()}
	r := newACReconciler(c, f, ce, &mockFilter{}, g)
	r.Filter = autoconfig.NewFilter()

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Synced True, got %+v", cond)
	}
}

func TestAutoConfigReconcile_FailedOperationOutsideFilterIsIgnored(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	ac.Spec.Filter = &v1alpha1.FilterSpec{ExcludePaths: []string{"/b"}}
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	f, ce, _, g := defaultMocks()
	ce.output.Failed = []autoconfig.OperationIssue{failedGetB()}
	c := fakeClientBuilder().WithObjects(ac, cm, stale).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, &mockFilter{}, g)
	r.Filter = autoconfig.NewFilter()

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if endpointExists(t, c, "test-ac-old") {
		t.Error("expected the stale endpoint deleted: the failed operation is filtered out")
	}
	updated := getAC(t, c, ac)
	if cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Synced True, got %+v", cond)
	}
	if len(updated.Status.FailedOperations) != 0 {
		t.Errorf("expected no failedOperations, got %+v", updated.Status.FailedOperations)
	}
}

func TestAutoConfigReconcile_FailedOperationKeepsSpecAndGeneratorNotes(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// A spec note from the external reference resolver, a note from the
	// generator, and an operation that failed: the sync holds it and still
	// reports both notes, as a clean sync does.
	f.result = &autoconfig.FetchResult{
		Data: []byte(`{"paths":{"/x":{"get":{"responses":{"200":{"$ref":"common.json#/Missing"}}}}}}`),
	}
	f.byURL = map[string]mockFetchOutcome{
		"https://example.com/common.json": {result: &autoconfig.FetchResult{Data: []byte(`{}`)}},
	}
	generatorNote := `schema reference "Ghost" (first used by GET /api/users) is not defined in components/schemas`
	g.output.Warnings = []string{generatorNote}
	ce.output.Failed = []autoconfig.OperationIssue{failedGetB()}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := getAC(t, c, ac)
	if cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced); cond == nil ||
		cond.Reason != v1alpha1.ReasonOperationsFailed {
		t.Fatalf("expected Synced False/OperationsFailed, got %+v", cond)
	}
	warnings := updated.Status.Warnings
	if len(warnings) != 2 || !slices.Contains(warnings, generatorNote) ||
		!slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, "Missing") }) {
		t.Errorf("status.warnings = %q, want the spec note and the generator note", warnings)
	}
	if events := drainEvents(rec); !slices.Contains(events, "Warning "+v1alpha1.ReasonSpecWarning+" "+generatorNote) {
		t.Errorf("expected the generator note as a SpecWarning event, got %v", events)
	}
}

func TestAutoConfigReconcile_RejectedEndpointKeepsStaleEndpoints(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b"))
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, stale).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{"test-ac-getb": invalidError("test-ac-getb")})).
		Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !endpointExists(t, c, "test-ac-old") {
		t.Error("expected the stale endpoint kept while an endpoint is rejected")
	}
	if !endpointExists(t, c, "test-ac-listusers") {
		t.Error("expected the healthy endpoint written")
	}
	if slices.Contains(ops, "delete test-ac-old") {
		t.Errorf("expected no delete, got %v", ops)
	}
}

func TestAutoConfigReconcile_TransientWriteErrorTakesPrecedenceOverHeldOperations(t *testing.T) {
	tests := map[string]struct {
		failed    []autoconfig.OperationIssue
		rejected  map[string]error
		endpoints []*v1alpha1.KrakenDEndpoint
	}{
		"a failed operation":  {failed: []autoconfig.OperationIssue{failedGetB()}},
		"a rejected endpoint": {rejected: map[string]error{"test-ac-getb": invalidError("test-ac-getb")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cm := testCUEDefinitionsCM()
			ac := syncedAutoConfig(cm)
			f, ce, fi, g := defaultMocks()
			g.output.Endpoints = append(g.output.Endpoints, tt.endpoints...)
			ce.output.Failed = tt.failed
			errFor := map[string]error{"test-ac-listusers": apierrors.NewInternalError(errors.New("boom"))}
			maps.Copy(errFor, tt.rejected)
			var ops []string
			c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
				WithInterceptorFuncs(recordEndpointWrites(&ops, errFor)).Build()
			r := newACReconciler(c, f, ce, fi, g)

			if _, err := reconcileAC(r, ac); err == nil {
				t.Fatal("expected the transient write error to be returned for backoff")
			}
			cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonEndpointReconcileFailed {
				t.Errorf("expected Synced False/EndpointReconcileFailed, got %+v", cond)
			}
		})
	}
}

func TestAutoConfigReconcile_IdenticalPassWithRejectedEndpointsWritesNoStatus(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// Six endpoints the API server rejects with a message longer than the
	// status keeps: the list is deterministic, bounded and unchanged on the
	// next identical pass.
	errFor := map[string]error{}
	for i := range 6 {
		opID := fmt.Sprintf("op%d", i)
		ep := generatedEndpoint(opID, fmt.Sprintf("/r%d", i))
		g.output.Endpoints = append(g.output.Endpoints, ep)
		ce.output.OperationIDs[fmt.Sprintf("/r%d:GET", i)] = opID
		errFor[ep.Name] = apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "KrakenDEndpoint"},
			ep.Name, field.ErrorList{field.Invalid(field.NewPath("spec", "endpoints").Index(0), strings.Repeat("v", 400), "rejected")})
	}
	var ops []string
	var statusUpdates int
	funcs := recordEndpointWrites(&ops, errFor)
	funcs.SubResourceUpdate = func(
		ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption,
	) error {
		statusUpdates++
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).WithInterceptorFuncs(funcs).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first := getAC(t, c, ac)
	drainEvents(rec)
	if len(first.Status.FailedOperations) != 6 {
		t.Fatalf("failedOperations = %+v, want 6", first.Status.FailedOperations)
	}
	for _, op := range first.Status.FailedOperations {
		if len(op.Message) > 256 {
			t.Errorf("message of %s is %d bytes, want at most 256", op.Endpoint, len(op.Message))
		}
	}
	statusUpdates = 0

	result, err := reconcileAC(r, first)

	if err != nil || result.RequeueAfter != defaultResyncInterval {
		t.Fatalf("second reconcile: %v, %+v", err, result)
	}
	if statusUpdates != 0 {
		t.Errorf("an identical second pass wrote status %d times, want none", statusUpdates)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("an identical second pass emitted %v, want no events", events)
	}
}

func TestAutoConfigReconcile_RejectionCauseIsLoggedInFullOnceWhenItChanges(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b"))
	ce.output.OperationIDs["/b:GET"] = "getB"
	cause := strings.Repeat("c", 600)
	rejected := apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "KrakenDEndpoint"},
		"test-ac-getb", field.ErrorList{field.Invalid(field.NewPath("spec", "endpoints").Index(0), "x", cause)})
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{"test-ac-getb": rejected})).Build()
	r := newACReconciler(c, f, ce, fi, g)
	var logged []string
	ctx := logf.IntoContext(context.Background(), funcr.New(func(prefix, args string) {
		logged = append(logged, prefix+args)
	}, funcr.Options{}))
	full := func() int {
		n := 0
		for _, line := range logged {
			if strings.Contains(line, cause) {
				n++
			}
		}
		return n
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ac)}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if n := full(); n != 1 {
		t.Fatalf("expected the full rejection logged once on the first pass, got %d in %q", n, logged)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ac)}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if n := full(); n != 1 {
		t.Errorf("expected no log on an identical second pass, got %d", n)
	}
}

func TestAutoConfigReconcile_FailedOperationsAreListedSortedAndCapped(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	// Seven failures in reverse order: the status lists them by path,
	// whatever order the evaluator reports them.
	for i := 7; i >= 1; i-- {
		ce.output.Failed = append(ce.output.Failed, autoconfig.OperationIssue{
			Operation: autoconfig.Operation{Method: "GET", Path: fmt.Sprintf("/p%d", i), OperationID: fmt.Sprintf("op%d", i)},
			Reason:    v1alpha1.ReasonCUEEvaluationFailed,
			Message:   "boom",
		})
	}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var paths []string
	for _, op := range getAC(t, c, ac).Status.FailedOperations {
		paths = append(paths, op.Path)
	}
	if want := []string{"/p1", "/p2", "/p3", "/p4", "/p5", "/p6", "/p7"}; !slices.Equal(paths, want) {
		t.Errorf("failedOperations paths = %v, want %v", paths, want)
	}
	// The condition names the first five by path, then counts the rest.
	want := "7 operations failed; they keep their last-synced endpoints and no stale endpoint is deleted until " +
		"they recover (see status.failedOperations): GET /p1 (op1): CUEEvaluationFailed; " +
		"GET /p2 (op2): CUEEvaluationFailed; GET /p3 (op3): CUEEvaluationFailed; " +
		"GET /p4 (op4): CUEEvaluationFailed; GET /p5 (op5): CUEEvaluationFailed; and 2 more"
	cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Message != want {
		t.Errorf("Synced = %+v, want message %q", cond, want)
	}
}

func TestAutoConfigReconcile_FailedOperationsStatusListIsCapped(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	for i := range 25 {
		ce.output.Failed = append(ce.output.Failed, autoconfig.OperationIssue{
			Operation: autoconfig.Operation{Method: "GET", Path: fmt.Sprintf("/p%02d", i), OperationID: fmt.Sprintf("op%02d", i)},
			Reason:    v1alpha1.ReasonCUEEvaluationFailed,
			Message:   strings.Repeat("x", 400),
		})
	}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	updated := getAC(t, c, ac)
	if got := len(updated.Status.FailedOperations); got != 20 {
		t.Errorf("failedOperations has %d entries, want 20", got)
	}
	for _, op := range updated.Status.FailedOperations {
		if len(op.Message) > 256 {
			t.Errorf("message of %s is %d bytes, want at most 256", op.Path, len(op.Message))
		}
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || !strings.HasPrefix(cond.Message, "25 operations failed;") {
		t.Errorf("expected the condition to count all 25 failures, got %+v", cond)
	}
}

func TestAutoConfigReconcile_RemappedFailedOperationInFilterIsHeld(t *testing.T) {
	// A HEAD operation that fails CUE is remapped by an override to GET
	// /v2/users: the failure carries that route, so the include filter keeps
	// it and the sync holds it instead of deleting the existing /v2/users
	// endpoint as stale.
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	ac.Spec.Filter = &v1alpha1.FilterSpec{IncludePaths: []string{"/v2/*"}}
	f, ce, _, g := defaultMocks()
	stale := staleOwnedEndpoint(t, ac, g)
	c := fakeClientBuilder().WithObjects(ac, cm, stale).WithStatusSubresource(ac).Build()
	ce.output.Failed = []autoconfig.OperationIssue{{
		Operation: autoconfig.Operation{Method: "GET", Path: "/v2/users", OperationID: "headUsers"},
		Reason:    v1alpha1.ReasonCUEEvaluationFailed,
		Message:   "boom",
	}}
	r := newACReconciler(c, f, ce, &mockFilter{}, g)
	r.Filter = autoconfig.NewFilter()

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertEndpointKept(t, c, stale.Name)
	if got := getAC(t, c, ac).Status.FailedOperations; len(got) != 1 || got[0].OperationID != "headUsers" {
		t.Errorf("failedOperations = %+v, want headUsers", got)
	}
}

func TestAutoConfigReconcile_FailedOperationWithUnknownMethodKeepsEndpoint(t *testing.T) {
	// The evaluator could not tell the method of the failed operation: it
	// reports it Failed with no method, and the sync holds it instead of
	// deleting the operation's existing endpoint.
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	existing := staleOwnedEndpoint(t, ac, g)
	c := fakeClientBuilder().WithObjects(ac, cm, existing).WithStatusSubresource(ac).Build()
	ce.output.Failed = []autoconfig.OperationIssue{{
		Operation: autoconfig.Operation{Path: "/users/{id}", OperationID: "getUser"},
		Reason:    v1alpha1.ReasonCUEEvaluationFailed,
		Message:   "conflicting values",
	}}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertEndpointKept(t, c, existing.Name)
	if got := getAC(t, c, ac).Status.FailedOperations; len(got) != 1 || got[0].Path != "/users/{id}" {
		t.Errorf("failedOperations = %+v, want /users/{id}", got)
	}
}

func invalidError(name string) error {
	return apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: "KrakenDEndpoint"},
		name, field.ErrorList{field.Invalid(field.NewPath("spec", "endpoints").Index(0), "x", "rejected")})
}

func TestAutoConfigReconcile_InvalidEndpointIsHeldNotRetried(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b"))
	ce.output.OperationIDs["/b:GET"] = "getB"
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{"test-ac-getb": invalidError("test-ac-getb")})).
		Build()
	r := newACReconciler(c, f, ce, fi, g)

	result, err := reconcileAC(r, ac)

	if err != nil || result.RequeueAfter != defaultResyncInterval {
		t.Fatalf("expected no error and the resync requeue, got %v, %+v", err, result)
	}
	if !endpointExists(t, c, "test-ac-listusers") {
		t.Error("expected the valid endpoint written")
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-getb" || failed[0].Reason != v1alpha1.ReasonEndpointRejected ||
		failed[0].Method != "GET" || failed[0].Path != "/b" || failed[0].OperationID != "getB" {
		t.Errorf("failedOperations = %+v", failed)
	}
	if len(failed) == 1 && !strings.HasPrefix(failed[0].Message, "KrakenDEndpoint.gateway.krakend.io") {
		t.Errorf("expected the API server's message without the controller's wrap, got %q", failed[0].Message)
	}
}

func TestAutoConfigReconcile_EndpointControlledByAnotherIsHeldNotRetried(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	taken := g.output.Endpoints[0].DeepCopy()
	taken.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: "KrakenDAutoConfig", Name: "other", UID: "other-uid",
		Controller: ptr.To(true),
	}}
	c := fakeClientBuilder().WithObjects(ac, cm, taken).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)

	result, err := reconcileAC(r, ac)

	if err != nil || result.RequeueAfter != defaultResyncInterval {
		t.Fatalf("expected no error and the resync requeue, got %v, %+v", err, result)
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-listusers" ||
		failed[0].Reason != v1alpha1.ReasonEndpointRejected || failed[0].OperationID != "listUsers" {
		t.Errorf("failedOperations = %+v", failed)
	}
}

func TestAutoConfigReconcile_DereferencesParameterRefsBeforeEvaluation(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	f.result = &autoconfig.FetchResult{Data: []byte(`{"paths":{"/a":{"get":{"parameters":[` +
		`{"$ref":"#/components/parameters/Limit"},{"$ref":"#/components/parameters/Nope"}]}}},` +
		`"components":{"parameters":{"Limit":{"name":"limit","in":"query"}}}}`)}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ce.gotInput == nil {
		t.Fatal("the evaluator was not called")
	}
	if !strings.Contains(string(ce.gotInput.SpecData), `"parameters":[{"in":"query","name":"limit"},`) {
		t.Errorf("expected the Limit parameter inlined into the operation, got %s", ce.gotInput.SpecData)
	}
	want := `parameter $ref "#/components/parameters/Nope" in GET /a cannot be resolved: ` +
		`pointer segment "Nope" not found`
	if got := getAC(t, c, ac).Status.Warnings; !slices.Contains(got, want) {
		t.Errorf("warnings = %q, want to contain %q", got, want)
	}
}

func TestAutoConfigReconcile_ParameterExpansionBeyondTheBodyLimitFailsTheSync(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	var paths []string
	for i := range 12 {
		paths = append(paths, fmt.Sprintf(
			`"/p%d":{"get":{"parameters":[{"$ref":"#/components/parameters/Big"}]}}`, i))
	}
	f.result = &autoconfig.FetchResult{Data: []byte(`{"paths":{` + strings.Join(paths, ",") +
		`},"components":{"parameters":{"Big":{"name":"big","in":"query","description":"` +
		strings.Repeat("x", 1<<20) + `"}}}}`)}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("expected the sync to fail")
	}
	if ce.called {
		t.Error("the evaluator must not run on an oversized spec")
	}
	cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonSpecFetchFailed {
		t.Errorf("expected Synced False with reason %s, got %+v", v1alpha1.ReasonSpecFetchFailed, cond)
	}
}

// configMapSpecWithExternalRef makes ac ConfigMap-sourced and f return a
// spec with one external $ref.
func configMapSpecWithExternalRef(ac *v1alpha1.KrakenDAutoConfig, f *mockFetcher) {
	ac.Spec.OpenAPI = v1alpha1.OpenAPISource{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "spec"}}
	f.result = &autoconfig.FetchResult{
		Data: []byte(`{"paths":{"/x":{"get":{"responses":{"200":{"$ref":"other.json#/R"}}}}}}`),
	}
}

const externalRefNote = `external $ref "other.json#/R" is not resolved: ` +
	"a ConfigMap-sourced spec cannot fetch other documents"

func TestAutoConfigReconcile_ReportsExternalRefsInConfigMapSpec(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	f, ce, fi, g := defaultMocks()
	configMapSpecWithExternalRef(ac, f)
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := getAC(t, c, ac).Status.Warnings; !slices.Equal(got, []string{externalRefNote}) {
		t.Errorf("warnings = %q, want [%q]", got, externalRefNote)
	}
}

func TestAutoConfigReconcile_SpecNotesEmittedBeforeUnmatchedFailure(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	f, ce, fi, g := defaultMocks()
	configMapSpecWithExternalRef(ac, f)
	ce.output.UnmatchedOverrides = []string{"WebhookStatus"}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("expected error for OnChange trigger, got nil")
	}
	wantNote := "Warning " + v1alpha1.ReasonSpecWarning + " " + externalRefNote
	wantUnmatched := "Warning UnmatchedOverride spec.overrides reference operationIds or backend indexes " +
		"not present in the OpenAPI spec: WebhookStatus"
	events := drainEvents(rec)
	noteIdx, unmatchedIdx := slices.Index(events, wantNote), slices.Index(events, wantUnmatched)
	if noteIdx < 0 || unmatchedIdx < 0 || noteIdx > unmatchedIdx {
		t.Errorf("expected %q before %q, got %v", wantNote, wantUnmatched, events)
	}
}

func TestAutoConfigReconcile_ManyExternalRefsStayBoundedAndQuiet(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	f, ce, fi, g := defaultMocks()
	configMapSpecWithExternalRef(ac, f)
	var refs []string
	for i := range 3 * maxStatusListLen {
		refs = append(refs, fmt.Sprintf(`"/p%03d":{"get":{"responses":{"200":{"$ref":"other.json#/R%03d"}}}}`, i, i))
	}
	rawSpec := []byte(`{"paths":{` + strings.Join(refs, ",") + `}}`)
	// fetchSpec rewrites the result's Data, so each pass gets its own copy.
	f.result = &autoconfig.FetchResult{Data: rawSpec}
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first := getAC(t, c, ac)
	if got := len(first.Status.Warnings); got != maxStatusListLen {
		t.Errorf("status warnings = %d, want %d", got, maxStatusListLen)
	}
	if !slices.IsSorted(first.Status.Warnings) {
		t.Errorf("status warnings are not sorted: %q", first.Status.Warnings)
	}
	specWarningEvents := 0
	for _, ev := range drainEvents(rec) {
		if strings.HasPrefix(ev, "Warning "+v1alpha1.ReasonSpecWarning+" ") {
			specWarningEvents++
		}
	}
	if specWarningEvents != maxStatusListLen {
		t.Errorf("SpecWarning events = %d, want %d", specWarningEvents, maxStatusListLen)
	}
	counts = writeCounts{}

	f.result = &autoconfig.FetchResult{Data: rawSpec}

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if counts != (writeCounts{}) {
		t.Errorf("an identical second pass wrote %+v, want no writes", counts)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("an identical second pass emitted %v, want no events", events)
	}
}

func TestAutoConfigReconcile_GeneratorWarningsPersistAndEmit(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	f, ce, fi, g := defaultMocks()
	g.output.Warnings = []string{
		`schema reference "Ghost" (first used by GET /api/users) is not defined in components/schemas`,
	}
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := getAC(t, c, ac).Status.Warnings; !slices.Equal(got, g.output.Warnings) {
		t.Errorf("warnings = %q, want %q", got, g.output.Warnings)
	}
	if events := drainEvents(rec); !slices.Contains(events, "Warning "+v1alpha1.ReasonSpecWarning+" "+g.output.Warnings[0]) {
		t.Errorf("expected a SpecWarning event, got %v", events)
	}
}

func TestAutoConfigReconcile_ManyUnresolvedSchemaRefsStayBoundedAndQuiet(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	f, ce, fi, g := defaultMocks()
	configMapSpecWithExternalRef(ac, f)
	var refs []string
	for i := range 15 {
		refs = append(refs, fmt.Sprintf(`"/p%03d":{"get":{"responses":{"200":{"$ref":"other.json#/R%03d"}}}}`, i, i))
	}
	rawSpec := []byte(`{"paths":{` + strings.Join(refs, ",") + `}}`)
	// fetchSpec rewrites the result's Data, so each pass gets its own copy.
	f.result = &autoconfig.FetchResult{Data: rawSpec}
	for i := range 3 * maxStatusListLen {
		g.output.Warnings = append(g.output.Warnings, fmt.Sprintf(
			"schema reference %q (first used by GET /api/users) is not defined in components/schemas",
			fmt.Sprintf("Ghost%03d", i)))
	}
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if got := len(getAC(t, c, ac).Status.Warnings); got != maxStatusListLen {
		t.Errorf("status warnings = %d, want %d", got, maxStatusListLen)
	}
	// A pass emits at most maxStatusListLen warning events in all, fetch
	// notes first, so the failure event of a failing pass keeps its budget.
	warningEvents := 0
	for _, ev := range drainEvents(rec) {
		if strings.HasPrefix(ev, "Warning ") {
			warningEvents++
		}
	}
	if warningEvents != maxStatusListLen {
		t.Errorf("warning events = %d, want %d", warningEvents, maxStatusListLen)
	}
	counts = writeCounts{}
	f.result = &autoconfig.FetchResult{Data: rawSpec}

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if counts != (writeCounts{}) {
		t.Errorf("an identical second pass wrote %+v, want no writes", counts)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("an identical second pass emitted %v, want no events", events)
	}
}

func TestAutoConfigReconcile_GeneratorNoteAlreadyEmittedAsFetchNoteIsNotRepeated(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	f, ce, fi, g := defaultMocks()
	configMapSpecWithExternalRef(ac, f)
	g.output.Warnings = []string{externalRefNote}
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	want := "Warning " + v1alpha1.ReasonSpecWarning + " " + externalRefNote
	n := 0
	for _, ev := range drainEvents(rec) {
		if ev == want {
			n++
		}
	}
	if n != 1 {
		t.Errorf("events carrying the note = %d, want 1", n)
	}
}

// Upgrading rewrites an endpoint that carries every component schema once, to
// its closure, and a pass after that writes nothing. It runs the real
// generator against the mock fetcher and evaluator.
func TestAutoConfigReconcile_UpgradeRewritesFullSchemaMapToClosureOnce(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := testAutoConfig()
	f, ce, fi, g := defaultMocks()
	specJSON := []byte(`{"paths":{},"components":{"schemas":{` +
		`"Pet":{"type":"object"},"Unused":{"type":"object"}}}}`)
	f.result = &autoconfig.FetchResult{Data: specJSON}
	ce.output.Entries[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"documentation/openapi":{"response_definition":{"200":{"ref":"Pet"}}}}`),
	}
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Generator = autoconfig.NewGenerator()
	// fetchSpec rewrites the result's Data, so each pass gets its own copy.
	reconcileOnce := func() {
		t.Helper()
		f.result = &autoconfig.FetchResult{Data: slices.Clone(specJSON)}
		if _, err := reconcileAC(r, ac); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	endpoint := func() v1alpha1.KrakenDEndpoint {
		t.Helper()
		var list v1alpha1.KrakenDEndpointList
		if err := c.List(context.Background(), &list, client.InNamespace(ac.Namespace)); err != nil || len(list.Items) != 1 {
			t.Fatalf("listing endpoints: %v, %d items", err, len(list.Items))
		}
		return list.Items[0]
	}

	reconcileOnce()
	old := endpoint()
	old.Spec.ComponentSchemas = autoconfig.ExtractComponentSchemas(specJSON)
	if err := c.Update(context.Background(), &old); err != nil {
		t.Fatalf("storing the pre-upgrade endpoint: %v", err)
	}
	counts = writeCounts{}

	reconcileOnce()
	if counts.updates != 1 {
		t.Errorf("the upgrade pass made %d endpoint updates, want 1", counts.updates)
	}
	if got := slices.Sorted(maps.Keys(endpoint().Spec.ComponentSchemas)); !slices.Equal(got, []string{"Pet"}) {
		t.Errorf("schemas after the upgrade = %v, want [Pet]", got)
	}
	counts = writeCounts{}

	reconcileOnce()
	if counts != (writeCounts{}) {
		t.Errorf("the pass after the upgrade wrote %+v, want no writes", counts)
	}
}

// generatedEndpoint returns the endpoint the generator produces for test-ac's
// GET operation opID at path.
func generatedEndpoint(opID, path string) *v1alpha1.KrakenDEndpoint {
	return &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ac-" + strings.ToLower(opID),
			Namespace: "default",
			Labels: map[string]string{
				autoconfig.LabelAutoConfig:    "test-ac",
				autoconfig.LabelAutoGenerated: "true",
			},
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "test-gw"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: path,
				Method:   "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: path}},
			}},
		},
	}
}

// endpointExists reports whether the endpoint name exists in default.
func endpointExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var ep v1alpha1.KrakenDEndpoint
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &ep)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("getting endpoint %s: %v", name, err)
	}
	return err == nil
}

func TestAutoConfigReconcile_AdoptsLabelledOrphansAndLeavesOthers(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	orphanDesired := g.output.Endpoints[0].DeepCopy() // test-ac-listusers, no owner
	orphanStale := generatedEndpoint("gone", "/gone")
	foreign := generatedEndpoint("foreign", "/foreign")
	foreign.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: "KrakenDAutoConfig", Name: "other", UID: "other-uid",
		Controller: ptr.To(true),
	}}
	oneLabel := generatedEndpoint("manual", "/manual")
	delete(oneLabel.Labels, autoconfig.LabelAutoGenerated)
	c := fakeClientBuilder().WithObjects(ac, cm, orphanDesired, orphanStale, foreign, oneLabel).
		WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var adopted v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(orphanDesired), &adopted); err != nil ||
		!metav1.IsControlledBy(&adopted, ac) {
		t.Errorf("expected %s adopted, got %v %+v", orphanDesired.Name, err, adopted.OwnerReferences)
	}
	if endpointExists(t, c, orphanStale.Name) {
		t.Errorf("expected the undesired labelled orphan %s adopted and deleted", orphanStale.Name)
	}
	for _, want := range []*v1alpha1.KrakenDEndpoint{foreign, oneLabel} {
		var got v1alpha1.KrakenDEndpoint
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(want), &got); err != nil {
			t.Errorf("expected %s kept: %v", want.Name, err)
			continue
		}
		if !equality.Semantic.DeepEqual(got.OwnerReferences, want.OwnerReferences) || !maps.Equal(got.Labels, want.Labels) {
			t.Errorf("%s changed: owners %+v labels %v", want.Name, got.OwnerReferences, got.Labels)
		}
	}
}

func TestAutoConfigReconcile_DeletesControlledEndpointWithStrippedLabels(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	stripped := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	stripped.Labels = nil
	f, ce, fi, g := defaultMocks()
	c := fakeClientBuilder().WithObjects(ac, cm, stripped).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if endpointExists(t, c, stripped.Name) {
		t.Error("expected the controlled endpoint deleted although its labels were removed")
	}
}

func TestAutoConfigReconcile_MergesManagedLabelsIntoExisting(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	current := ownedCopy(t, ac, g.output.Endpoints[0])
	current.Labels = map[string]string{"team": "payments", autoconfig.LabelAutoConfig: "test-ac"}
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm, current).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(current), &ep); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"team": "payments", autoconfig.LabelAutoConfig: "test-ac", autoconfig.LabelAutoGenerated: "true",
	}
	if !maps.Equal(ep.Labels, want) {
		t.Errorf("labels = %v, want %v", ep.Labels, want)
	}

	// Labels that contain the managed ones need no further write.
	counts = writeCounts{}
	if _, err := reconcileAC(r, getAC(t, c, ac)); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if counts.updates != 0 {
		t.Errorf("expected no endpoint update, got %+v", counts)
	}
}

func TestOwnedEndpointPredicate_PassesLabelChange(t *testing.T) {
	old := generatedEndpoint("a", "/a")
	relabelled := old.DeepCopy()
	relabelled.Labels = nil
	if !ownedEndpointPredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: relabelled}) {
		t.Error("expected a label change to pass the Owns predicate")
	}
}

func TestAutoConfigReconcile_NeverTouchesAnotherNamespacesEndpoints(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// A labelled orphan in another namespace is not adopted.
	orphan := generatedEndpoint("elsewhere", "/elsewhere")
	orphan.Namespace = "other"
	// An endpoint there carrying a controller reference with this
	// AutoConfig's UID is a forgery: owner references do not cross
	// namespaces, so it is neither listed nor deleted as stale.
	forged := generatedEndpoint("forged", "/forged")
	forged.Namespace = "other"
	forged.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: "KrakenDAutoConfig", Name: ac.Name, UID: ac.UID,
		Controller: ptr.To(true),
	}}
	c := fakeClientBuilder().WithObjects(ac, cm, orphan, forged).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, want := range []*v1alpha1.KrakenDEndpoint{orphan, forged} {
		var got v1alpha1.KrakenDEndpoint
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(want), &got); err != nil {
			t.Errorf("expected %s/%s kept: %v", want.Namespace, want.Name, err)
			continue
		}
		if !equality.Semantic.DeepEqual(got.OwnerReferences, want.OwnerReferences) {
			t.Errorf("%s/%s owner references changed: %+v", want.Namespace, want.Name, got.OwnerReferences)
		}
	}
}

func TestAutoConfigReconcile_DoesNotAdoptATerminatingOrphan(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	terminating := generatedEndpoint("dying", "/dying")
	terminating.Finalizers = []string{"test/hold"}
	c := fakeClientBuilder().WithObjects(ac, cm, terminating).WithStatusSubresource(ac).Build()
	if err := c.Delete(context.Background(), terminating); err != nil {
		t.Fatalf("delete: %v", err)
	}
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(terminating), &got); err != nil {
		t.Fatalf("expected the terminating endpoint kept: %v", err)
	}
	if got.DeletionTimestamp.IsZero() || len(got.OwnerReferences) != 0 {
		t.Errorf("expected a terminating endpoint left alone, got %+v", got.ObjectMeta)
	}
}

// recordEndpointWrites records every endpoint create, update and delete as
// "<verb> <name>" in ops, and fails creates and updates of the endpoints
// errFor names with that error.
func recordEndpointWrites(ops *[]string, errFor map[string]error) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
				*ops = append(*ops, "create "+obj.GetName())
				if err := errFor[obj.GetName()]; err != nil {
					return err
				}
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
				*ops = append(*ops, "update "+obj.GetName())
				if err := errFor[obj.GetName()]; err != nil {
					return err
				}
			}
			return c.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*v1alpha1.KrakenDEndpoint); ok {
				*ops = append(*ops, "delete "+obj.GetName())
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
}

func TestAutoConfigReconcile_WritesBeforeDeletingStale(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	// getUser was renamed getUserById upstream: same route, new name.
	old := ownedCopy(t, ac, generatedEndpoint("getUser", "/users/{id}"))
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = []*v1alpha1.KrakenDEndpoint{generatedEndpoint("getUserById", "/users/{id}")}
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, old).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, nil)).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if want := []string{"create test-ac-getuserbyid", "delete test-ac-getuser"}; !slices.Equal(ops, want) {
		t.Errorf("endpoint writes = %v, want %v", ops, want)
	}
}

func TestAutoConfigReconcile_AttemptsEveryWriteAndKeepsStaleOnFailure(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = []*v1alpha1.KrakenDEndpoint{
		generatedEndpoint("a", "/a"), generatedEndpoint("b", "/b"), generatedEndpoint("c", "/c"),
	}
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, stale).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{"test-ac-b": errors.New("etcd timeout")})).
		Build()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := reconcileAC(r, ac)

	if err == nil || !strings.Contains(err.Error(), "upserting endpoint test-ac-b: etcd timeout") {
		t.Fatalf("expected the aggregated write error, got %v", err)
	}
	for name, want := range map[string]bool{
		"test-ac-a": true, "test-ac-b": false, "test-ac-c": true, "test-ac-old": true,
	} {
		if got := endpointExists(t, c, name); got != want {
			t.Errorf("endpoint %s exists = %v, want %v", name, got, want)
		}
	}
	cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonEndpointReconcileFailed || !strings.Contains(cond.Message, "test-ac-b") {
		t.Errorf("expected Synced False/EndpointReconcileFailed naming test-ac-b, got %+v", cond)
	}
}

func TestAutoConfigReconcile_RacedWriteDoesNotStopOtherWrites(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = []*v1alpha1.KrakenDEndpoint{generatedEndpoint("a", "/a"), generatedEndpoint("b", "/b")}
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{
			"test-ac-a": conflictError("krakendendpoints", "test-ac-a"),
		})).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	result, err := reconcileAC(r, ac)

	assertQuietRequeue(t, result, err, rec)
	if !endpointExists(t, c, "test-ac-b") {
		t.Error("expected test-ac-b written despite the race on test-ac-a")
	}
}

func TestAutoConfigReconcile_FailedDeleteDoesNotStopOtherDeletes(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	c := fakeClientBuilder().WithObjects(ac, cm,
		ownedCopy(t, ac, generatedEndpoint("old1", "/old1")), ownedCopy(t, ac, generatedEndpoint("old2", "/old2")),
	).WithStatusSubresource(ac).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if obj.GetName() == "test-ac-old1" {
				return errors.New("etcd timeout")
			}
			return c.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := reconcileAC(r, ac)

	if err == nil || !strings.Contains(err.Error(), "deleting endpoint test-ac-old1: etcd timeout") {
		t.Fatalf("expected the delete error, got %v", err)
	}
	if endpointExists(t, c, "test-ac-old2") {
		t.Error("expected test-ac-old2 deleted despite the failed delete of test-ac-old1")
	}
	cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonEndpointReconcileFailed {
		t.Errorf("expected Synced False/EndpointReconcileFailed, got %+v", cond)
	}
}

func TestAutoConfigReconcile_AdoptionFailureStillWritesAndKeepsStale(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	orphan := generatedEndpoint("orphan", "/orphan")
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, stale, orphan).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{"test-ac-orphan": errors.New("etcd timeout")})).
		Build()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := reconcileAC(r, ac)

	if err == nil || !strings.Contains(err.Error(), "adopting endpoint test-ac-orphan: etcd timeout") {
		t.Fatalf("expected the adoption error, got %v", err)
	}
	if !endpointExists(t, c, "test-ac-listusers") {
		t.Error("expected the desired endpoint written despite the failed adoption")
	}
	for _, name := range []string{"test-ac-old", "test-ac-orphan"} {
		if !endpointExists(t, c, name) {
			t.Errorf("expected %s kept: a failed adoption must not delete anything", name)
		}
	}
}

func TestAutoConfigReconcile_DesiredOrphanIsReownedWithOneWrite(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// A labelled orphan the pass desires, with a spec that differs: adopting
	// it and then writing it would cost two updates.
	orphan := generatedEndpoint("listUsers", "/old-path")
	var counts writeCounts
	c := fakeClientBuilder().WithObjects(ac, cm, orphan).WithStatusSubresource(ac).
		WithInterceptorFuncs(countWrites(&counts)).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counts.updates != 1 || counts.creates != 0 {
		t.Errorf("expected one update of the desired orphan, got %+v", counts)
	}
	var got v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(orphan), &got); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&got, ac) || got.Spec.Endpoints[0].Endpoint != "/api/users" {
		t.Errorf("expected the orphan re-owned with the desired spec, got %+v", got)
	}
}

func TestAutoConfigReconcile_FailureMessageOrderIsStable(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	failing := map[string]error{
		"test-ac-a": errors.New("boom"), "test-ac-b": errors.New("boom"), "test-ac-c": errors.New("boom"),
	}
	var ops []string
	funcs := recordEndpointWrites(&ops, failing)
	// A cache lists in no particular order: hand back the labelled list reversed.
	funcs.List = func(
		ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption,
	) error {
		if err := c.List(ctx, list, opts...); err != nil {
			return err
		}
		if eps, ok := list.(*v1alpha1.KrakenDEndpointList); ok {
			slices.Reverse(eps.Items)
		}
		return nil
	}
	c := fakeClientBuilder().WithObjects(ac, cm,
		generatedEndpoint("a", "/a"), generatedEndpoint("b", "/b"), generatedEndpoint("c", "/c"),
	).WithStatusSubresource(ac).WithInterceptorFuncs(funcs).Build()
	r := newACReconciler(c, f, ce, fi, g)

	_, err := reconcileAC(r, ac)

	want := "reconciling endpoints: [adopting endpoint test-ac-a: boom, " +
		"adopting endpoint test-ac-b: boom, adopting endpoint test-ac-c: boom]"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

func TestAutoConfigReconcile_RaceMixedWithRealErrorFailsListingBoth(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = []*v1alpha1.KrakenDEndpoint{generatedEndpoint("a", "/a"), generatedEndpoint("b", "/b")}
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{
			"test-ac-a": conflictError("krakendendpoints", "test-ac-a"),
			"test-ac-b": errors.New("etcd timeout"),
		})).Build()
	r := newACReconciler(c, f, ce, fi, g)

	result, err := reconcileAC(r, ac)

	if err == nil || result.RequeueAfter != 0 {
		t.Fatalf("expected a backoff error, got %+v, %v", result, err)
	}
	for _, want := range []string{"upserting endpoint test-ac-a", "upserting endpoint test-ac-b: etcd timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected the error to list %q, got %v", want, err)
		}
	}
}

func TestAutoConfigReconcile_LongCUEErrorIsBoundedInStatusAndEvent(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	ce.output = nil
	// A whole-evaluation error joining the message of every one of 300
	// operations is about 25 KB.
	lines := make([]string, 300)
	for i := range lines {
		lines[i] = fmt.Sprintf("operation%03d: field is incompatible with the policy definition at path %s",
			i, strings.Repeat("x", 20))
	}
	ce.err = errors.New(strings.Join(lines, "\n"))
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("expected the evaluation error to be returned")
	}

	got := getAC(t, c, ac)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonCUEEvaluationFailed || len(cond.Message) > maxConditionMessageBytes {
		t.Fatalf("expected Synced False/CUEEvaluationFailed within %d bytes, got %+v", maxConditionMessageBytes, cond)
	}
	events := drainEvents(rec)
	if want := "Warning CUEEvaluationFailed " + cond.Message; !slices.Contains(events, want) {
		t.Errorf("expected the Warning event to carry the truncated message, got %d events", len(events))
	}
}

func TestAutoConfigReconcile_ManyFailedWritesAreBoundedAndStatusWritten(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	failing := make(map[string]error, 200)
	g.output.Endpoints = nil
	for i := range 200 {
		ep := generatedEndpoint(fmt.Sprintf("op%03d", i), fmt.Sprintf("/op%03d", i))
		g.output.Endpoints = append(g.output.Endpoints, ep)
		failing[ep.Name] = errors.New(strings.Repeat("e", 200))
	}
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, failing)).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	_, err := reconcileAC(r, ac)

	if err == nil || !strings.Contains(err.Error(), "test-ac-op199") {
		t.Fatalf("expected the returned error to carry every failure, got %v", err)
	}

	if len(ops) != 200 {
		t.Errorf("expected all 200 writes attempted, got %d", len(ops))
	}
	got := getAC(t, c, ac)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionSynced)
	if got.Status.Phase != v1alpha1.AutoConfigPhaseError || cond == nil ||
		cond.Reason != v1alpha1.ReasonEndpointReconcileFailed || len(cond.Message) > maxConditionMessageBytes {
		t.Fatalf("expected phase Error and a Synced message within %d bytes, got %q %+v",
			maxConditionMessageBytes, got.Status.Phase, cond)
	}
	if n := strings.Count(cond.Message, "upserting endpoint"); n != 5 || !strings.HasSuffix(cond.Message, "; and 195 more") {
		t.Errorf("expected 5 failures named and a tail of 195 more, got %d in %q", n, cond.Message)
	}
	events := drainEvents(rec)
	if want := "Warning EndpointReconcileFailed " + cond.Message; !slices.Contains(events, want) {
		t.Errorf("expected the Warning event to carry the truncated message, got %d events", len(events))
	}
}

func TestAutoConfigReconcile_OrphanGoneBeforeAdoptionIsNotAFailure(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	orphan := generatedEndpoint("orphan", "/orphan")
	gone := apierrors.NewNotFound(
		schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "krakendendpoints"}, orphan.Name)
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, stale, orphan).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{orphan.Name: gone})).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("expected an orphan deleted before its adoption to be nothing to do, got %v", err)
	}
	if endpointExists(t, c, "test-ac-old") {
		t.Error("expected the stale endpoint deleted: the vanished orphan is not a failure")
	}
}

func TestAutoConfigReconcile_ConflictAndAlreadyExistsRequeueQuietly(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = []*v1alpha1.KrakenDEndpoint{generatedEndpoint("a", "/a"), generatedEndpoint("b", "/b")}
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, map[string]error{
			"test-ac-a": conflictError("krakendendpoints", "test-ac-a"),
			"test-ac-b": apierrors.NewAlreadyExists(
				schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "krakendendpoints"}, "test-ac-b"),
		})).Build()
	rec := fakeRecorder()
	r := newACReconciler(c, f, ce, fi, g)
	r.Recorder = rec

	result, err := reconcileAC(r, ac)

	assertQuietRequeue(t, result, err, rec)
	if len(ops) != 2 {
		t.Errorf("expected both writes attempted, got %v", ops)
	}
}

func TestEndpointFailuresError_IsNeverClassifiedAsARace(t *testing.T) {
	gr := schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "krakendendpoints"}
	failures := endpointFailuresError{errs: []error{
		errors.New("etcd timeout"),
		conflictError("krakendendpoints", "a"),
		apierrors.NewAlreadyExists(gr, "b"),
	}}

	if apierrors.IsConflict(failures) || apierrors.IsAlreadyExists(failures) {
		t.Error("expected a failed pass never to read as a lost race")
	}
}

func endpointNames(eps []v1alpha1.KrakenDEndpoint) []string {
	names := make([]string, len(eps))
	for i := range eps {
		names[i] = eps[i].Name
	}
	return names
}

func TestAutoConfigReconcile_PrecheckHoldsAttributedOperations(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b"))
	checker := &fakeChecker{verdicts: []configcheck.Verdict{{Findings: []configcheck.Finding{{
		Endpoint: types.NamespacedName{Namespace: "default", Name: "test-ac-getb"}, Index: 0,
		Message: "'timeout' time: unknown unit",
	}}}}}
	c := fakeClientBuilder().WithObjects(ac, cm, stale, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(checker.calls) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(checker.calls))
	}
	// Round 1 checks both candidates and models the stale endpoint gone.
	if names := endpointNames(checker.calls[0]); !slices.Equal(names,
		[]string{"test-ac-listusers", "test-ac-getb", "test-ac-old"}) {
		t.Errorf("round 1 replace set = %v", names)
	}
	if old := checker.calls[0][2]; len(old.Spec.Endpoints) != 0 || old.Spec.ComponentSchemas != nil {
		t.Errorf("expected an empty copy of the stale endpoint, got %+v", old.Spec)
	}
	// Round 2 re-checks what remains; test-ac-getb failed, so the stale
	// endpoint stays and is not modelled gone.
	if names := endpointNames(checker.calls[1]); !slices.Equal(names, []string{"test-ac-listusers"}) {
		t.Errorf("round 2 replace set = %v", names)
	}
	if endpointExists(t, c, "test-ac-getb") || !endpointExists(t, c, "test-ac-listusers") ||
		!endpointExists(t, c, "test-ac-old") {
		t.Error("expected listusers written, getb not written, old kept")
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-getb" ||
		failed[0].Reason != v1alpha1.ReasonConfigValidationFailed ||
		failed[0].Message != "'timeout' time: unknown unit" {
		t.Errorf("failedOperations = %+v", failed)
	}
}

func TestAutoConfigReconcile_PrecheckUnavailableWritesNothing(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	f, ce, fi, g := defaultMocks()
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, stale, testGateway()).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, nil)).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = &fakeChecker{err: errors.New("no free validator slot before the deadline")}

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("expected an error to retry with backoff")
	}
	if len(ops) != 0 {
		t.Errorf("expected no endpoint writes or deletes, got %v", ops)
	}
	cond := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Errorf("expected Synced False/ValidatorUnavailable, got %+v", cond)
	}
}

func TestAutoConfigReconcile_UnattributedCheckFailure(t *testing.T) {
	otherEndpoint := configcheck.Verdict{Findings: []configcheck.Finding{{
		Endpoint: types.NamespacedName{Namespace: "team-b", Name: "orders"}, Index: 0, Message: "wildcard conflict",
	}}}
	gatewayRoot := configcheck.Verdict{Findings: []configcheck.Finding{{Index: -1, Message: "'timeout' time: unknown unit"}}}
	for name, tc := range map[string]struct {
		failure, baseline configcheck.Verdict
		wantWrite         bool
	}{
		"another endpoint, gateway already broken: write": {otherEndpoint, otherEndpoint, true},
		"another endpoint, change breaks gateway: hold":   {otherEndpoint, configcheck.Verdict{OK: true}, false},
		"gateway root, change breaks gateway: hold":       {gatewayRoot, configcheck.Verdict{OK: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			cm := testCUEDefinitionsCM()
			ac := syncedAutoConfig(cm)
			f, ce, fi, g := defaultMocks()
			c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
			r := newACReconciler(c, f, ce, fi, g)
			r.Checker = &fakeChecker{verdicts: []configcheck.Verdict{tc.failure, tc.baseline}}

			if _, err := reconcileAC(r, ac); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := endpointExists(t, c, "test-ac-listusers"); got != tc.wantWrite {
				t.Errorf("endpoint written = %v, want %v", got, tc.wantWrite)
			}
			if tc.wantWrite {
				if failed := getAC(t, c, ac).Status.FailedOperations; len(failed) != 0 {
					t.Errorf("failedOperations = %+v, want none for a write", failed)
				}
			} else {
				failed := getAC(t, c, ac).Status.FailedOperations
				if len(failed) != 1 ||
					!strings.HasPrefix(failed[0].Message, "the change fails the gateway config check: ") {
					t.Errorf("failedOperations = %+v", failed)
				}
			}
		})
	}
}

func TestAttributeFindings_ReadsEndpointNotIndexAndKeepsTheLeastMessage(t *testing.T) {
	a, b := generatedEndpoint("a", "/a"), generatedEndpoint("b", "/b")
	got := attributeFindings([]configcheck.Finding{
		{Endpoint: types.NamespacedName{Namespace: "default", Name: "test-ac-a"}, Index: 0, Message: "second"},
		{Endpoint: types.NamespacedName{Namespace: "default", Name: "test-ac-a"}, Index: -1, Message: "first"},
		{Endpoint: types.NamespacedName{Namespace: "default", Name: "test-ac-b"}, Index: -1, Message: "entry unknown"},
		{Index: -1, Message: "gateway root"},
		{Endpoint: types.NamespacedName{Namespace: "team-b", Name: "orders"}, Index: 0, Message: "elsewhere"},
	}, []*v1alpha1.KrakenDEndpoint{a, b})

	if len(got) != 2 || got["test-ac-a"].message != "first" || got["test-ac-b"].message != "entry unknown" {
		t.Errorf("attributeFindings = %+v, want test-ac-a: first, test-ac-b: entry unknown", got)
	}
}

func TestAutoConfigReconcile_PrecheckHoldsWhatTheRoundsLeftUnchecked(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// Seven candidates; each round's check names the first one left, so five
	// rounds attribute five and leave two that no passing check covered.
	names := []string{"test-ac-listusers"}
	for _, id := range []string{"b", "c", "d", "e", "f", "g"} {
		g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint(id, "/"+id))
		names = append(names, "test-ac-"+id)
	}
	checker := &fakeChecker{}
	for _, name := range names[:maxPrecheckRounds] {
		checker.verdicts = append(checker.verdicts, configcheck.Verdict{Findings: []configcheck.Finding{{
			Endpoint: types.NamespacedName{Namespace: "default", Name: name}, Index: 0, Message: "bad " + name,
		}}})
	}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(checker.calls) != maxPrecheckRounds {
		t.Errorf("config checks = %d, want %d", len(checker.calls), maxPrecheckRounds)
	}
	for _, name := range names {
		if endpointExists(t, c, name) {
			t.Errorf("endpoint %s was written without a passing check", name)
		}
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != len(names) {
		t.Fatalf("failedOperations = %+v, want all %d held", failed, len(names))
	}
	for _, f := range failed {
		if f.Endpoint == "test-ac-g" && f.Message != "not checked: 5 other operations failed the gateway config check first" {
			t.Errorf("message of the unchecked endpoint = %q", f.Message)
		}
	}
}

func TestAutoConfigReconcile_SteadyStateRunsNoCheck(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	current := ownedCopy(t, ac, g.output.Endpoints[0])
	c := fakeClientBuilder().WithObjects(ac, cm, current, testGateway()).WithStatusSubresource(ac).Build()
	checker := &fakeChecker{}
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(checker.calls) != 0 {
		t.Errorf("expected no config check without writes, got %d", len(checker.calls))
	}
}

func TestAutoConfigReconcile_PrecheckHoldsEnterpriseOnlyNamespacesOnACEGateway(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	keys := generatedEndpoint("getKeys", "/keys")
	keys.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(
		`{"auth/api-keys":{"roles":["admin"]},"documentation/openapi":{"audience":["public"]}}`)}
	keys.Spec.Endpoints[0].Backends[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(
		`{"backend/http/client":{"proxy_address":"http://proxy"}}`)}
	docsOnly := generatedEndpoint("getDocs", "/docs")
	docsOnly.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(
		`{"documentation/openapi":{"audience":["public"]}}`)}
	g.output.Endpoints = append(g.output.Endpoints, keys, docsOnly)
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, nil)).Build()
	checker := &fakeChecker{}
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	for sync := 1; sync <= 2; sync++ {
		ops = nil
		if _, err := reconcileAC(r, ac); err != nil {
			t.Fatalf("sync %d: %v", sync, err)
		}
		if slices.ContainsFunc(ops, func(op string) bool { return strings.HasSuffix(op, " test-ac-getkeys") }) {
			t.Errorf("sync %d wrote the held endpoint: %v", sync, ops)
		}
	}
	if endpointExists(t, c, "test-ac-getkeys") || !endpointExists(t, c, "test-ac-listusers") ||
		!endpointExists(t, c, "test-ac-getdocs") {
		t.Error("expected listusers and getdocs written, getkeys held")
	}
	for i, call := range checker.calls {
		if slices.Contains(endpointNames(call), "test-ac-getkeys") {
			t.Errorf("config check %d included the held endpoint: %v", i, endpointNames(call))
		}
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-getkeys" ||
		failed[0].Reason != v1alpha1.ReasonEndpointRejected ||
		!strings.Contains(failed[0].Message, "spec.endpoints[0].extraConfig auth/api-keys") ||
		!strings.Contains(failed[0].Message, "spec.endpoints[0].backends[0].extraConfig backend/http/client") ||
		strings.Contains(failed[0].Message, "documentation/openapi") {
		t.Errorf("failedOperations = %+v", failed)
	}
}

func TestAutoConfigReconcile_PrecheckWritesBackendKeysCEHonors(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints[0].Spec.Endpoints[0].Backends[0].ExtraConfig = &runtime.RawExtension{Raw: []byte(
		`{"backend/http/client":{"send_body_on_redirect":true}}`)}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !endpointExists(t, c, "test-ac-listusers") {
		t.Error("expected the endpoint written: CE honors send_body_on_redirect, as the webhook admits it")
	}
	if failed := getAC(t, c, ac).Status.FailedOperations; len(failed) != 0 {
		t.Errorf("failedOperations = %+v, want none", failed)
	}
}

func TestRouteCollisions_TheLowerNameKeepsTheRoute(t *testing.T) {
	desired := []*v1alpha1.KrakenDEndpoint{generatedEndpoint("getB", "/h/{b}"), generatedEndpoint("getA", "/h/{a}")}

	got := routeCollisions(desired, nil)

	if len(got) != 1 {
		t.Fatalf("routeCollisions = %+v, want only test-ac-getb held", got)
	}
	rej := got["test-ac-getb"]
	if rej.endpoint != desired[0] || rej.reason != v1alpha1.ReasonConfigValidationFailed ||
		!strings.HasPrefix(rej.message, "has the same route as GET /h/{a} in test-ac-geta: paths that differ only in ") ||
		rej.cause == nil {
		t.Errorf("rejection = %+v", rej)
	}
}

func TestRouteCollisions_AnExistingEndpointKeepsTheRouteOverANewOne(t *testing.T) {
	// getB sorts after getA but is already served, so it keeps the route.
	existing := generatedEndpoint("getB", "/h/{b}")
	existing.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	desired := []*v1alpha1.KrakenDEndpoint{generatedEndpoint("getA", "/h/{a}"), generatedEndpoint("getB", "/h/{b}")}

	got := routeCollisions(desired, []v1alpha1.KrakenDEndpoint{*existing})

	if len(got) != 1 || got["test-ac-geta"].endpoint != desired[0] ||
		!strings.HasPrefix(got["test-ac-geta"].message, "has the same route as GET /h/{b} in test-ac-getb: ") {
		t.Errorf("routeCollisions = %+v, want only test-ac-geta held", got)
	}
}

func TestRouteCollisions_TheOldestExistingEndpointKeepsTheRoute(t *testing.T) {
	older, newer := generatedEndpoint("getB", "/h/{b}"), generatedEndpoint("getA", "/h/{a}")
	older.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	newer.CreationTimestamp = metav1.NewTime(time.Unix(2000, 0))
	desired := []*v1alpha1.KrakenDEndpoint{generatedEndpoint("getA", "/h/{a}"), generatedEndpoint("getB", "/h/{b}")}

	got := routeCollisions(desired, []v1alpha1.KrakenDEndpoint{*newer, *older})

	if len(got) != 1 || got["test-ac-geta"].endpoint != desired[0] {
		t.Errorf("routeCollisions = %+v, want only test-ac-geta held", got)
	}
}

func TestRouteCollisions_AStaleEndpointTakesNoPart(t *testing.T) {
	// getUser was renamed getUserById: the old endpoint still holds the
	// route until it is deleted, and the new one must not be held for it.
	stale := generatedEndpoint("getUser", "/users/{id}")
	stale.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	desired := []*v1alpha1.KrakenDEndpoint{generatedEndpoint("getUserById", "/users/{id}")}

	if got := routeCollisions(desired, []v1alpha1.KrakenDEndpoint{*stale}); len(got) != 0 {
		t.Errorf("routeCollisions = %+v, want none", got)
	}
}

func TestAutoConfigReconcile_PrecheckHoldsANewShapeCollisionAndKeepsStaleInTheCheck(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// getB is served already; getA shares its route shape and is new.
	served := ownedCopy(t, ac, generatedEndpoint("getB", "/h/{b}"))
	served.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	g.output.Endpoints = []*v1alpha1.KrakenDEndpoint{
		generatedEndpoint("getA", "/h/{a}"), generatedEndpoint("getB", "/h/{b}"), generatedEndpoint("getC", "/c"),
	}
	// The check would fail the clashing endpoint if it were put to it.
	checker := &fakeChecker{judge: func(replace []v1alpha1.KrakenDEndpoint) configcheck.Verdict {
		if slices.Contains(endpointNames(replace), "test-ac-geta") {
			return configcheck.Verdict{Findings: []configcheck.Finding{{
				Endpoint: types.NamespacedName{Namespace: "default", Name: "test-ac-geta"}, Index: 0, Message: "clash",
			}}}
		}
		return configcheck.Verdict{OK: true}
	}}
	c := fakeClientBuilder().WithObjects(ac, cm, served, stale, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if endpointExists(t, c, "test-ac-geta") || !endpointExists(t, c, "test-ac-getc") ||
		!endpointExists(t, c, "test-ac-old") {
		t.Error("expected getA held, getC written and the stale endpoint kept")
	}
	// The held collision keeps the stale endpoint: the check does not replace
	// it with an empty copy, so it stays as it is. The held endpoint is not
	// checked.
	if len(checker.calls) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checker.calls))
	}
	if names := endpointNames(checker.calls[0]); !slices.Equal(names, []string{"test-ac-getc"}) {
		t.Errorf("replace set = %v, want only test-ac-getc (no empty copy of the stale endpoint)", names)
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-geta" ||
		failed[0].Reason != v1alpha1.ReasonConfigValidationFailed ||
		!strings.HasPrefix(failed[0].Message, "has the same route as GET /h/{b} in test-ac-getb: ") {
		t.Errorf("failedOperations = %+v", failed)
	}
}

func TestAutoConfigReconcile_HoldsAStoredSameShapePairsLoserAndKeepsStale(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// Both endpoints were written before the precheck existed; getB is the
	// older, so the gateway serves it and getA never gets its route.
	older, newer := generatedEndpoint("getB", "/h/{b}"), generatedEndpoint("getA", "/h/{a}")
	servedB, servedA := ownedCopy(t, ac, older), ownedCopy(t, ac, newer)
	servedB.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	servedA.CreationTimestamp = metav1.NewTime(time.Unix(2000, 0))
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	g.output.Endpoints = []*v1alpha1.KrakenDEndpoint{newer, older}
	var ops []string
	c := fakeClientBuilder().WithObjects(ac, cm, servedA, servedB, stale, testGateway()).WithStatusSubresource(ac).
		WithInterceptorFuncs(recordEndpointWrites(&ops, nil)).Build()
	checker := &fakeChecker{}
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(ops) != 0 || len(checker.calls) != 0 {
		t.Errorf("expected no writes, deletes or checks, got %v and %d checks", ops, len(checker.calls))
	}
	got := getAC(t, c, ac)
	failed := got.Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-geta" ||
		failed[0].Reason != v1alpha1.ReasonConfigValidationFailed {
		t.Errorf("failedOperations = %+v, want test-ac-geta held", failed)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionSynced)
	if cond == nil || cond.Reason != v1alpha1.ReasonOperationsFailed {
		t.Errorf("expected Synced False/OperationsFailed, got %+v", cond)
	}
}

func TestAttributeFindings_CauseCarriesEveryFindingInOrder(t *testing.T) {
	a := generatedEndpoint("a", "/a")
	key := types.NamespacedName{Namespace: "default", Name: "test-ac-a"}

	got := attributeFindings([]configcheck.Finding{
		{Endpoint: key, Index: 0, Message: "zeta"},
		{Endpoint: key, Index: -1, Message: "alpha"},
	}, []*v1alpha1.KrakenDEndpoint{a})

	if want := "default/test-ac-a spec.endpoints[0]: zeta; default/test-ac-a: alpha"; got["test-ac-a"].cause.Error() != want {
		t.Errorf("cause = %v, want %q", got["test-ac-a"].cause, want)
	}
	if got["test-ac-a"].message != "alpha" {
		t.Errorf("message = %q, want the least finding, alpha", got["test-ac-a"].message)
	}
}

// readyEndpoint returns ep as the endpoint controller reports it at its
// current generation: Ready with the given status and reason.
func readyEndpoint(ep *v1alpha1.KrakenDEndpoint, status metav1.ConditionStatus, reason string) *v1alpha1.KrakenDEndpoint {
	out := ep.DeepCopy()
	out.Status.ObservedGeneration = out.Generation
	out.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.ConditionReady, Status: status, Reason: reason, LastTransitionTime: metav1.Now(),
	}}
	return out
}

func TestSummarizeReadiness(t *testing.T) {
	a := readyEndpoint(generatedEndpoint("a", "/a"), metav1.ConditionTrue, "Ready")
	updated := readyEndpoint(generatedEndpoint("b", "/b"), metav1.ConditionTrue, "Ready")
	updated.Generation = 2 // changed since the endpoint controller last reported
	gone := readyEndpoint(generatedEndpoint("c", "/c"), metav1.ConditionTrue, "Ready")

	got := summarizeReadiness(
		[]v1alpha1.KrakenDEndpoint{*a, *updated, *gone},
		map[string]bool{"test-ac-d": true},
		map[string]bool{"test-ac-c": true},
	)

	want := endpointReadiness{total: 3, ready: 1, notReady: []string{"test-ac-b: Pending", "test-ac-d: Pending"}}
	if got.total != want.total || got.ready != want.ready || !slices.Equal(got.notReady, want.notReady) {
		t.Errorf("summarizeReadiness = %+v, want %+v", got, want)
	}
}

func TestEndpointsReadyCondition(t *testing.T) {
	t.Run("all ready", func(t *testing.T) {
		got := endpointsReadyCondition(endpointReadiness{total: 2, ready: 2}, 3)
		if got.Type != v1alpha1.ConditionEndpointsReady || got.Status != metav1.ConditionTrue ||
			got.Reason != v1alpha1.ReasonAllEndpointsReady || got.ObservedGeneration != 3 ||
			got.Message != "2 of 2 endpoints ready" {
			t.Errorf("unexpected condition %+v", got)
		}
	})
	t.Run("not ready names the first five", func(t *testing.T) {
		notReady := []string{"a: Pending", "b: Pending", "c: Pending", "d: Pending", "e: Pending", "f: Pending"}
		got := endpointsReadyCondition(endpointReadiness{total: 8, ready: 2, notReady: notReady}, 3)
		want := "6 of 8 endpoints not ready: a: Pending; b: Pending; c: Pending; d: Pending; e: Pending; and 1 more"
		if got.Status != metav1.ConditionFalse || got.Reason != v1alpha1.ReasonEndpointsNotReady ||
			got.ObservedGeneration != 3 || got.Message != want {
			t.Errorf("unexpected condition %+v, want message %q", got, want)
		}
	})
}

func TestAutoConfigReconcile_AggregatesEndpointReadiness(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b"))
	users := readyEndpoint(ownedCopy(t, ac, g.output.Endpoints[0]), metav1.ConditionTrue, "Ready")
	b := readyEndpoint(ownedCopy(t, ac, g.output.Endpoints[1]), metav1.ConditionFalse, v1alpha1.ReasonEndpointConflict)
	c := fakeClientBuilder().WithObjects(ac, cm, users, b).WithStatusSubresource(ac, users, b).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	updated := getAC(t, c, ac)
	if updated.Status.ReadyEndpoints != 1 {
		t.Errorf("readyEndpoints = %d, want 1", updated.Status.ReadyEndpoints)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionEndpointsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonEndpointsNotReady ||
		cond.Message != "1 of 2 endpoints not ready: test-ac-getb: EndpointConflict" {
		t.Errorf("unexpected EndpointsReady %+v", cond)
	}
	ready := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonEndpointsNotReady {
		t.Errorf("expected Ready False/EndpointsNotReady while an endpoint is not ready, got %+v", ready)
	}
}

func TestOwnedEndpointPredicate_PassesReadinessChangesOnly(t *testing.T) {
	old := generatedEndpoint("a", "/a")
	old.Generation = 1
	ready := readyEndpoint(old, metav1.ConditionTrue, "Ready")
	reworded := ready.DeepCopy()
	reworded.Status.Conditions[0].Message = "same readiness, new message"
	p := ownedEndpointPredicate()
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: ready}) {
		t.Error("expected a readiness change to pass")
	}
	if p.Update(event.UpdateEvent{ObjectOld: ready, ObjectNew: reworded}) {
		t.Error("expected a message-only status change to be ignored")
	}
}

func TestAutoConfigReconcile_ReadyWhenEveryEndpointIsReady(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	users := readyEndpoint(ownedCopy(t, ac, g.output.Endpoints[0]), metav1.ConditionTrue, "Ready")
	c := fakeClientBuilder().WithObjects(ac, cm, users).WithStatusSubresource(ac, users).Build()
	r := newACReconciler(c, f, ce, fi, g)

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	updated := getAC(t, c, ac)
	for _, cond := range []string{v1alpha1.ConditionEndpointsReady, v1alpha1.ConditionReady} {
		if !meta.IsStatusConditionTrue(updated.Status.Conditions, cond) {
			t.Errorf("expected %s True, got %+v", cond, meta.FindStatusCondition(updated.Status.Conditions, cond))
		}
	}
	if updated.Status.ReadyEndpoints != 1 {
		t.Errorf("readyEndpoints = %d, want 1", updated.Status.ReadyEndpoints)
	}
}
