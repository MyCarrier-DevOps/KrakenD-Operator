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

package controller

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// A Service named like the gateway that nothing controls and that does not
// carry the gateway's labels is somebody else's: its selector, which the
// gateway would rewrite to its own pods, is left alone, and the gateway's
// status names it.
func TestGatewayReconcile_UnownedServiceIsNotTakenOver(t *testing.T) {
	gw := reconciledGateway()
	selector := map[string]string{"app": "victim"}
	victim := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Spec:       corev1.ServiceSpec{Selector: selector},
	}
	c := fakeClientBuilder().WithObjects(gw, victim).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Errorf("expected the refusal to be reported as an error")
	}

	var svc corev1.Service
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(victim), &svc); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(svc.Spec.Selector, selector) || len(svc.OwnerReferences) != 0 {
		t.Errorf("Service taken over: selector %v, ownerReferences %v", svc.Spec.Selector, svc.OwnerReferences)
	}
	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionResourcesControlled)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonResourceNotControlled {
		t.Fatalf("ResourcesControlled = %+v, want False/ResourceNotControlled", cond)
	}
	if want := "service default/test-gw"; !strings.Contains(cond.Message, want) {
		t.Errorf("message %q does not name %q", cond.Message, want)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonResourceNotControlled {
		t.Errorf("Ready = %+v, want False/ResourceNotControlled", ready)
	}
}

// A Deployment named like the gateway that nothing controls and that does not
// carry the gateway's labels is somebody else's: the gateway does not rewrite
// it, and its status names it.
func TestReconcileInfrastructure_UnownedDeploymentIsNotTakenOver(t *testing.T) {
	ctx := context.Background()
	gw := makeGWWithJob("echo ok")
	victim := makeConvergedDeployment(gw, "abc123")
	victim.OwnerReferences = nil
	c := fakeClientBuilder().WithObjects(gw, victim).Build()
	r := &KrakenDGatewayReconciler{Client: c, APIReader: c, Scheme: testScheme(), Recorder: fakeRecorder()}
	in := convergedInputs("abc123")
	in.configMapName = "gw-config-abc123"

	_, err := reconcileInfrastructureOf(ctx, r, gw, in)

	if err == nil {
		t.Errorf("expected the refusal to be reported as an error")
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKeyFromObject(victim), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.OwnerReferences) != 0 || len(d.Labels) != 0 {
		t.Errorf("Deployment taken over: ownerReferences %v, labels %v", d.OwnerReferences, d.Labels)
	}
	refused := notControlledIn(err)
	if len(refused) != 1 || refused[0].kind != "deployment" {
		t.Errorf("refused = %v, want the deployment", refused)
	}
}

// What the gateway controls, or an object orphaned from it that still carries
// its selector labels, is written as always; an object of an optional kind
// that nothing controls and that lacks the labels is not, and the caller can
// tell why.
func TestApplyOptional_TakesOverOnlyWhatTheGatewayMayControl(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	existing := func(mutate func(u *unstructured.Unstructured)) *unstructured.Unstructured {
		u := controlledChild(gw, virtualServiceGVK, gw.Name)
		mutate(u)
		return u
	}
	for name, tc := range map[string]struct {
		existing *unstructured.Unstructured
		refused  bool
	}{
		"controlled by the gateway": {existing: existing(func(*unstructured.Unstructured) {})},
		"orphaned with its labels": {existing: existing(func(u *unstructured.Unstructured) {
			u.SetOwnerReferences(nil)
			u.SetLabels(resources.SelectorLabels(gw))
		})},
		"unlabelled and uncontrolled": {existing: existing(func(u *unstructured.Unstructured) {
			u.SetOwnerReferences(nil)
		}), refused: true},
		"labelled but controlled by another": {existing: existing(func(u *unstructured.Unstructured) {
			u.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "other", UID: "other-uid", Controller: new(true),
			}})
			u.SetLabels(resources.SelectorLabels(gw))
		}), refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
				WithObjects(gw, tc.existing).Build()
			r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

			_, applied, err := r.applyOptional(context.Background(), gw, virtualServiceGVK, gw.Name,
				resources.SelectorLabels(gw), func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"built": "yes"}) })

			if got := errors.Is(err, errNotControlled); got != tc.refused {
				t.Fatalf("errors.Is(err, errNotControlled) = %v (err = %v), want %v", got, err, tc.refused)
			}
			if !tc.refused && (err != nil || !applied) {
				t.Fatalf("applied = %v, err = %v", applied, err)
			}
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(virtualServiceGVK)
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(tc.existing), stored); err != nil {
				t.Fatal(err)
			}
			if written := stored.GetLabels()["built"] == "yes"; written == tc.refused {
				t.Errorf("object written = %v, want %v (labels %v)", written, !tc.refused, stored.GetLabels())
			}
			if !tc.refused && !metav1.IsControlledBy(stored, gw) {
				t.Errorf("not controlled by the gateway after the write: %v", stored.GetOwnerReferences())
			}
		})
	}
}

