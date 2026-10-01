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
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestEndpointReconcile_NotFound(t *testing.T) {
	c := fakeClientBuilder().Build()
	rec := fakeRecorder()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

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

func TestEndpointReconcile_InitialPhase(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(ep).
		Build()
	rec := fakeRecorder()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ep1", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Error("should not requeue; initial phase is set inline")
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: "ep1", Namespace: "default"},
		&updated,
	); err != nil {
		t.Fatalf("failed to get endpoint: %v", err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhasePending {
		t.Errorf("expected phase Pending until the gateway accepts the endpoint, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_GatewayNotFound(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "nonexistent-gw"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
		Status: v1alpha1.KrakenDEndpointStatus{Phase: v1alpha1.EndpointPhasePending},
	}
	c := fakeClientBuilder().
		WithObjects(ep).
		WithStatusSubresource(ep).
		Build()
	rec := fakeRecorder()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ep1", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: "ep1", Namespace: "default"},
		&updated,
	); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseDetached {
		t.Errorf("expected Detached, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_PolicyNotFound(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api/v1/test",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://backend:8080"},
							URLPattern: "/test",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "missing-policy"},
						},
					},
				},
			},
		},
		Status: v1alpha1.KrakenDEndpointStatus{Phase: v1alpha1.EndpointPhasePending},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(ep).
		Build()
	rec := fakeRecorder()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ep1", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: "ep1", Namespace: "default"},
		&updated,
	); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseInvalid {
		t.Errorf("expected Invalid, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_Active(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
		},
	}
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api/v1/test",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://backend:8080"},
							URLPattern: "/test",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "my-policy"},
						},
					},
				},
			},
		},
		Status: v1alpha1.KrakenDEndpointStatus{
			Phase:      v1alpha1.EndpointPhasePending,
			Conditions: []metav1.Condition{acceptedAt(0)},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, policy, ep).
		WithStatusSubresource(ep).
		Build()
	rec := fakeRecorder()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ep1", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(
		context.Background(),
		types.NamespacedName{Name: "ep1", Namespace: "default"},
		&updated,
	); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
		t.Errorf("expected Active, got %s", updated.Status.Phase)
	}
	if updated.Status.EndpointCount != 1 {
		t.Errorf("expected endpoint count 1, got %d", updated.Status.EndpointCount)
	}
	if updated.Status.Methods != "GET" {
		t.Errorf("expected methods %q, got %q", "GET", updated.Status.Methods)
	}
}

func TestEndpointReconcile_GatewayToEndpoints(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	ep1 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	ep2 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "other-gw"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	c := fakeClientBuilder().WithObjects(ep1, ep2).Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme()}

	requests := r.gatewayToEndpoints(context.Background(), gw)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "ep1" {
		t.Errorf("expected ep1, got %s", requests[0].Name)
	}
}

func TestEndpointReconcile_ActiveNoPolicyRef(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/test",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:8080"}, URLPattern: "/"},
					},
				},
			},
		},
		Status: v1alpha1.KrakenDEndpointStatus{
			Phase:      v1alpha1.EndpointPhasePending,
			Conditions: []metav1.Condition{acceptedAt(0)},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(ep).
		Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ep),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
		t.Errorf("expected Active, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_PolicyToEndpoints(t *testing.T) {
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100}},
	}
	ep1 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/a", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:8080"}, URLPattern: "/", PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
				}},
			},
		},
	}
	ep2 := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/b", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:8080"}, URLPattern: "/"},
				}},
			},
		},
	}
	c := fakeClientBuilder().WithObjects(ep1, ep2).Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme()}

	requests := r.policyToEndpoints(context.Background(), policy)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "ep1" {
		t.Errorf("expected ep1, got %s", requests[0].Name)
	}
}

func TestEndpointReconcile_NoOpWhenUnchanged(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default", Generation: 1},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/test",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: "/"}},
				},
			},
		},
		Status: v1alpha1.KrakenDEndpointStatus{
			Phase:              v1alpha1.EndpointPhaseActive,
			ObservedGeneration: 1,
			EndpointCount:      1,
			Methods:            "GET",
			Conditions: []metav1.Condition{
				{Type: "ResolvedRefs", Status: metav1.ConditionTrue, Reason: "RefsResolved",
					Message: "Gateway and all policy references resolved", ObservedGeneration: 1,
					LastTransitionTime: transitionTime},
				acceptedAt(1),
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready",
					Message: "References resolved and accepted by the gateway", ObservedGeneration: 1,
					LastTransitionTime: transitionTime},
			},
		},
	}
	writes := 0
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(ep).
		WithInterceptorFuncs(countStatusWrites[*v1alpha1.KrakenDEndpoint](&writes)).
		Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ep)}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writes != 0 {
		t.Errorf("status writes for an unchanged endpoint = %d, want 0", writes)
	}
}

