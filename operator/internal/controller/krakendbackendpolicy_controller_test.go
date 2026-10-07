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
	"errors"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
		if c.Type == "Ready" && c.Status == metav1.ConditionFalse {
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
		if c.Type == "Ready" && c.Status == metav1.ConditionFalse {
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
		if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
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
	writes := 0
	c := fakeClientBuilder().
		WithObjects(policy).
		WithStatusSubresource(policy).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDBackendPolicy](&writes)).
		Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	// First reconcile sets status
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writes != 1 {
		t.Fatalf("status writes after the first reconcile = %d, want 1", writes)
	}
	// Second reconcile should detect no change
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("unexpected error on second reconcile: %v", err)
	}
	if writes != 1 {
		t.Errorf("status writes after the second reconcile = %d, want none beyond the first", writes)
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
		if cond.Type == v1alpha1.ConditionReady && cond.Status == metav1.ConditionFalse {
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
		if cond.Type == v1alpha1.ConditionReady && cond.Status == metav1.ConditionFalse {
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
				{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "InvalidRateLimit"},
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
		if cond.Type == v1alpha1.ConditionReady && cond.Status == metav1.ConditionTrue {
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

func TestPolicyReconcile_ReadyReplacesPolicyValid(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default", Generation: 2},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100}},
		Status: v1alpha1.KrakenDBackendPolicyStatus{Conditions: []metav1.Condition{{
			Type: "PolicyValid", Status: metav1.ConditionTrue, Reason: "Valid",
			Message: "Policy configuration is valid", ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
		}}},
	}
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	var stored v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &stored); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(stored.Status.Conditions, "PolicyValid") != nil {
		t.Error("the legacy PolicyValid condition was not removed")
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != "Ready" || ready.ObservedGeneration != 2 ||
		stored.Status.ObservedGeneration != 2 {
		t.Errorf("Ready = %+v, observedGeneration %d; want True/Ready at generation 2",
			ready, stored.Status.ObservedGeneration)
	}
}

func TestPolicyReconcile_InvalidPolicyEventOnTransitionOnly(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default", Generation: 1},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{MaxErrors: 0, Interval: 60, Timeout: 30},
		},
	}
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: rec}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := []string{"Warning InvalidCircuitBreaker circuitBreaker.maxErrors must be positive"}
	if got := drainEvents(rec); !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := drainEvents(rec); len(got) != 0 {
		t.Errorf("unchanged invalid policy: events = %q, want none", got)
	}
}

func TestPolicyEndpointPredicate_DropsStatusOnlyUpdates(t *testing.T) {
	old := &v1alpha1.KrakenDEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "default", Generation: 1}}
	statusOnly := old.DeepCopy()
	statusOnly.Status.Phase = v1alpha1.EndpointPhaseActive
	specChange := old.DeepCopy()
	specChange.Generation = 2
	p := policyEndpointPredicate()
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: statusOnly}) {
		t.Error("a status-only endpoint update must not recount references")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: specChange}) {
		t.Error("an endpoint spec change must recount references")
	}
	if !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Error("endpoint creates and deletes must recount references")
	}
}

func TestPolicyReconcile_AddsTheProtectionFinalizer(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"}}
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	r := &KrakenDBackendPolicyReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(&got, v1alpha1.PolicyProtectionFinalizer) {
		t.Fatalf("finalizers = %v, want the protection finalizer", got.Finalizers)
	}
}

func referencingEndpoint(name, policy string) *v1alpha1.KrakenDEndpoint {
	return &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/a", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{
					Host: []string{"http://svc"}, URLPattern: "/",
					PolicyRef: &v1alpha1.PolicyRef{Name: policy},
				}},
			}},
		},
	}
}

func TestPolicyReconcile_TerminatingPolicyIsHeldUntilUnreferenced(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "p", Namespace: "default", Finalizers: []string{v1alpha1.PolicyProtectionFinalizer},
	}}
	ref := referencingEndpoint("uses-p", "p")
	c := fakeClientBuilder().WithObjects(policy, ref).WithStatusSubresource(policy).Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{Client: c, APIReader: c, Scheme: testScheme(), Recorder: rec}
	key := client.ObjectKeyFromObject(policy)
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	var got v1alpha1.KrakenDBackendPolicy

	if err := c.Delete(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("a referenced policy was removed: %v", err)
	}
	if got.Status.ReferencedBy != 1 {
		t.Errorf("referencedBy = %d, want 1 while terminating", got.Status.ReferencedBy)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Warning "+v1alpha1.ReasonPolicyDeletionBlocked) || !strings.Contains(ev, "default/uses-p") {
			t.Errorf("event = %q, want a %s warning naming default/uses-p", ev, v1alpha1.ReasonPolicyDeletionBlocked)
		}
	default:
		t.Error("no event explains why the policy is still terminating")
	}

	if err := c.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if err := c.Get(context.Background(), key, &got); !apierrors.IsNotFound(err) {
		t.Errorf("unreferenced terminating policy: Get err = %v, want NotFound", err)
	}
}

