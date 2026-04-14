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
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestConditionsEqual_BothEmpty(t *testing.T) {
	if !conditionsEqual(nil, nil) {
		t.Error("two nil slices should be equal")
	}
	if !conditionsEqual([]metav1.Condition{}, []metav1.Condition{}) {
		t.Error("two empty slices should be equal")
	}
}

func TestConditionsEqual_DifferentLength(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	if conditionsEqual(a, nil) {
		t.Error("different lengths should not be equal")
	}
}

func TestConditionsEqual_SameContent(t *testing.T) {
	a := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "all good", ObservedGeneration: 1},
		{Type: "Available", Status: metav1.ConditionFalse, Reason: "Degraded", Message: "not ready", ObservedGeneration: 2},
	}
	b := []metav1.Condition{
		{Type: "Available", Status: metav1.ConditionFalse, Reason: "Degraded", Message: "not ready", ObservedGeneration: 2},
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "all good", ObservedGeneration: 1},
	}
	if !conditionsEqual(a, b) {
		t.Error("same conditions in different order should be equal")
	}
}

func TestConditionsEqual_IgnoresLastTransitionTime(t *testing.T) {
	now := metav1.Now()
	a := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: now},
	}
	b := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Time{}},
	}
	if !conditionsEqual(a, b) {
		t.Error("should ignore LastTransitionTime differences")
	}
}

func TestConditionsEqual_DifferentStatus(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "OK"}}
	if conditionsEqual(a, b) {
		t.Error("different statuses should not be equal")
	}
}

func TestConditionsEqual_DifferentReason(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "NotOK"}}
	if conditionsEqual(a, b) {
		t.Error("different reasons should not be equal")
	}
}

func TestConditionsEqual_DifferentMessage(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "a"}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "b"}}
	if conditionsEqual(a, b) {
		t.Error("different messages should not be equal")
	}
}

func TestConditionsEqual_DifferentGeneration(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", ObservedGeneration: 1}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", ObservedGeneration: 2}}
	if conditionsEqual(a, b) {
		t.Error("different generations should not be equal")
	}
}

func TestConditionsEqual_MissingType(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	b := []metav1.Condition{{Type: "Other", Status: metav1.ConditionTrue, Reason: "OK"}}
	if conditionsEqual(a, b) {
		t.Error("different types should not be equal")
	}
}

func TestEnsureEndpointIndexes_Sequential(t *testing.T) {
	resetIndexRegistry()
	defer resetIndexRegistry()

	indexer := &stubIndexer{}
	mgr := &stubManager{indexer: indexer}

	// First call registers both indexes (2 IndexField calls).
	if err := EnsureEndpointIndexes(mgr); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if indexer.callCount() != 2 {
		t.Fatalf("expected 2 IndexField calls after first call, got %d", indexer.callCount())
	}

	// Second call should be a no-op (idempotent — returns from cache).
	if err := EnsureEndpointIndexes(mgr); err != nil {
		t.Fatalf("second (idempotent) call failed: %v", err)
	}
	if indexer.callCount() != 2 {
		t.Errorf("expected still 2 IndexField calls after idempotent call, got %d", indexer.callCount())
	}
}

func TestEnsureEndpointIndexes_ConcurrentCallers(t *testing.T) {
	resetIndexRegistry()
	defer resetIndexRegistry()

	indexer := &stubIndexer{}
	mgr := &stubManager{indexer: indexer}

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if err := EnsureEndpointIndexes(mgr); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent EnsureEndpointIndexes failed: %v", err)
	}

	// registerEndpointIndexes should have been called exactly once
	// (2 IndexField calls) regardless of how many goroutines raced.
	if indexer.callCount() != 2 {
		t.Errorf("expected exactly 2 IndexField calls (one registration), got %d", indexer.callCount())
	}
}