func TestEndpointReconcile_DetachedToActive(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/test",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: "/"}},
				},
			},
		},
		Status: v1alpha1.KrakenDEndpointStatus{
			Phase:      v1alpha1.EndpointPhaseDetached,
			Conditions: []metav1.Condition{acceptedAt(0)},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(ep).
		Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ep)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
		t.Errorf("expected Active after gateway discovered, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_MultiplePoliciesDedup(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100}},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/a", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:8080"}, URLPattern: "/a", PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
				}},
				{Endpoint: "/b", Method: "POST", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:8080"}, URLPattern: "/b", PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
				}},
			},
		},
		Status: v1alpha1.KrakenDEndpointStatus{
			Phase:      v1alpha1.EndpointPhasePending,
			Conditions: []metav1.Condition{acceptedAt(0)},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, policy, ep).
		WithStatusSubresource(ep).
		Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ep)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
		t.Errorf("expected Active with deduped policy, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_CrossNamespaceGatewayRef(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "operator-ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1", Namespace: "operator-ns"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/test", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:8080"}, URLPattern: "/"},
				}},
			},
		},
		Status: v1alpha1.KrakenDEndpointStatus{
			Phase:      v1alpha1.EndpointPhasePending,
			Conditions: []metav1.Condition{acceptedAt(0)},
		},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(ep).
		Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ep),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
		t.Errorf("expected Active for cross-namespace gateway, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_CrossNamespaceGatewayNotFound(t *testing.T) {
	// Gateway exists in "operator-ns" but endpoint references "wrong-ns"
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "operator-ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1", Namespace: "wrong-ns"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
		Status: v1alpha1.KrakenDEndpointStatus{Phase: v1alpha1.EndpointPhasePending},
	}
	c := fakeClientBuilder().
		WithObjects(gw, ep).
		WithStatusSubresource(ep).
		Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(ep),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ep), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseDetached {
		t.Errorf("expected Detached when gateway in wrong namespace, got %s", updated.Status.Phase)
	}
}

func TestEndpointReconcile_GatewayToEndpointsCrossNamespace(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "operator-ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
	epSameNs := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-same", Namespace: "operator-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	epCrossNs := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-cross", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1", Namespace: "operator-ns"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	epOther := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-other", Namespace: "app-ns"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "other-gw"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	c := fakeClientBuilder().WithObjects(epSameNs, epCrossNs, epOther).Build()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme()}

	requests := r.gatewayToEndpoints(context.Background(), gw)
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests (same-ns + cross-ns), got %d", len(requests))
	}
	names := map[string]bool{}
	for _, req := range requests {
		names[req.Name] = true
	}
	if !names["ep-same"] {
		t.Error("expected ep-same in results")
	}
	if !names["ep-cross"] {
		t.Error("expected ep-cross in results")
	}
}

var ep1Request = ctrl.Request{NamespacedName: types.NamespacedName{Name: "ep1", Namespace: "default"}}

// testGW1 is the gateway default/gw1 the endpoint tests reference.
func testGW1() *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.7.0", Edition: v1alpha1.EditionCE},
	}
}

// endpointOnGW1 returns default/ep1 on gw1 at the given generation, with one
// GET entry and the given seeded conditions.
func endpointOnGW1(generation int64, conds ...metav1.Condition) *v1alpha1.KrakenDEndpoint {
	return &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default", Generation: generation},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/api/v1/test", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://backend:8080"}, URLPattern: "/test"}},
			}},
		},
		Status: v1alpha1.KrakenDEndpointStatus{Conditions: conds},
	}
}

// transitionTime is a fixed lastTransitionTime for seeded conditions.
var transitionTime = metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

// acceptedAt is the gateway's Accepted=True for an included endpoint.
func acceptedAt(generation int64) metav1.Condition {
	return metav1.Condition{
		Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonAccepted,
		Message: "Included in the configuration of gateway default/gw1", ObservedGeneration: generation,
		LastTransitionTime: transitionTime,
	}
}