// A reference the cache has not seen yet must keep the policy: the finalizer is
// only released after an uncached list finds no reference.
func TestPolicyReconcile_StaleCacheDoesNotReleaseAReferencedPolicy(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "p", Namespace: "default", Finalizers: []string{v1alpha1.PolicyProtectionFinalizer},
	}}
	cached := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	// The API server's view has no field index, as a real one has none for a CRD.
	live := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(referencingEndpoint("just-created", "p")).Build()
	r := &KrakenDBackendPolicyReconciler{Client: cached, APIReader: live, Scheme: testScheme(), Recorder: fakeRecorder()}
	key := client.ObjectKeyFromObject(policy)
	if err := cached.Delete(context.Background(), policy); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got v1alpha1.KrakenDBackendPolicy
	if err := cached.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("a policy referenced on the API server was released: %v", err)
	}
}

func TestNamedReferrers_BoundsTheList(t *testing.T) {
	var referrers []v1alpha1.KrakenDEndpoint
	for _, n := range []string{"g", "c", "a", "f", "b", "e", "d"} {
		referrers = append(referrers, *referencingEndpoint(n, "p"))
	}

	want := "default/a, default/b, default/c, default/d, default/e and 2 more"
	if got := namedReferrers(referrers); got != want {
		t.Errorf("namedReferrers = %q, want %q", got, want)
	}
}

func TestEndpointPolicyHandler_DeletedEndpointEnqueuesItsPolicy(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	r := &KrakenDBackendPolicyReconciler{}

	r.endpointPolicyHandler().Delete(context.Background(),
		event.DeleteEvent{Object: referencingEndpoint("uses-p", "p")}, q)

	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1", q.Len())
	}
	got, _ := q.Get()
	if want := (types.NamespacedName{Name: "p", Namespace: "default"}); got.NamespacedName != want {
		t.Errorf("enqueued %v, want %v", got.NamespacedName, want)
	}
}

// stubPolicyChecker answers every CheckPolicy with verdict and err, and counts
// the calls.
type stubPolicyChecker struct {
	verdict configcheck.Verdict
	err     error
	calls   int
}

func (s *stubPolicyChecker) CheckPolicy(
	context.Context, *v1alpha1.KrakenDBackendPolicy, configcheck.Memo,
) (configcheck.Verdict, error) {
	s.calls++
	return s.verdict, s.err
}

// policyInRange is a policy whose typed fields all pass the range checks.
func policyInRange() *v1alpha1.KrakenDBackendPolicy {
	return &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default", Generation: 1},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{MaxErrors: 5, Interval: 60, Timeout: 30},
		},
	}
}

func TestPolicyReconcile_ReadyIsFalseWhenThePolicyFailsKrakendCheckOnItsOwn(t *testing.T) {
	policy := policyInRange()
	const output = "- at '/endpoints/0/backend/0/extra_config/qos~1circuit-breaker/max_errors': got string, want integer"
	checker := &stubPolicyChecker{verdict: configcheck.Verdict{Output: output}}
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{
		Client: c, Scheme: testScheme(), Recorder: rec, Checker: checker,
	}

	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var stored v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &stored); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonPolicyInvalid {
		t.Fatalf("Ready = %+v, want False/PolicyInvalid", ready)
	}
	for _, want := range []string{"fails krakend check on its own", output} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("Ready message = %q, want it to contain %q", ready.Message, want)
		}
	}
	if ready.ObservedGeneration != stored.Generation || stored.Status.ObservedGeneration != stored.Generation {
		t.Errorf("observedGeneration = %d (condition %d), want %d",
			stored.Status.ObservedGeneration, ready.ObservedGeneration, stored.Generation)
	}
	events := drainEvents(rec)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning PolicyInvalid ") {
		t.Errorf("events = %q, want one Warning PolicyInvalid", events)
	}
}

func TestPolicyReconcile_ReadyIsUnknownWhenTheCheckCannotRun(t *testing.T) {
	policy := policyInRange()
	checker := &stubPolicyChecker{err: errors.New("krakend: executable file not found")}
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	rec := fakeRecorder()
	r := &KrakenDBackendPolicyReconciler{
		Client: c, Scheme: testScheme(), Recorder: rec, Checker: checker,
	}

	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
	_, err := r.Reconcile(context.Background(), req)

	if err == nil {
		t.Error("Reconcile returned no error, so the check is not retried with backoff")
	}
	var stored v1alpha1.KrakenDBackendPolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &stored); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionUnknown || ready.Reason != v1alpha1.ReasonValidatorUnavailable {
		t.Fatalf("Ready = %+v, want Unknown/ValidatorUnavailable", ready)
	}
	if !strings.Contains(ready.Message, "krakend: executable file not found") {
		t.Errorf("Ready message = %q, want it to carry the cause", ready.Message)
	}
	if events := drainEvents(rec); len(events) != 0 {
		t.Errorf("events = %q, want none: an unavailable validator is not a policy failure", events)
	}
}

func TestPolicyReconcile_AReconcileOfUnchangedContentRunsNoExtraCheck(t *testing.T) {
	policy := policyInRange()
	c := fakeClientBuilder().WithObjects(policy).WithStatusSubresource(policy).Build()
	validator := &countingValidator{}
	r := &KrakenDBackendPolicyReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Checker: configcheck.New(c, renderer.New(renderer.Options{}), validator, 1, nil),
		Memo:    configcheck.NewLRUMemo(8),
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}

	for range 3 {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if validator.calls != 1 {
		t.Errorf("krakend ran %d times over 3 reconciles of one content, want 1", validator.calls)
	}
}
