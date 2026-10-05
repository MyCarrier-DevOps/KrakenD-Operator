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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

func TestGatewayReconcile_StepsThatConsumeTheDeploymentWaitForIt(t *testing.T) {
	gw := newerSpecGateway()
	dep := settledDeployment(gw, "A")
	limitCPU(&gw.Spec) // the update the Deployment step cannot write
	gw.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo done"}
	// An HPA the gateway controls and no longer wants: autoscaling is not configured.
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace}}
	if err := controllerutil.SetControllerReference(gw, hpa, testScheme()); err != nil {
		t.Fatal(err)
	}
	c := fakeClientBuilder().WithObjects(gw, dep, hpa).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("A"), &mockValidator{})
	servedByFailing := &servedGateway{c: c, r: r, gw: gw}
	servedByFailing.failDeploymentWrites()

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("a rejected Deployment update must fail the pass")
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(gw.Namespace)); err != nil || len(jobs.Items) != 0 {
		t.Errorf("jobs = %d (%v), want none: the post-restart Job runs only after the Deployment reconciled",
			len(jobs.Items), err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), hpa); err != nil {
		t.Errorf("the HPA was deleted before the Deployment carried its replica count: %v", err)
	}
}

// foreignConfigMap is a ConfigMap the gateway does not control, at the
// content-addressed name of config checksum.
func foreignConfigMap(gw *v1alpha1.KrakenDGateway, checksum string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: resources.ConfigMapName(gw, checksum), Namespace: gw.Namespace},
		Data:       map[string]string{resources.ConfigKey: `{"someone":"else"}`},
	}
}

func TestGatewayReconcile_AnAppliedConfigThatCannotBePublishedKeepsObservedGenerationBehind(t *testing.T) {
	gw := newerSpecGateway()
	gw.Spec.Replicas = new(int32(3)) // the edit at generation 2, which the held Deployment never receives
	c := fakeClientBuilder().WithObjects(gw, settledDeployment(gw, "A"), foreignConfigMap(gw, "A")).
		WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("A"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("a foreign ConfigMap at the applied config's name must fail the pass so it is retried")
	}

	got := getGateway(t, c, gw)
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want it held at 1: the Deployment is held and never saw generation 2",
			got.Status.ObservedGeneration)
	}
}

func TestGatewayReconcile_ObservedGenerationCatchesUpOnceTheErrorClears(t *testing.T) {
	gw := newerSpecGateway()
	c := fakeClientBuilder().WithObjects(gw, settledDeployment(gw, "A"), foreignConfigMap(gw, "A")).
		WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("A"), &mockValidator{})
	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("a foreign ConfigMap at the applied config's name must fail the pass")
	}
	if got := getGateway(t, c, gw).Status.ObservedGeneration; got != 1 {
		t.Fatalf("observedGeneration = %d while the error persists, want 1", got)
	}

	if err := c.Delete(context.Background(), foreignConfigMap(gw, "A")); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile once the foreign ConfigMap is gone: %v", err)
	}

	got := getGateway(t, c, gw)
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if got.Status.ObservedGeneration != 2 || ready == nil || ready.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, Ready = %+v, want both at 2 once the error clears",
			got.Status.ObservedGeneration, ready)
	}
}

func TestGatewayReconcile_AVerdictOnThisGenerationDoesNotHoldObservedGenerationBack(t *testing.T) {
	cases := []struct {
		name  string
		setup func(gw *v1alpha1.KrakenDGateway, r *KrakenDGatewayReconciler) []client.Object
	}{
		{
			name: "rejected render with the applied config's ConfigMap missing",
			setup: func(gw *v1alpha1.KrakenDGateway, r *KrakenDGatewayReconciler) []client.Object {
				r.Renderer = renderOutput("B")
				r.Validator = &countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")}
				return nil
			},
		},
		{
			name: "validator unavailable",
			setup: func(gw *v1alpha1.KrakenDGateway, r *KrakenDGatewayReconciler) []client.Object {
				r.Renderer = renderOutput("B")
				r.Validator = &countingValidator{err: errors.New("fork/exec krakend: no such file or directory")}
				return nil
			},
		},
		{
			name: "plugin ConfigMap missing",
			setup: func(gw *v1alpha1.KrakenDGateway, r *KrakenDGatewayReconciler) []client.Object {
				gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
					{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
				}}
				return nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newerSpecGateway()
			r := newTestGatewayReconciler(nil, renderOutput("A"), &mockValidator{})
			tc.setup(gw, r)
			c := fakeClientBuilder().WithObjects(gw, settledDeployment(gw, "A")).WithStatusSubresource(gw).Build()
			r.Client, r.APIReader = c, c

			_ = reconcileGateway(t, r, gw) // the unavailable validator fails the pass; the others do not

			if got := getGateway(t, c, gw).Status.ObservedGeneration; got != 2 {
				t.Errorf("observedGeneration = %d, want 2: the verdict is on this generation", got)
			}
		})
	}
}