// storedEP1 returns the stored default/ep1.
func storedEP1(t *testing.T, c client.Client) *v1alpha1.KrakenDEndpoint {
	t.Helper()
	var ep v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), ep1Request.NamespacedName, &ep); err != nil {
		t.Fatalf("getting ep1: %v", err)
	}
	return &ep
}

func TestEndpointReconcile_ReadyDerivedFromBothWriters(t *testing.T) {
	conflict := metav1.Condition{
		Type: v1alpha1.ConditionAccepted, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonEndpointConflict,
		Message: "conflict", ObservedGeneration: 1, LastTransitionTime: transitionTime,
	}
	legacyAvailable := func(status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{Type: "Available", Status: status, Reason: reason, Message: "legacy",
			ObservedGeneration: 1, LastTransitionTime: transitionTime}
	}
	legacyActive := endpointOnGW1(1, legacyAvailable(metav1.ConditionTrue, "ReferencesValid"))
	legacyActive.Status.Phase = v1alpha1.EndpointPhaseActive
	legacyConflicted := endpointOnGW1(1, legacyAvailable(metav1.ConditionFalse, "EndpointConflict"), conflict)
	legacyConflicted.Status.Phase = v1alpha1.EndpointPhaseConflicted

	tests := []struct {
		name       string
		ep         *v1alpha1.KrakenDEndpoint
		wantReady  metav1.ConditionStatus
		wantReason string
		wantPhase  v1alpha1.EndpointPhase
	}{
		{"no gateway verdict yet", endpointOnGW1(1), metav1.ConditionUnknown, "Pending", v1alpha1.EndpointPhasePending},
		{"accepted", endpointOnGW1(1, acceptedAt(1)), metav1.ConditionTrue, "Ready", v1alpha1.EndpointPhaseActive},
		{"verdict for an older generation", endpointOnGW1(2, acceptedAt(1)),
			metav1.ConditionUnknown, "Pending", v1alpha1.EndpointPhasePending},
		{"healthy endpoint with the old status", legacyActive,
			metav1.ConditionUnknown, "Pending", v1alpha1.EndpointPhasePending},
		{"conflicted endpoint with the old status", legacyConflicted,
			metav1.ConditionFalse, "EndpointConflict", v1alpha1.EndpointPhaseConflicted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seeded := meta.FindStatusCondition(tt.ep.Status.Conditions, v1alpha1.ConditionAccepted)
			c := fakeClientBuilder().WithObjects(testGW1(), tt.ep).WithStatusSubresource(tt.ep).Build()
			r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}
			if _, err := r.Reconcile(context.Background(), ep1Request); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			stored := storedEP1(t, c)
			gen := tt.ep.Generation
			refs := meta.FindStatusCondition(stored.Status.Conditions, "ResolvedRefs")
			if refs == nil || refs.Status != metav1.ConditionTrue || refs.Reason != "RefsResolved" ||
				refs.ObservedGeneration != gen {
				t.Errorf("ResolvedRefs = %+v, want True/RefsResolved at generation %d", refs, gen)
			}
			ready := meta.FindStatusCondition(stored.Status.Conditions, "Ready")
			if ready == nil || ready.Status != tt.wantReady || ready.Reason != tt.wantReason ||
				ready.ObservedGeneration != gen {
				t.Errorf("Ready = %+v, want %s/%s at generation %d", ready, tt.wantReady, tt.wantReason, gen)
			}
			if stored.Status.Phase != tt.wantPhase {
				t.Errorf("phase = %q, want %q", stored.Status.Phase, tt.wantPhase)
			}
			if meta.FindStatusCondition(stored.Status.Conditions, "Available") != nil {
				t.Error("legacy Available condition was not removed")
			}
			got := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAccepted)
			if (seeded == nil) != (got == nil) || (got != nil && (got.Status != seeded.Status ||
				got.Reason != seeded.Reason || got.Message != seeded.Message ||
				got.ObservedGeneration != seeded.ObservedGeneration ||
				!got.LastTransitionTime.Equal(&seeded.LastTransitionTime))) {
				t.Errorf("Accepted = %+v, want it untouched (%+v): the gateway owns it", got, seeded)
			}
		})
	}
}

