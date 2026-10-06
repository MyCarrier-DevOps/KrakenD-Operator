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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestRBAC_RareWritePathsSucceedAsTheOperator drives a write path no other
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
}
