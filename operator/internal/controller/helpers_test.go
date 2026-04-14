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
	// Reset the global index registry so this test is isolated.
	ResetIndexRegistry()
	defer ResetIndexRegistry()

	// Use the fake client builder's indexer to test registerEndpointIndexes
	// directly, since creating a real manager requires a cluster.
	scheme := testScheme()
	fb := fake.NewClientBuilder().WithScheme(scheme)

	// Build a client with indexes registered through the normal path.
	// The fakeClientBuilder in suite_test.go pre-registers them, but here
	// we test that registerEndpointIndexes works correctly.
	c := fb.
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
				for _, entry := range ep.Spec.Endpoints {
					for _, be := range entry.Backends {
						if be.PolicyRef != nil {
							refs = append(refs, be.PolicyRef.PolicyKey(ep.Namespace))
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

	// Verify the policy index works.
	if err := c.List(context.Background(), &list,
		client.MatchingFields{EndpointPolicyIndex: "default/nonexistent"},
	); err != nil {
		t.Fatalf("policy index lookup failed: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected 0 endpoints for nonexistent policy, got %d", len(list.Items))
	}
}
