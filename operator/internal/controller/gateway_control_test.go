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
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
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

// foreignDeployment is a Deployment named like gw that nothing controls and
// that lacks the gateway's labels: somebody else's, which the gateway refuses.
func foreignDeployment(gw *v1alpha1.KrakenDGateway) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: new(int32(5))},
	}
}

// The HPA scales the Deployment named like the gateway. While the gateway
// refuses that Deployment, the HPA would scale somebody else's, so it is not
// written.
func TestGatewayReconcile_NoHPAForARefusedDeployment(t *testing.T) {
	gw := reconciledGateway()
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: new(int32(1)), MaxReplicas: 1}
	c := fakeClientBuilder().WithObjects(gw, foreignDeployment(gw)).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	if err := reconcileGateway(t, r, gw); len(notControlledIn(err)) != 1 {
		t.Fatalf("err = %v, want the Deployment's refusal", err)
	}

	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(gw), &hpa); !apierrors.IsNotFound(err) {
		t.Errorf("HPA get err = %v, want NotFound: it targets %+v, the refused Deployment",
			err, hpa.Spec.ScaleTargetRef)
	}
}

// An HPA the gateway wrote before its Deployment was refused would keep
// scaling somebody else's Deployment, so it is deleted.
func TestGatewayReconcile_DeletesItsHPAWhileTheDeploymentIsRefused(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: new(int32(1)), MaxReplicas: 1}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace, OwnerReferences: ownedBy(gw),
	}}
	c := fakeClientBuilder().WithObjects(gw, foreignDeployment(gw), hpa).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	if err := reconcileGateway(t, r, gw); len(notControlledIn(err)) != 1 {
		t.Fatalf("err = %v, want the Deployment's refusal", err)
	}

	if err := c.Get(t.Context(), client.ObjectKeyFromObject(hpa), hpa); !apierrors.IsNotFound(err) {
		t.Errorf("HPA get err = %v, want NotFound: the gateway's HPA must not scale the refused Deployment", err)
	}
}

// While the Deployment is held its step does not run, so nothing refuses the
// Deployment named like the gateway, but the pass's read still tells whether
// the gateway controls it. One it does not control is never the target of an
// HPA the gateway writes, and the gateway's own HPA is deleted.
func TestGatewayReconcile_NoHPAForAForeignDeploymentWhileItIsHeld(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(gw *v1alpha1.KrakenDGateway)
		rend  renderer.Renderer
		val   renderer.Validator
	}{
		"a plugin ConfigMap is missing": {
			setup: func(gw *v1alpha1.KrakenDGateway) {
				gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
					{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "missing", Key: "auth.so"}},
				}}
			},
			rend: renderOf(`{"version":3}`), val: &mockValidator{},
		},
		"no config is applied yet": {
			setup: func(*v1alpha1.KrakenDGateway) {},
			rend:  renderOf(`{"version":3,"name":"rejected"}`),
			val:   &countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")},
		},
		"no ConfigMap holds the applied config": {
			setup: func(gw *v1alpha1.KrakenDGateway) {
				gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(`{"version":3,"name":"gone"}`))
			},
			rend: renderOf(`{"version":3,"name":"rejected"}`),
			val:  &countingValidator{err: rejectedBy("- at '/endpoints/0/endpoint': bad")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.UID = "gw-uid"
			gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: new(int32(1)), MaxReplicas: 1}
			tc.setup(gw)
			hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
				Name: gw.Name, Namespace: gw.Namespace, OwnerReferences: ownedBy(gw),
			}}
			c := fakeClientBuilder().WithObjects(gw, foreignDeployment(gw), hpa).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, tc.rend, tc.val)

			_ = reconcileGateway(t, r, gw)

			if err := c.Get(t.Context(), client.ObjectKeyFromObject(hpa), hpa); !apierrors.IsNotFound(err) {
				t.Errorf("HPA get err = %v (target %+v), want NotFound: the gateway's HPA must not scale a "+
					"Deployment the gateway does not control", err, hpa.Spec.ScaleTargetRef)
			}
		})
	}
}

