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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// discoveryDownMapper fails every lookup the way an unreachable API server does.
type discoveryDownMapper struct{ meta.RESTMapper }

func (discoveryDownMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("the server is currently unable to handle the request")
}

func TestInstalledOptionalKinds(t *testing.T) {
	installed, missing, err := installedOptionalKinds(optionalCRDMapper(virtualServiceGVK))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(installed, []schema.GroupVersionKind{virtualServiceGVK}) {
		t.Errorf("installed = %v, want only the VirtualService kind", installed)
	}
	if !slices.Equal(missing, []schema.GroupVersionKind{dragonflyGVK, externalSecretGVK}) {
		t.Errorf("missing = %v, want Dragonfly and ExternalSecret", missing)
	}

	if _, _, err := installedOptionalKinds(discoveryDownMapper{}); err == nil {
		t.Error("a discovery failure must fail startup, not silently skip a watch")
	}
}

// controlledChild is an object of kind gvk named name that gw controls.
func controlledChild(gw *v1alpha1.KrakenDGateway, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(gw.Namespace)
	u.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(gw, v1alpha1.GroupVersion.WithKind("KrakenDGateway"))})
	return u
}

func TestGatewayReconcile_DeletesTheResourcesOfDisabledFeatures(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Status.ConfigChecksum = "applied"
	gw.Status.DragonflyAddress = "test-gw-dragonfly.default.svc.cluster.local:6379"
	for _, typ := range []string{v1alpha1.ConditionIstioConfigured, v1alpha1.ConditionDragonflyReady} {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "WasEnabled"})
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(gw, v1alpha1.GroupVersion.WithKind("KrakenDGateway"))},
	}}
	children := []*unstructured.Unstructured{
		controlledChild(gw, dragonflyGVK, resources.DragonflyName(gw)),
		controlledChild(gw, externalSecretGVK, resources.ExternalSecretName(gw)),
		controlledChild(gw, virtualServiceGVK, gw.Name),
	}
	objs := []client.Object{gw, hpa}
	for _, ch := range children {
		objs = append(objs, ch)
	}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
		WithObjects(objs...).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), &autoscalingv2.HorizontalPodAutoscaler{}); !apierrors.IsNotFound(err) {
		t.Errorf("HPA Get = %v; removing spec.autoscaling must delete the HPA", err)
	}
	for _, ch := range children {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(ch.GroupVersionKind())
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(ch), u); !apierrors.IsNotFound(err) {
			t.Errorf("%s Get = %v; a disabled feature's resource must be deleted", ch.GetKind(), err)
		}
	}
	got := getGateway(t, c, gw)
	for _, typ := range []string{v1alpha1.ConditionIstioConfigured, v1alpha1.ConditionDragonflyReady} {
		if cond := meta.FindStatusCondition(got.Status.Conditions, typ); cond != nil {
			t.Errorf("%s = %+v; a disabled feature's condition must be removed", typ, cond)
		}
	}
	if got.Status.DragonflyAddress != "" {
		t.Errorf("status.dragonflyAddress = %q, want it cleared", got.Status.DragonflyAddress)
	}
}

func TestGatewayReconcile_NeverDeletesWhatItDoesNotControl(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Status.ConfigChecksum = "applied"
	foreign := controlledChild(gw, virtualServiceGVK, gw.Name)
	foreign.SetOwnerReferences(nil)
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
		WithObjects(gw, foreign).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(virtualServiceGVK)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), u); err != nil {
		t.Errorf("a VirtualService the gateway does not control must survive: %v", err)
	}
}

