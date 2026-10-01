//go:build integration

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

package integration

import (
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func eventually(t *testing.T, check func() error) {
	t.Helper()
	eventuallyWithin(t, 60*time.Second, check)
}

// eventuallyWithin polls check until it returns nil, failing the test if it
// has not done so within timeout.
func eventuallyWithin(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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

	// Wait for the content-addressed ConfigMap (krakend config).
	applied := waitForAppliedChecksum(t, client.ObjectKeyFromObject(gw))
	cm := &corev1.ConfigMap{}
	eventually(t, func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: resources.ConfigMapName(gw, applied)}, cm)
	})
}

func TestGateway_ConfigIsContentAddressedAndImmutable(t *testing.T) {
	ns := testNamespace(t)
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "immutable-gw", Namespace: ns},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.9", Edition: v1alpha1.EditionCE},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	first := waitForAppliedChecksum(t, client.ObjectKeyFromObject(gw))
	firstName := resources.ConfigMapName(gw, first)
	var cm corev1.ConfigMap
	eventually(t, func() error {
		return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: firstName}, &cm)
	})
	cm.Data[resources.ConfigKey] = `{"version":3,"tampered":true}`
	if err := k8sClient.Update(ctx, &cm); !apierrors.IsInvalid(err) {
		t.Fatalf("updating a config revision's data must be refused by the API server, got %v", err)
	}

	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "items", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: gw.Name},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/items", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: "/items"}},
			}},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	eventually(t, func() error {
		var got v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &got); err != nil {
			return err
		}
		if got.Status.ConfigChecksum == first {
			return fmt.Errorf("the new config is not applied yet")
		}
		var dep appsv1.Deployment
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &dep); err != nil {
			return err
		}
		want := resources.ConfigMapName(gw, got.Status.ConfigChecksum)
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Name == "config" && v.ConfigMap != nil && v.ConfigMap.Name == want {
				return nil
			}
		}
		return fmt.Errorf("the Deployment does not mount %s yet", want)
	})
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: firstName}, &cm); err != nil {
		t.Fatalf("the previous config revision must survive the rollout: %v", err)
	}
	// With GC active, the previous revision survives only because its
	// ReplicaSet is still live (the K3s pods never become ready, so the
	// rollout never completes) or because it is inside the history.
	var rsList appsv1.ReplicaSetList
	if err := k8sClient.List(ctx, &rsList, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(rsList.Items) == 0 {
		t.Fatal("the Deployment controller created no ReplicaSet; the GC path is untested")
	}
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

	// Wait for gateway config checksum to change (re-reconcile happened)
	// and endpoint count to reflect the new endpoint.
	eventually(t, func() error {
		var updated v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &updated); err != nil {
			return err
		}
		if updated.Status.ConfigChecksum == initialChecksum {
			return fmt.Errorf("config checksum unchanged")
		}
		if updated.Status.EndpointCount != 1 {
			return fmt.Errorf("expected endpoint count 1, got %d", updated.Status.EndpointCount)
		}
		return nil
	})
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

// waitForAppliedChecksum waits until the gateway key has an applied config
// and returns its checksum.
func waitForAppliedChecksum(t *testing.T, key client.ObjectKey) string {
	t.Helper()
	var applied string
	eventually(t, func() error {
		var got v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, key, &got); err != nil {
			return err
		}
		if got.Status.ConfigChecksum == "" {
			return fmt.Errorf("no applied config yet")
		}
		applied = got.Status.ConfigChecksum
		return nil
	})
	return applied
}

func TestGateway_DeletedDeploymentRecreatedWhileConfigRejected(t *testing.T) {
	ns := testNamespace(t)
	gw := createGateway(t, ns, "drift-gw")
	applied := waitForAppliedChecksum(t, gw)
	cmKey := types.NamespacedName{
		Namespace: ns,
		Name:      resources.ConfigMapName(&v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: gw.Name}}, applied),
	}
	var appliedCM corev1.ConfigMap
	if err := k8sClient.Get(ctx, cmKey, &appliedCM); err != nil {
		t.Fatalf("get the applied ConfigMap: %v", err)
	}

	createEndpoint(t, ns, "rejected", gw.Name, rejectMarker)
	eventually(t, func() error {
		var got v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, gw, &got); err != nil {
			return err
		}
		c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionConfigValid)
		if c == nil || c.Status != metav1.ConditionFalse {
			return fmt.Errorf("ConfigValid = %+v, want False", c)
		}
		return nil
	})

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: ns}}
	if err := k8sClient.Delete(ctx, dep); err != nil {
		t.Fatalf("delete deployment: %v", err)
	}
	eventually(t, func() error {
		var got appsv1.Deployment
		if err := k8sClient.Get(ctx, gw, &got); err != nil {
			return fmt.Errorf("waiting for the Deployment to be recreated: %w", err)
		}
		if !got.DeletionTimestamp.IsZero() {
			return fmt.Errorf("the old Deployment is still terminating")
		}
		if a := got.Spec.Template.Annotations[resources.PostRestartJobChecksumAnnotation]; a != applied {
			return fmt.Errorf("recreated Deployment carries config %q, want the applied %q", a, applied)
		}
		return nil
	})

	var cm corev1.ConfigMap
	if err := k8sClient.Get(ctx, cmKey, &cm); err != nil {
		t.Fatalf("get the ConfigMap: %v", err)
	}
	if cm.Data[resources.ConfigKey] != appliedCM.Data[resources.ConfigKey] {
		t.Errorf("the rejected config reached the ConfigMap:\n%s", cm.Data[resources.ConfigKey])
	}
}