func TestEndpointReconcile_ResolvedRefsEventsOnTransitionOnly(t *testing.T) {
	ep := endpointOnGW1(1)
	c := fakeClientBuilder().WithObjects(ep).WithStatusSubresource(ep).Build()
	rec := fakeRecorder()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	if _, err := r.Reconcile(context.Background(), ep1Request); err != nil {
		t.Fatal(err)
	}
	stored := storedEP1(t, c)
	if stored.Status.Phase != v1alpha1.EndpointPhaseDetached {
		t.Errorf("phase = %q, want Detached", stored.Status.Phase)
	}
	want := []string{"Warning GatewayNotFound gateway default/gw1 not found"}
	if got := drainEvents(rec); !slices.Equal(got, want) {
		t.Errorf("first reconcile events = %q, want %q", got, want)
	}

	if _, err := r.Reconcile(context.Background(), ep1Request); err != nil {
		t.Fatal(err)
	}
	if got := drainEvents(rec); len(got) != 0 {
		t.Errorf("unchanged ResolvedRefs: events = %q, want none", got)
	}

	if err := c.Create(context.Background(), testGW1()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ep1Request); err != nil {
		t.Fatal(err)
	}
	want = []string{"Normal RefsResolved Gateway and all policy references resolved"}
	if got := drainEvents(rec); !slices.Equal(got, want) {
		t.Errorf("after the gateway appears: events = %q, want %q", got, want)
	}
}

func TestEndpointReconcile_StatusConflictRequeuesWithoutClobbering(t *testing.T) {
	ep := endpointOnGW1(1)
	endpointGets := 0
	c := fakeClientBuilder().
		WithObjects(testGW1(), ep).
		WithStatusSubresource(ep).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				stored, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				endpointGets++
				if endpointGets > 1 {
					return nil
				}
				// The gateway writes Accepted after this reconcile read the endpoint.
				fresh := stored.DeepCopy()
				meta.SetStatusCondition(&fresh.Status.Conditions, acceptedAt(1))
				return cl.Status().Update(ctx, fresh)
			},
		}).
		Build()
	rec := fakeRecorder()
	r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	result, err := r.Reconcile(context.Background(), ep1Request)
	assertQuietRequeue(t, result, err, rec)
	stored := storedEP1(t, c)
	if meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAccepted) == nil {
		t.Fatal("the stale write removed the gateway's Accepted condition")
	}

	if _, err := r.Reconcile(context.Background(), ep1Request); err != nil {
		t.Fatal(err)
	}
	stored = storedEP1(t, c)
	ready := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue ||
		meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionAccepted) == nil {
		t.Errorf("after the retry: conditions = %+v, want Accepted kept and Ready True", stored.Status.Conditions)
	}
}

func TestEndpointReconcile_FirstMissingPolicyIsReportedInSpecOrder(t *testing.T) {
	// A map-ordered lookup names a different missing policy from run to run,
	// which changes the message and rewrites status on every reconcile.
	for run := range 20 {
		ep := endpointOnGW1(1)
		ep.Spec.Endpoints[0].Backends = []v1alpha1.BackendSpec{
			{Host: []string{"http://b:8080"}, URLPattern: "/b", PolicyRef: &v1alpha1.PolicyRef{Name: "policy-b"}},
			{Host: []string{"http://a:8080"}, URLPattern: "/a", PolicyRef: &v1alpha1.PolicyRef{Name: "policy-a"}},
		}
		c := fakeClientBuilder().WithObjects(testGW1(), ep).WithStatusSubresource(ep).Build()
		r := &KrakenDEndpointReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}
		if _, err := r.Reconcile(context.Background(), ep1Request); err != nil {
			t.Fatal(err)
		}
		refs := meta.FindStatusCondition(storedEP1(t, c).Status.Conditions, v1alpha1.ConditionResolvedRefs)
		if refs == nil || !strings.Contains(refs.Message, `"policy-b"`) {
			t.Fatalf("run %d: ResolvedRefs = %+v, want the message to name policy-b, the first in spec order", run, refs)
		}
	}
}

func TestEndpointPredicate(t *testing.T) {
	old := endpointOnGW1(1, acceptedAt(1))
	acceptedFlipped := old.DeepCopy()
	acceptedFlipped.Status.Conditions[0].Status = metav1.ConditionFalse
	acceptedFlipped.Status.Conditions[0].Reason = v1alpha1.ReasonEndpointConflict

	p := endpointPredicate()
	tests := []struct {
		name   string
		newObj *v1alpha1.KrakenDEndpoint
		want   bool
	}{
		{"gateway changed Accepted", acceptedFlipped, true},
	}
	for _, tt := range tests {
		if got := p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: tt.newObj}); got != tt.want {
			t.Errorf("%s: Update = %v, want %v", tt.name, got, tt.want)
		}
	}
}
