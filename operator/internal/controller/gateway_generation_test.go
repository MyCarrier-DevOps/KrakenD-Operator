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
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// newerSpecGateway is a CE gateway serving config "A" with its Deployment
// settled, whose spec has been edited since its status was written: the
// stored generation is 2 and status.observedGeneration is 1.
func newerSpecGateway() *v1alpha1.KrakenDGateway {
	gw := servingGateway("A", convergedImage)
	gw.Generation = 2
	gw.Status.ObservedGeneration = 1
	return gw
}

// rejectHPAWrites makes every create of a HorizontalPodAutoscaler fail.
func rejectHPAWrites() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
				return errors.New("the HorizontalPodAutoscaler was rejected by an admission webhook")
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

func TestGatewayReconcile_AChildErrorKeepsObservedGenerationBehind(t *testing.T) {
	gw := newerSpecGateway()
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MaxReplicas: 5}
	c := fakeClientBuilder().WithObjects(gw, settledDeployment(gw, "A")).WithStatusSubresource(gw).
		WithInterceptorFuncs(rejectHPAWrites()).Build()
	r := newTestGatewayReconciler(c, renderOutput("A"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("a rejected HorizontalPodAutoscaler must fail the pass so it is retried")
	}

	got := getGateway(t, c, gw)
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want it held at 1: the spec at generation 2 is not fully applied",
			got.Status.ObservedGeneration)
	}
	if ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady); ready == nil ||
		ready.ObservedGeneration != 1 {
		t.Errorf("Ready = %+v, want its observedGeneration held at 1 with the status's", ready)
	}
}
