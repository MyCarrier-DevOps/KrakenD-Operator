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
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