func TestFieldIndexesReturnCorrectValues(t *testing.T) {
	// Use fakeClientBuilder which registers the canonical index functions,
	// rather than duplicating closures inline.
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:80"}, URLPattern: "/",
						PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					{Host: []string{"http://svc:80"}, URLPattern: "/alt",
						PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
				}},
			},
		},
	}
	c := fakeClientBuilder().WithObjects(ep).Build()

	// Verify the gateway index works.
	var list v1alpha1.KrakenDEndpointList
	if err := c.List(context.Background(), &list,
		client.MatchingFields{EndpointGatewayIndex: "default/gw1"},
	); err != nil {
		t.Fatalf("gateway index lookup failed: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 endpoint, got %d", len(list.Items))
	}

	// Verify the policy index query returns the endpoint.
	if err := c.List(context.Background(), &list,
		client.MatchingFields{EndpointPolicyIndex: "default/pol1"},
	); err != nil {
		t.Fatalf("policy index lookup failed: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 endpoint, got %d", len(list.Items))
	}

	// Verify non-matching lookup returns 0.
	if err := c.List(context.Background(), &list,
		client.MatchingFields{EndpointPolicyIndex: "default/nonexistent"},
	); err != nil {
		t.Fatalf("policy index lookup failed: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected 0 endpoints for nonexistent policy, got %d", len(list.Items))
	}
}

func TestPolicyIndexFunc_Deduplicates(t *testing.T) {
	resetIndexRegistry()
	defer resetIndexRegistry()

	indexer := &stubIndexer{}
	mgr := &stubManager{indexer: indexer}
	if err := EnsureEndpointIndexes(mgr); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	fn := indexer.funcs[EndpointPolicyIndex]
	if fn == nil {
		t.Fatal("policy index function not captured")
	}

	t.Run("intra-entry", func(t *testing.T) {
		// Two backends in the same entry both reference "pol1".
		ep := &v1alpha1.KrakenDEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/api", Method: "GET", Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:80"}, URLPattern: "/",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
						{Host: []string{"http://svc:80"}, URLPattern: "/alt",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					}},
				},
			},
		}

		refs := fn(ep)
		if len(refs) != 1 {
			t.Errorf("expected 1 deduped policy ref, got %d: %v", len(refs), refs)
		}
		if len(refs) > 0 && refs[0] != "default/pol1" {
			t.Errorf("expected %q, got %q", "default/pol1", refs[0])
		}
	})

	t.Run("cross-entry", func(t *testing.T) {
		// Same policy referenced in two different entries.
		// Verifies the seen map is scoped outside the outer loop.
		ep := &v1alpha1.KrakenDEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default"},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/a", Method: "GET", Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:80"}, URLPattern: "/",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					}},
					{Endpoint: "/b", Method: "POST", Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:80"}, URLPattern: "/",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					}},
				},
			},
		}

		refs := fn(ep)
		if len(refs) != 1 {
			t.Errorf("expected 1 deduped policy ref, got %d: %v", len(refs), refs)
		}
		if len(refs) > 0 && refs[0] != "default/pol1" {
			t.Errorf("expected %q, got %q", "default/pol1", refs[0])
		}
	})
}

// resetIndexRegistry clears the index registry for test isolation.
func resetIndexRegistry() {
	indexRegistry.Range(func(key, _ any) bool {
		indexRegistry.Delete(key)
		return true
	})
}

// stubIndexer implements client.FieldIndexer to track IndexField calls
// and capture the registered functions for direct invocation in tests.
type stubIndexer struct {
	mu    sync.Mutex
	calls int
	funcs map[string]client.IndexerFunc
}

func (s *stubIndexer) IndexField(
	_ context.Context, _ client.Object, field string, fn client.IndexerFunc,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.funcs == nil {
		s.funcs = make(map[string]client.IndexerFunc)
	}
	s.funcs[field] = fn
	return nil
}

func (s *stubIndexer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stubManager implements enough of ctrl.Manager for EnsureEndpointIndexes.
type stubManager struct {
	ctrl.Manager // embed to satisfy interface; nil methods panic if called
	indexer      client.FieldIndexer
}

func (m *stubManager) GetFieldIndexer() client.FieldIndexer { return m.indexer }
