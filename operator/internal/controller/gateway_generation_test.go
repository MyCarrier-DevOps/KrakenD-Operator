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
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
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

func TestGatewayReconcile_AChildErrorDoesNotStarveTheIndependentChildren(t *testing.T) {
	gw := newerSpecGateway()
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MaxReplicas: 5}
	gw.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo done"}
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	gw.Spec.License = &v1alpha1.LicenseConfig{ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{Enabled: true}}
	gw.Spec.Istio = &v1alpha1.IstioSpec{
		Enabled: true, Hosts: []string{"api.example.com"}, Gateways: []string{"istio-system/gw"},
	}
	c := fakeClientBuilder().
		WithRESTMapper(optionalCRDMapper(dragonflyGVK, externalSecretGVK, virtualServiceGVK)).
		WithObjects(gw, settledDeployment(gw, "A")).WithStatusSubresource(gw).
		WithInterceptorFuncs(rejectHPAWrites()).Build()
	r := newTestGatewayReconciler(c, renderOutput("A"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("a rejected HorizontalPodAutoscaler must fail the pass so it is retried")
	}

	for gvk, name := range map[schema.GroupVersionKind]string{
		dragonflyGVK:      resources.DragonflyName(gw),
		externalSecretGVK: resources.ExternalSecretName(gw),
		virtualServiceGVK: gw.Name,
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: gw.Namespace, Name: name}, u); err != nil {
			t.Errorf("the %s must still be reconciled after the HPA failed: %v", gvk.Kind, err)
		}
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(gw.Namespace)); err != nil || len(jobs.Items) != 1 {
		t.Errorf("jobs = %d (%v), want the post-restart Job: it waits for the Deployment, not for the HPA",
			len(jobs.Items), err)
	}
}
