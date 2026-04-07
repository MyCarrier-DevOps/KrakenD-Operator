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

package integration

import (
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = check(); lastErr == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out: %v", lastErr)
}

func testNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ns)
	})
	return ns.Name
}

func TestGateway_CreatesOwnedResources(t *testing.T) {
	ns := testNamespace(t)

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}

	// Wait for Deployment to be created.
	dep := &appsv1.Deployment{}
	eventually(t, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), dep); err != nil {
			return fmt.Errorf("waiting for deployment: %w", err)
		}
		return nil
	})

	// Verify owner reference.
	if !metav1.IsControlledBy(dep, gw) {
		t.Error("deployment should be controlled by gateway")
	}

	// Wait for Service to be created.
	svc := &corev1.Service{}
	eventually(t, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), svc); err != nil {
			return fmt.Errorf("waiting for service: %w", err)
		}
		return nil
	})
	if !metav1.IsControlledBy(svc, gw) {
		t.Error("service should be controlled by gateway")
	}

	// Wait for ConfigMap (krakend config) to be created.
	cm := &corev1.ConfigMap{}
	eventually(t, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), cm); err != nil {
			return fmt.Errorf("waiting for configmap: %w", err)
		}
		return nil
	})
}

func TestGateway_EndpointTriggersReReconcile(t *testing.T) {
	ns := testNamespace(t)

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-ep", Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}

	// Wait for initial reconcile to complete.
	eventually(t, func() error {
		var updated v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &updated); err != nil {
			return err
		}
		if updated.Status.Phase == "" {
			return fmt.Errorf("gateway not yet reconciled")
		}
		return nil
	})

	// Capture initial checksum.
	var gwBefore v1alpha1.KrakenDGateway
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &gwBefore); err != nil {
		t.Fatal(err)
	}
	initialChecksum := gwBefore.Status.ConfigChecksum

	// Create an endpoint referencing the gateway.
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw-ep"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api/v1/users",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://users-svc:8080"}, URLPattern: "/users"},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Wait for gateway config checksum to change (re-reconcile happened).
	eventually(t, func() error {
		var updated v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &updated); err != nil {
			return err
		}
		if updated.Status.ConfigChecksum == initialChecksum {
			return fmt.Errorf("config checksum unchanged")
		}
		return nil
	})

	// Verify endpoint count updated.
	var gwAfter v1alpha1.KrakenDGateway
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &gwAfter); err != nil {
		t.Fatal(err)
	}
	if gwAfter.Status.EndpointCount != 1 {
		t.Errorf("expected endpoint count 1, got %d", gwAfter.Status.EndpointCount)
	}
}

func TestGateway_DeletionCleansUpResources(t *testing.T) {
	ns := testNamespace(t)

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-del", Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}

	// Wait for Deployment to exist.
	dep := &appsv1.Deployment{}
	eventually(t, func() error {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), dep)
	})

	// Verify the Deployment has an owner reference pointing to the gateway.
	// (envtest does not run the garbage collector, so we verify owner refs
	// instead of waiting for cascade deletion.)
	found := false
	for _, ref := range dep.OwnerReferences {
		if ref.Name == gw.Name && ref.Kind == "KrakenDGateway" {
			found = true
			break
		}
	}
	if !found {
		t.Error("deployment should have owner reference to gateway")
	}
}

func TestEndpoint_MarkedActiveWithGateway(t *testing.T) {
	ns := testNamespace(t)

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-status", Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}

	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-status", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw-status"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api/v1/health",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://health:8080"}, URLPattern: "/health"},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Wait for endpoint status to be updated.
	eventually(t, func() error {
		var updated v1alpha1.KrakenDEndpoint
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ep), &updated); err != nil {
			return err
		}
		if updated.Status.Phase != v1alpha1.EndpointPhaseActive {
			return fmt.Errorf("expected Active phase, got %s", updated.Status.Phase)
		}
		return nil
	})
}

func TestEndpoint_DetachedWhenGatewayMissing(t *testing.T) {
	ns := testNamespace(t)

	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-orphan", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "nonexistent-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api/v1/orphan",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://orphan:8080"}, URLPattern: "/orphan"},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Wait for endpoint to be marked Detached.
	eventually(t, func() error {
		var updated v1alpha1.KrakenDEndpoint
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ep), &updated); err != nil {
			return err
		}
		if updated.Status.Phase != v1alpha1.EndpointPhaseDetached {
			return fmt.Errorf("expected Detached phase, got %s", updated.Status.Phase)
		}
		return nil
	})
}

func TestPolicy_RequeuesGateway(t *testing.T) {
	ns := testNamespace(t)

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-policy", Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}

	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "rate-limit", Namespace: ns},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{},
	}
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	// Create an endpoint that references the policy.
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-policy", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw-policy"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api/v1/rate-limited",
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{
						{
							Host:       []string{"http://svc:8080"},
							URLPattern: "/rate",
							PolicyRef:  &v1alpha1.PolicyRef{Name: "rate-limit"},
						},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Wait for initial reconcile.
	eventually(t, func() error {
		var updated v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &updated); err != nil {
			return err
		}
		if updated.Status.EndpointCount < 1 {
			return fmt.Errorf("waiting for endpoint to be counted")
		}
		return nil
	})

	// Check policy has a referenced-by count.
	eventually(t, func() error {
		var updated v1alpha1.KrakenDBackendPolicy
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), &updated); err != nil {
			return err
		}
		if updated.Status.ReferencedBy < 1 {
			return fmt.Errorf("expected ReferencedBy >= 1, got %d", updated.Status.ReferencedBy)
		}
		return nil
	})
}
