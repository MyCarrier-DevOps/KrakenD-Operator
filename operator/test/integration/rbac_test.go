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

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestRBAC_RareWritePathsSucceedAsTheOperator drives write paths no other
// scenario reaches, as the operator's ServiceAccount (see
// operatorRBACConfig): a verb missing from the generated role leaves the
// object stuck and the check times out.
func TestRBAC_RareWritePathsSucceedAsTheOperator(t *testing.T) {
	t.Run("the HPA is deleted when autoscaling is removed", func(t *testing.T) {
		ns := testNamespace(t)
		gw := &v1alpha1.KrakenDGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-hpa", Namespace: ns},
			Spec: v1alpha1.KrakenDGatewaySpec{
				Version: "2.9", Edition: v1alpha1.EditionCE,
				Autoscaling: &v1alpha1.AutoscalingSpec{MaxReplicas: 3},
			},
		}
		if err := k8sClient.Create(ctx, gw); err != nil {
			t.Fatalf("create gateway: %v", err)
		}
		key := client.ObjectKeyFromObject(gw)
		eventually(t, func() error {
			return k8sClient.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{})
		})

		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var cur v1alpha1.KrakenDGateway
			if err := k8sClient.Get(ctx, key, &cur); err != nil {
				return err
			}
			cur.Spec.Autoscaling = nil
			return k8sClient.Update(ctx, &cur)
		}); err != nil {
			t.Fatalf("remove autoscaling: %v", err)
		}

		eventually(t, func() error {
			err := k8sClient.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("HPA still present (get err = %v)", err)
		})
	})

	t.Run("a labelled ServiceAccount that predates the gateway is adopted", func(t *testing.T) {
		ns := testNamespace(t)
		gw := &v1alpha1.KrakenDGateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-adopt", Namespace: ns},
			Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.9", Edition: v1alpha1.EditionCE},
		}
		// Taking over an object changes its ownerReferences, which the
		// OwnerReferencesPermissionEnforcement plugin authorizes as delete.
		// Only an object that carries the gateway's selector labels is taken.
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: gw.Name, Namespace: ns, Labels: resources.SelectorLabels(gw),
		}}
		if err := k8sClient.Create(ctx, sa); err != nil {
			t.Fatalf("create ServiceAccount: %v", err)
		}
		if err := k8sClient.Create(ctx, gw); err != nil {
			t.Fatalf("create gateway: %v", err)
		}
		key := client.ObjectKeyFromObject(gw)
		eventually(t, func() error {
			if err := k8sClient.Get(ctx, key, &appsv1.Deployment{}); err != nil {
				return fmt.Errorf("deployment: %w", err)
			}
			var cur corev1.ServiceAccount
			if err := k8sClient.Get(ctx, key, &cur); err != nil {
				return err
			}
			if !metav1.IsControlledBy(&cur, gw) {
				return fmt.Errorf("ServiceAccount is not controlled by the gateway yet: %v", cur.OwnerReferences)
			}
			return nil
		})
	})

	t.Run("an unlabelled ServiceAccount that predates the gateway is not adopted", func(t *testing.T) {
		ns := testNamespace(t)
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "gw-refuse", Namespace: ns}}
		if err := k8sClient.Create(ctx, sa); err != nil {
			t.Fatalf("create ServiceAccount: %v", err)
		}
		gw := &v1alpha1.KrakenDGateway{
			ObjectMeta: metav1.ObjectMeta{Name: sa.Name, Namespace: ns},
			Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.9", Edition: v1alpha1.EditionCE},
		}
		if err := k8sClient.Create(ctx, gw); err != nil {
			t.Fatalf("create gateway: %v", err)
		}
		key := client.ObjectKeyFromObject(gw)
		eventually(t, func() error {
			var cur v1alpha1.KrakenDGateway
			if err := k8sClient.Get(ctx, key, &cur); err != nil {
				return err
			}
			cond := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionResourcesControlled)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonResourceNotControlled {
				return fmt.Errorf("ResourcesControlled = %+v, want False/ResourceNotControlled", cond)
			}
			return nil
		})
		var cur corev1.ServiceAccount
		if err := k8sClient.Get(ctx, key, &cur); err != nil {
			t.Fatal(err)
		}
		if len(cur.OwnerReferences) != 0 {
			t.Errorf("ServiceAccount ownerReferences = %v, want none", cur.OwnerReferences)
		}
		if err := k8sClient.Get(ctx, key, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
			t.Errorf("Deployment get = %v, want NotFound: nothing runs as the unlabelled ServiceAccount", err)
		}
	})
}