func TestGatewayReconcile_KeepsChildrenAnotherControllerOwns(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Status.ConfigChecksum = "applied"
	other := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "other-gw", Namespace: gw.Namespace, UID: "other-uid"}}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(other, v1alpha1.GroupVersion.WithKind("KrakenDGateway"))}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace, OwnerReferences: owner,
	}}
	children := []*unstructured.Unstructured{
		controlledChild(gw, dragonflyGVK, resources.DragonflyName(gw)),
		controlledChild(gw, externalSecretGVK, resources.ExternalSecretName(gw)),
		controlledChild(gw, virtualServiceGVK, gw.Name),
	}
	objs := []client.Object{gw, hpa}
	for _, ch := range children {
		ch.SetOwnerReferences(owner)
		objs = append(objs, ch)
	}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
		WithObjects(objs...).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(hpa), &autoscalingv2.HorizontalPodAutoscaler{}); err != nil {
		t.Errorf("an HPA another gateway controls must survive: %v", err)
	}
	for _, ch := range children {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(ch.GroupVersionKind())
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(ch), u); err != nil {
			t.Errorf("a %s another gateway controls must survive: %v", ch.GetKind(), err)
		}
	}
}

func TestGatewayReconcile_DeletesWithAUIDPrecondition(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Status.ConfigChecksum = "applied"
	vs := controlledChild(gw, virtualServiceGVK, gw.Name)
	vs.SetUID("vs-uid")
	var preconditionUID *types.UID
	c := interceptor.NewClient(
		fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
			WithObjects(gw, vs).WithStatusSubresource(gw).Build(),
		interceptor.Funcs{Delete: func(
			ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption,
		) error {
			var o client.DeleteOptions
			o.ApplyOptions(opts)
			if o.Preconditions != nil {
				preconditionUID = o.Preconditions.UID
			}
			return cl.Delete(ctx, obj, opts...)
		}})
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if preconditionUID == nil || *preconditionUID != "vs-uid" {
		t.Errorf("delete precondition UID = %v, want vs-uid", preconditionUID)
	}
}

func TestGatewayReconcile_NoCRDNeverReadsLive(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Status.ConfigChecksum = "applied"
	c := interceptor.NewClient(
		fakeClientBuilder().WithRESTMapper(optionalCRDMapper()).WithObjects(gw).WithStatusSubresource(gw).Build(),
		interceptor.Funcs{Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				t.Errorf("live Get of %s %s without its CRD", u.GetKind(), key)
			}
			return cl.Get(ctx, key, obj, opts...)
		}})
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestGatewayReconcile_DisablingDragonflyDropsItsMetricSeries(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Status.ConfigChecksum = "applied"
	dragonflyReady.WithLabelValues(gw.Namespace, gw.Name).Set(1)
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
		WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if dragonflyReady.DeleteLabelValues(gw.Namespace, gw.Name) {
		t.Error("the dragonfly_ready series is still reported after Dragonfly was disabled")
	}
}

func TestGatewayReconcile_CEFallbackKeepsTheLicenseExternalSecretAndDragonfly(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	gw.UID = "gw-uid"
	// The license is read through the ExternalSecret path: secretRef and
	// externalSecret are mutually exclusive.
	gw.Spec.License.SecretRef = nil
	gw.Spec.License.ExternalSecret = v1alpha1.ExternalSecretLicenseConfig{Enabled: true}
	secret.Name = resources.ExternalSecretName(gw)
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	gw.Status.ConfigChecksum = "applied"
	children := []*unstructured.Unstructured{
		controlledChild(gw, dragonflyGVK, resources.DragonflyName(gw)),
		controlledChild(gw, externalSecretGVK, resources.ExternalSecretName(gw)),
	}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
		WithObjects(gw, secret, children[0], children[1]).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})
	r.LicenseParser = parser

	// The license has lapsed and fallbackToCE is set, so the gateway serves CE.
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !meta.IsStatusConditionTrue(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionCEFallbackApplied) {
		t.Fatal("precondition: the gateway must be on the CE fallback")
	}

	for _, ch := range children {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(ch.GroupVersionKind())
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(ch), u); err != nil {
			t.Errorf("%s must survive a CE fallback, it is still enabled in the spec: %v", ch.GetKind(), err)
		}
	}
}

// mapperClient serves mapper as its RESTMapper.
type mapperClient struct {
	client.Client
	mapper meta.RESTMapper
}

func (m mapperClient) RESTMapper() meta.RESTMapper { return m.mapper }