// Nothing rolls a refused Deployment, which is somebody else's: a newly applied
// config raises no Progressing beside it. ResourcesControlled reports the
// refusal instead.
func TestGatewayReconcile_ARefusedDeploymentRaisesNoProgressing(t *testing.T) {
	gw := reconciledGateway()
	c := fakeClientBuilder().WithObjects(gw, foreignDeployment(gw)).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	if err := reconcileGateway(t, r, gw); len(notControlledIn(err)) != 1 {
		t.Fatalf("err = %v, want the Deployment's refusal", err)
	}

	got := getGateway(t, c, gw)
	if got.Status.ConfigChecksum != "cs1" {
		t.Fatalf("configChecksum = %q, want the newly applied cs1", got.Status.ConfigChecksum)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing); condTrue(cond) {
		t.Errorf("Progressing = %+v, want it not raised: nothing rolls the refused Deployment", cond)
	}
}

// A refusal lasts until someone renames the gateway or hands the Deployment
// over, and the config stage keeps publishing a ConfigMap per applied config
// meanwhile: collection keeps them to the revision history.
func TestGatewayReconcile_CollectsConfigRevisionsWhileTheDeploymentIsRefused(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	c := fakeClientBuilder().WithObjects(gw, foreignDeployment(gw)).WithStatusSubresource(gw).Build()
	rend := renderOf(`{"version":3,"name":"c0"}`)
	r := newTestGatewayReconciler(c, rend, &mockValidator{})

	for i := range 6 {
		rend.output = renderOf(fmt.Sprintf(`{"version":3,"name":"c%d"}`, i)).output
		if err := reconcileGateway(t, r, gw); len(notControlledIn(err)) != 1 {
			t.Fatalf("pass %d: err = %v, want the Deployment's refusal", i, err)
		}
	}

	applied := resources.ConfigMapName(gw, getGateway(t, c, gw).Status.ConfigChecksum)
	if got := remainingConfigMaps(t, c, gw); len(got) != configMapHistoryLimit || !slices.Contains(got, applied) {
		t.Errorf("config ConfigMaps after 6 applied configs = %v, want %d with the applied %s among them",
			got, configMapHistoryLimit, applied)
	}
}

// A Deployment write that fails for another reason refuses nothing: the HPA is
// written as before.
func TestGatewayReconcile_AFailedDeploymentWriteStillWritesTheHPA(t *testing.T) {
	s := serveGateway(t)
	s.failDeploymentWrites()
	s.editSpec(t, func(spec *v1alpha1.KrakenDGatewaySpec) {
		spec.Image = "img:v2"
		spec.Autoscaling = &v1alpha1.AutoscalingSpec{MaxReplicas: 5}
	})

	if err := reconcileGateway(t, s.r, s.gw); err == nil || len(notControlledIn(err)) != 0 {
		t.Fatalf("err = %v, want the failed Deployment write and no refusal", err)
	}

	var hpa autoscalingv2.HorizontalPodAutoscaler
	getObject(t, s.c, s.gw, s.gw.Name, &hpa)
	if hpa.Spec.MaxReplicas != 5 {
		t.Errorf("HPA maxReplicas = %d, want 5", hpa.Spec.MaxReplicas)
	}
}

// The VirtualService routes to the Service named like the gateway. While the
// gateway refuses that Service, the VirtualService would publish somebody
// else's pods on the gateway's hosts, so it is not written, and
// IstioConfigured says why.
func TestGatewayReconcile_NoVirtualServiceForARefusedService(t *testing.T) {
	for name, also := range map[string]func(gw *v1alpha1.KrakenDGateway) []client.Object{
		"the Service": func(*v1alpha1.KrakenDGateway) []client.Object { return nil },
		"the Service, while the ServiceAccount holds the Deployment": func(gw *v1alpha1.KrakenDGateway) []client.Object {
			return []client.Object{otherControllersServiceAccount(&gw.ObjectMeta)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			gw := reconciledGateway()
			gw.Spec.Istio = &v1alpha1.IstioSpec{
				Enabled: true, Hosts: []string{"public.example.com"}, Gateways: []string{"istio-system/public"},
			}
			victim := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
				Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "victim-db-admin"}},
			}
			c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(virtualServiceGVK)).
				WithObjects(append(also(gw), gw, victim)...).WithStatusSubresource(gw).Build()
			r := acceptanceReconciler(c, fakeRecorder(),
				&renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

			if err := reconcileGateway(t, r, gw); len(notControlledIn(err)) == 0 {
				t.Fatalf("err = %v, want the Service's refusal", err)
			}

			vs := &unstructured.Unstructured{}
			vs.SetGroupVersionKind(virtualServiceGVK)
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(gw), vs); !apierrors.IsNotFound(err) {
				t.Errorf("VirtualService get err = %v, want NotFound: it routes %v to the refused Service",
					err, vs.Object["spec"])
			}
			cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionIstioConfigured)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonResourceNotControlled {
				t.Errorf("IstioConfigured = %+v, want False/ResourceNotControlled", cond)
			}
		})
	}
}

