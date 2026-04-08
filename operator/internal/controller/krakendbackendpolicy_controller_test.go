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
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPolicyReconcile_NotFound(t *testing.T) {
	c := fakeClientBuilder().Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

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

func TestPolicyReconcile_NoReferences(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(policy),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ReferencedBy != 0 {
		t.Errorf("expected 0 references, got %d", updated.Status.ReferencedBy)
	}
}

func TestPolicyReconcile_WithReferences(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	ep1 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/test",
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
	ep2 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/other",
					Method:   "POST",
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://svc2:8080"},
							URLPattern: "/other",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "pol1"},
						},
					},
				},
			},
		},
	}
	ep3 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep3", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/nopolicy",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc3:8080"}, URLPattern: "/"},
					},
				},
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy, ep1, ep2, ep3).
		WithStatusSubresource(policy).
		Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(policy),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.ReferencedBy != 2 {
		t.Errorf("expected 2 references, got %d", updated.Status.ReferencedBy)
	}
}

func TestPolicyReconcile_InvalidCircuitBreaker(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
				MaxErrors: 0,
				Interval:  60,
				Timeout:   30,
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(policy),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, c := range updated.Status.Conditions {
		if c.Type == "PolicyValid" && c.Status == metav1.ConditionFalse {
			found = true
		}
	}
	if !found {
		t.Error("expected Valid=False condition for invalid circuit breaker")
	}
}

func TestPolicyReconcile_InvalidRateLimit(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: -1},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(policy),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range updated.Status.Conditions {
		if c.Type == "PolicyValid" && c.Status == metav1.ConditionFalse {
			found = true
		}
	}
	if !found {
		t.Error("expected Valid=False for invalid rate limit")
	}
}

func TestPolicyReconcile_ValidCondition(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{MaxErrors: 5, Interval: 60, Timeout: 30},
			RateLimit:      &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(policy),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range updated.Status.Conditions {
		if c.Type == "PolicyValid" && c.Status == metav1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Error("expected Valid=True condition")
	}
}

func TestPolicyMapper_PolicyRefsFromEndpoint(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/a",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://a"}, URLPattern: "/", PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
						{Host: []string{"http://b"}, URLPattern: "/", PolicyRef: &v1alpha1.PolicyRef{Name: "pol2"}},
					},
				},
				{
					Endpoint: "/b",
					Method:   "POST",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://c"}, URLPattern: "/", PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					},
				},
			},
		},
	}

	requests := policyRefsFromEndpoint(ep)
	if len(requests) != 2 {
		t.Fatalf("expected 2 unique policy requests, got %d", len(requests))
	}
	names := map[string]bool{}
	for _, req := range requests {
		names[req.Name] = true
	}
	if !names["pol1"] || !names["pol2"] {
		t.Errorf("expected pol1 and pol2, got %v", names)
	}
}

func TestPolicyMapper_PolicyRefsFromNonEndpoint(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
	}
	requests := policyRefsFromEndpoint(gw)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests from non-endpoint object, got %d", len(requests))
	}
}

func TestPolicyMapper_PolicyRefsFromEndpointNoPolicies(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/a", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://a"}, URLPattern: "/"},
				}},
			},
		},
	}
	requests := policyRefsFromEndpoint(ep)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests, got %d", len(requests))
	}
}

func TestPolicyReconcile_StatusNoOpWhenUnchanged(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	// First reconcile sets status
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Second reconcile should detect no change
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}
}

func TestPolicyReconcile_InvalidCircuitBreakerInterval(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
				MaxErrors: 5,
				Interval:  0,
				Timeout:   30,
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPolicyValid && cond.Status == metav1.ConditionFalse {
			if cond.Reason != "InvalidCircuitBreaker" {
				t.Errorf("expected InvalidCircuitBreaker reason, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("expected Invalid condition for zero interval")
}

func TestPolicyReconcile_InvalidCircuitBreakerTimeout(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{
				MaxErrors: 5,
				Interval:  60,
				Timeout:   0,
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPolicyValid && cond.Status == metav1.ConditionFalse {
			if cond.Reason != "InvalidCircuitBreaker" {
				t.Errorf("expected InvalidCircuitBreaker reason, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("expected Invalid condition for zero timeout")
}

func TestPolicyReconcile_InvalidToValid(t *testing.T) {
	// Start with an invalid policy that already has an invalid condition
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
		Status: v1alpha1.KrakenDBackendPolicyStatus{
			Conditions: []metav1.Condition{
				{Type: v1alpha1.ConditionPolicyValid, Status: metav1.ConditionFalse, Reason: "InvalidRateLimit"},
			},
		},
	}
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatal(err)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPolicyValid && cond.Status == metav1.ConditionTrue {
			return
		}
	}
	t.Error("expected Valid=True after fixing policy")
}

func TestValidatePolicy_NilSpecs(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{},
	}
	reason, msg := validatePolicy(policy)
	if reason != "" || msg != "" {
		t.Errorf("expected valid for nil specs, got reason=%q msg=%q", reason, msg)
	}
}