func TestDeleteOptionalIfControlled_CRDCheckErrorNamesTheKind(t *testing.T) {
	gw := reconciledGateway()
	r := newTestGatewayReconciler(
		mapperClient{Client: fakeClientBuilder().Build(), mapper: discoveryDownMapper{}},
		renderOutput("applied"), &mockValidator{})

	err := r.deleteOptionalIfControlled(context.Background(), gw, virtualServiceGVK, gw.Name)

	if err == nil || !strings.Contains(err.Error(), "checking VirtualService CRD") {
		t.Errorf("error = %v, want it to read \"checking VirtualService CRD\"", err)
	}
}

func TestDeleteIfControlled_ErrorNamesTheKind(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	vs := controlledChild(gw, virtualServiceGVK, gw.Name)
	c := interceptor.NewClient(fakeClientBuilder().WithObjects(vs).Build(), interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return errors.New("forbidden")
		}})
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(virtualServiceGVK)
	target.SetName(gw.Name)
	target.SetNamespace(gw.Namespace)

	err := r.deleteIfControlled(context.Background(), gw, target)

	if err == nil || !strings.Contains(err.Error(), "VirtualService") {
		t.Errorf("error = %v, want it to name the VirtualService kind", err)
	}
}

func TestGatewayReconcile_MissingOptionalCRDIsACondition(t *testing.T) {
	cases := []struct {
		name   string
		enable func(gw *v1alpha1.KrakenDGateway)
		cond   string
		status metav1.ConditionStatus
	}{
		{"Dragonfly", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
		}, v1alpha1.ConditionDragonflyReady, metav1.ConditionFalse},
		{"Istio", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Istio = &v1alpha1.IstioSpec{
				Enabled: true, Hosts: []string{"api.example.com"}, Gateways: []string{"istio-system/gw"},
			}
		}, v1alpha1.ConditionIstioConfigured, metav1.ConditionFalse},
		{"license ExternalSecret", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Edition = v1alpha1.EditionEE
			gw.Spec.License = &v1alpha1.LicenseConfig{ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{
				Enabled:        true,
				SecretStoreRef: v1alpha1.SecretStoreRef{Name: "vault", Kind: "ClusterSecretStore"},
				RemoteRef:      v1alpha1.ExternalRemoteRef{Key: "krakend/license"},
			}}
		}, v1alpha1.ConditionLicenseSecretUnavailable, metav1.ConditionTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.Status.ConfigChecksum = "applied"
			tc.enable(gw)
			c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build() // no optional CRDs
			r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

			for range 2 {
				if err := reconcileGateway(t, r, gw); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			}
			cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, tc.cond)
			if cond == nil || cond.Status != tc.status || cond.Reason != v1alpha1.ReasonCRDNotInstalled {
				t.Errorf("%s = %+v, want %s/%s", tc.cond, cond, tc.status, v1alpha1.ReasonCRDNotInstalled)
			}
			if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonCRDNotInstalled); n != 1 {
				t.Errorf("CRDNotInstalled events over two reconciles = %d, want 1", n)
			}
		})
	}
}

func TestGatewayReconcile_DragonflyNotYetCreatedRecordsOneEvent(t *testing.T) {
	gw := reconciledGateway()
	gw.Status.ConfigChecksum = "applied"
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(dragonflyGVK)).
		WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionDragonflyReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonDragonflyNotReady {
		t.Errorf("DragonflyReady = %+v, want False/%s", cond, v1alpha1.ReasonDragonflyNotReady)
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonDragonflyNotReady); n != 1 {
		t.Errorf("DragonflyNotReady events over two reconciles = %d, want 1", n)
	}
}

func TestGatewayReconcile_MissingDragonflyCRDZeroesTheReadyGauge(t *testing.T) {
	gw := reconciledGateway()
	gw.Status.ConfigChecksum = "applied"
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	dragonflyReady.WithLabelValues(gw.Namespace, gw.Name).Set(1)
	t.Cleanup(func() { dragonflyReady.DeleteLabelValues(gw.Namespace, gw.Name) })
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build() // no optional CRDs
	r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := testutil.ToFloat64(dragonflyReady.WithLabelValues(gw.Namespace, gw.Name)); got != 0 {
		t.Errorf("dragonfly_ready = %v, want 0 while the CRD is missing", got)
	}
}