// A VirtualService the gateway wrote before its Service was refused would keep
// routing to somebody else's pods, so it is deleted.
func TestGatewayReconcile_DeletesItsVirtualServiceWhileTheServiceIsRefused(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Spec.Istio = &v1alpha1.IstioSpec{Enabled: true, Hosts: []string{"public.example.com"}}
	vs := controlledChild(gw, virtualServiceGVK, gw.Name)
	victim := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace}}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(virtualServiceGVK)).
		WithObjects(gw, victim, vs).WithStatusSubresource(gw).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	if err := reconcileGateway(t, r, gw); len(notControlledIn(err)) != 1 {
		t.Fatalf("err = %v, want the Service's refusal", err)
	}

	if err := c.Get(t.Context(), client.ObjectKeyFromObject(vs), vs); !apierrors.IsNotFound(err) {
		t.Errorf("VirtualService get err = %v, want NotFound: the gateway's VirtualService must not route to "+
			"the refused Service", err)
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

			if got := len(notControlledIn(err)) == 1; got != tc.refused {
				t.Fatalf("refused = %v (err = %v), want %v", got, err, tc.refused)
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

			if got := len(notControlledIn(err)) == 1; got != tc.refused {
				t.Fatalf("refused = %v (err = %v), want %v", got, err, tc.refused)
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

// A Dragonfly named like the gateway's that the gateway refuses is somebody
// else's: the gateway does not report it ready, does not publish its address,
// and does not point the rendered Redis pool at it.
func TestGatewayReconcile_ARefusedDragonflyIsNotTheGateways(t *testing.T) {
	gw := reconciledGateway()
	gw.UID = "gw-uid"
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	df := controlledChild(gw, dragonflyGVK, resources.DragonflyName(gw))
	df.SetOwnerReferences(nil)
	if err := unstructured.SetNestedField(df.Object, "ready", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	c := fakeClientBuilder().WithRESTMapper(optionalCRDMapper(dragonflyGVK)).
		WithObjects(gw, df).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderer.New(renderer.Options{}), &mockValidator{})

	if err := reconcileGateway(t, r, gw); len(notControlledIn(err)) != 1 {
		t.Fatalf("err = %v, want the Dragonfly's refusal", err)
	}

	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionDragonflyReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonResourceNotControlled {
		t.Errorf("DragonflyReady = %+v, want False/ResourceNotControlled", cond)
	}
	if got.Status.DragonflyAddress != "" {
		t.Errorf("dragonflyAddress = %q, want none for a Dragonfly the gateway refuses", got.Status.DragonflyAddress)
	}
	var cm corev1.ConfigMap
	getObject(t, c, gw, resources.ConfigMapName(gw, got.Status.ConfigChecksum), &cm)
	if dns := resources.DragonflyServiceDNS(gw); strings.Contains(cm.Data[resources.ConfigKey], dns) {
		t.Errorf("the rendered config points at %s, the refused Dragonfly's Service", dns)
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

// While a child the gateway refused cannot be evaluated this pass, because its
// write fails for another reason, the condition keeps what it said.
func TestGatewayReconcile_AFailedWriteDoesNotClearResourcesControlled(t *testing.T) {
	gw := reconciledGateway()
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionResourcesControlled, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonResourceNotControlled, Message: "service default/test-gw is somebody else's",
	})
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Service); ok {
					return errors.New("the server is currently unable to handle the request")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := acceptanceReconciler(c, fakeRecorder(), &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatalf("expected the failed Service write to be reported")
	}

	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionResourcesControlled)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("ResourcesControlled = %+v, want it to stay False", cond)
	}
}

// A Deployment the pass never wrote, because a hold applies, is not one the
// gateway is known to control: the condition does not turn True on it.
func TestGatewayReconcile_AHeldDeploymentDoesNotSetResourcesControlled(t *testing.T) {
	gw := reconciledGateway()
	const config = `{"version":3,"name":"with-plugins"}`
	gw.Status.ConfigChecksum = hash.SHA256Hex([]byte(config))
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{
		{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "plugins-a", Key: "auth.so"}},
	}}
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionResourcesControlled, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonResourceNotControlled, Message: "deployment default/test-gw is somebody else's",
	})
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionResourcesControlled)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("ResourcesControlled = %+v, want it to stay False while the Deployment is held", cond)
	}
}