// A Service orphaned from the gateway by `kubectl delete --cascade=orphan`
// still carries its selector labels and is taken back; one the gateway
// controls is written as always.
func TestGatewayReconcile_ServiceTheGatewayMayControlIsRewritten(t *testing.T) {
	for name, existing := range map[string]func(gw *v1alpha1.KrakenDGateway) *corev1.Service{
		"orphaned with its labels": func(gw *v1alpha1.KrakenDGateway) *corev1.Service {
			return &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: gw.Name, Namespace: gw.Namespace, Labels: resources.SelectorLabels(gw),
			}}
		},
		"controlled by the gateway": func(gw *v1alpha1.KrakenDGateway) *corev1.Service {
			return &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: gw.Name, Namespace: gw.Namespace, OwnerReferences: ownedBy(gw),
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			gw := reconciledGateway()
			svc := existing(gw)
			svc.Spec.Selector = map[string]string{"app": "stale"}
			c := fakeClientBuilder().WithObjects(gw, svc).WithStatusSubresource(gw).Build()
			r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

			_ = reconcileGateway(t, r, gw)

			var got corev1.Service
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(svc), &got); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got.Spec.Selector, resources.SelectorLabels(gw)) || !metav1.IsControlledBy(&got, gw) {
				t.Errorf("Service not written: selector %v, ownerReferences %v", got.Spec.Selector, got.OwnerReferences)
			}
			cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionResourcesControlled)
			if cond == nil || cond.Status != metav1.ConditionTrue {
				t.Errorf("ResourcesControlled = %+v, want True", cond)
			}
		})
	}
}

// An orphaned Dragonfly still carries the labels the operator stamps on that
// kind, which are not the gateway's selector labels: it is taken back, and one
// that carries only the selector labels is somebody else's.
func TestReconcileDragonfly_TakesOverOnlyWhatCarriesItsOwnLabels(t *testing.T) {
	for name, tc := range map[string]struct {
		labels  func(gw *v1alpha1.KrakenDGateway) map[string]string
		refused bool
	}{
		"its own labels":      {labels: resources.DragonflyLabels},
		"the selector labels": {labels: resources.SelectorLabels, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.UID = "gw-uid"
			gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
			orphan := controlledChild(gw, dragonflyGVK, resources.DragonflyName(gw))
			orphan.SetOwnerReferences(nil)
			orphan.SetLabels(tc.labels(gw))
			c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
				WithObjects(gw, orphan).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOutput("applied"), &mockValidator{})

			err := r.reconcileDragonfly(context.Background(), gw)

			if got := errors.Is(err, errNotControlled); got != tc.refused {
				t.Fatalf("errors.Is(err, errNotControlled) = %v (err = %v), want %v", got, err, tc.refused)
			}
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(dragonflyGVK)
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(orphan), stored); err != nil {
				t.Fatal(err)
			}
			if got := metav1.IsControlledBy(stored, gw); got == tc.refused {
				t.Errorf("controlled by the gateway = %v, want %v", got, !tc.refused)
			}
		})
	}
}

// Every refused object is named, sorted, with the labels that hand that object
// over: its own kind's, not one set for all.
func TestGatewayReconcile_EveryRefusedObjectIsNamedWithItsOwnLabels(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	named := metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace}
	dragonfly := controlledChild(gw, dragonflyGVK, resources.DragonflyName(gw))
	dragonfly.SetOwnerReferences(nil)
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(optionalOwnedGVKs...)).
		WithObjects(gw, dragonfly, &corev1.Service{ObjectMeta: named}, &policyv1.PodDisruptionBudget{ObjectMeta: named}).
		WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	_ = reconcileGateway(t, r, gw)

	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionResourcesControlled)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("ResourcesControlled = %+v, want False", cond)
	}
	wantInOrder := []string{
		"dragonfly default/test-gw-dragonfly has no controller and lacks the labels " +
			"app.kubernetes.io/instance=test-gw-dragonfly,app.kubernetes.io/managed-by=krakend-operator",
		"pdb default/test-gw has no controller and lacks the labels " +
			"app.kubernetes.io/instance=test-gw,app.kubernetes.io/managed-by=krakend-operator",
		"service default/test-gw has no controller and lacks the labels " +
			"app.kubernetes.io/instance=test-gw,app.kubernetes.io/managed-by=krakend-operator",
	}
	rest := cond.Message
	for _, want := range wantInOrder {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("message %q lacks %q, in order", cond.Message, want)
		}
		rest = rest[i+len(want):]
	}
}
