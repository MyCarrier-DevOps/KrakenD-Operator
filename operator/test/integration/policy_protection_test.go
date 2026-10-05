//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// expectProtectionFinalizer fails until the operator has put its finalizer on
// the policy.
func expectProtectionFinalizer(t *testing.T, key client.ObjectKey) {
	t.Helper()
	eventually(t, func() error {
		var got v1alpha1.KrakenDBackendPolicy
		if err := k8sClient.Get(ctx, key, &got); err != nil {
			return err
		}
		if !controllerutil.ContainsFinalizer(&got, v1alpha1.PolicyProtectionFinalizer) {
			return fmt.Errorf("finalizers = %v", got.Finalizers)
		}
		return nil
	})
}

func TestPolicy_DeletionWaitsForReferencesThenCompletes(t *testing.T) {
	ns := testNamespace(t)
	createGateway(t, ns, "gw-protect")
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "protected", Namespace: ns}}
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "uses-protected", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw-protect"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/protected", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{
					Host: []string{"http://svc:8080"}, URLPattern: "/",
					PolicyRef: &v1alpha1.PolicyRef{Name: "protected"},
				}},
			}},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(policy)
	expectProtectionFinalizer(t, key)
	eventually(t, func() error {
		var got v1alpha1.KrakenDBackendPolicy
		if err := k8sClient.Get(ctx, key, &got); err != nil {
			return err
		}
		if got.Status.ReferencedBy != 1 {
			return fmt.Errorf("referencedBy = %d, want 1", got.Status.ReferencedBy)
		}
		return nil
	})

	if err := k8sClient.Delete(ctx, policy); err != nil {
		t.Fatalf("delete of a referenced policy was refused: %v", err)
	}
	eventually(t, func() error {
		n, err := eventCount(ns, "protected", v1alpha1.ReasonPolicyDeletionBlocked)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no %s event yet", v1alpha1.ReasonPolicyDeletionBlocked)
		}
		return nil
	})
	time.Sleep(3 * time.Second) // give the controller every chance to release it wrongly
	var held v1alpha1.KrakenDBackendPolicy
	if err := k8sClient.Get(ctx, key, &held); err != nil || held.DeletionTimestamp.IsZero() {
		t.Fatalf("referenced policy: err = %v, deletionTimestamp = %v; want it held while terminating",
			err, held.DeletionTimestamp)
	}

	if err := k8sClient.Delete(ctx, ep); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		err := k8sClient.Get(ctx, key, &held)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("policy still present (err = %v)", err)
	})
}

func TestPolicy_UnreferencedPolicyDeletesPromptly(t *testing.T) {
	ns := testNamespace(t)
	policy := &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: "lonely", Namespace: ns}}
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(policy)
	expectProtectionFinalizer(t, key)

	if err := k8sClient.Delete(ctx, policy); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var got v1alpha1.KrakenDBackendPolicy
		if err := k8sClient.Get(ctx, key, &got); apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("unreferenced policy still present")
	})
}
