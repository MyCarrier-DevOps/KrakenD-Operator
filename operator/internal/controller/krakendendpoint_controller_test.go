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
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	c := fakeClientBuilder().
		WithObjects(ep).
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
	if !result.Requeue {
		t.Error("expected requeue after setting initial phase")
	}

	var updated v1alpha1.KrakenDEndpoint
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ep1", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get endpoint: %v", err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhasePending {
		t.Errorf("expected phase Pending, got %s", updated.Status.Phase)
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
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ep1", Namespace: "default"}, &updated); err != nil {
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
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ep1", Namespace: "default"}, &updated); err != nil {
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
		Status: v1alpha1.KrakenDEndpointStatus{Phase: v1alpha1.EndpointPhasePending},
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
	if err := c.Get(context.Background(), types.NamespacedName{Name: "ep1", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get: %v", err)
	}
	if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
		t.Errorf("expected Active, got %s", updated.Status.Phase)
	}
	if updated.Status.EndpointCount != 1 {
		t.Errorf("expected endpoint count 1, got %d", updated.Status.EndpointCount)
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
	if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
		t.Errorf("expected Active, got %s", updated.Status.Phase)
	}
}
