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
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func TestEnsureEndpointIndexes_Idempotent(t *testing.T) {
	ResetIndexRegistry()
	defer ResetIndexRegistry()

	// stubIndexer records IndexField calls and succeeds on each.
	indexer := &stubIndexer{}

	// First call should register both indexes.
	if err := registerEndpointIndexes(indexer); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if indexer.callCount() != 2 {
		t.Fatalf("expected 2 IndexField calls, got %d", indexer.callCount())
	}
}

func TestEnsureEndpointIndexes_ConcurrentCallers(t *testing.T) {
	ResetIndexRegistry()
	defer ResetIndexRegistry()

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
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, EndpointGatewayIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				ns := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace)
				return []string{ns + "/" + ep.Spec.GatewayRef.Name}
			},
		).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, EndpointPolicyIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				var refs []string
				seen := make(map[string]struct{})
				for _, entry := range ep.Spec.Endpoints {
					for _, be := range entry.Backends {
						if be.PolicyRef == nil {
							continue
						}
						key := be.PolicyRef.PolicyKey(ep.Namespace)
						if _, ok := seen[key]; !ok {
							seen[key] = struct{}{}
							refs = append(refs, key)
						}
					}
				}
				return refs
			},
		).
		WithObjects(
			&v1alpha1.KrakenDEndpoint{
				ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
				Spec: v1alpha1.KrakenDEndpointSpec{
					GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
					Endpoints:  []v1alpha1.EndpointEntry{},
				},
			},
		).
		Build()

	var list v1alpha1.KrakenDEndpointList
	if err := c.List(context.Background(), &list,
		client.MatchingFields{EndpointGatewayIndex: "default/gw1"},
	); err != nil {
		t.Fatalf("gateway index lookup failed: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 endpoint, got %d", len(list.Items))
	}

	if err := c.List(context.Background(), &list,
		client.MatchingFields{EndpointPolicyIndex: "default/nonexistent"},
	); err != nil {
		t.Fatalf("policy index lookup failed: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected 0 endpoints for nonexistent policy, got %d", len(list.Items))
	}
}

// stubIndexer implements client.FieldIndexer to track IndexField calls.
type stubIndexer struct {
	mu    sync.Mutex
	calls int
}

func (s *stubIndexer) IndexField(
	_ context.Context, _ client.Object, _ string, _ client.IndexerFunc,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
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
func (m *stubManager) GetScheme() *runtime.Scheme           { return testScheme() }
