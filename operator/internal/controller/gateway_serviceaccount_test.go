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
	"maps"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// otherControllersServiceAccount is a ServiceAccount named like gw that a
// different controller owns.
func otherControllersServiceAccount(gw *metav1.ObjectMeta) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "other-controller", UID: "uid-other",
			Controller: ptr.To(true),
		}},
	}}
}

func TestReconcileInfrastructure_NothingRunsAsAServiceAccountAnotherControllerOwns(t *testing.T) {
	ctx := context.Background()
	gw := makeGWWithJob("echo ok")
	foreign := otherControllersServiceAccount(&gw.ObjectMeta)
	c := fakeClientBuilder().WithObjects(gw, foreign, makeConvergedDeployment(gw, "abc123")).Build()
	r := &KrakenDGatewayReconciler{Client: c, APIReader: c, Scheme: testScheme(), Recorder: fakeRecorder()}
	in := convergedInputs("abc123")
	in.configMapName = "gw-config-abc123"

	_, err := reconcileInfrastructureOf(ctx, r, gw, in)

	if err == nil {
		t.Errorf("expected the hold to be reported as an error")
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}, &d); err != nil {
		t.Fatal(err)
	}
	if got := d.Spec.Template.Spec.ServiceAccountName; got == foreign.Name {
		t.Errorf("Deployment runs as ServiceAccount %q, which another controller owns", got)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(gw.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("post-restart Jobs created: %d, want 0", len(jobs.Items))
	}
}

// A ServiceAccount named like the gateway that nothing controls and that does
// not carry the gateway's labels is somebody else's: the gateway does not take
// it over, and nothing runs as it.
func TestReconcileInfrastructure_NothingRunsAsAnUnownedServiceAccount(t *testing.T) {
	ctx := context.Background()
	gw := makeGWWithJob("echo ok")
	labels := map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	annotations := map[string]string{"azure.workload.identity/client-id": "x"}
	unowned := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace, Labels: labels, Annotations: annotations,
	}}
	c := fakeClientBuilder().WithObjects(gw, unowned).Build()
	r := &KrakenDGatewayReconciler{Client: c, APIReader: c, Scheme: testScheme(), Recorder: fakeRecorder()}
	in := convergedInputs("abc123")
	in.configMapName = "gw-config-abc123"

	_, err := reconcileInfrastructureOf(ctx, r, gw, in)

	if err == nil {
		t.Errorf("expected the hold to be reported as an error")
	}
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKeyFromObject(gw), &d); !apierrors.IsNotFound(err) {
		t.Errorf("Deployment get err = %v, want NotFound: nothing runs as the unowned ServiceAccount", err)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(gw.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("post-restart Jobs created: %d, want 0", len(jobs.Items))
	}
	var sa corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKeyFromObject(unowned), &sa); err != nil {
		t.Fatal(err)
	}
	if len(sa.OwnerReferences) != 0 {
		t.Errorf("ServiceAccount ownerReferences = %v, want none", sa.OwnerReferences)
	}
	if !maps.Equal(sa.Labels, labels) || !maps.Equal(sa.Annotations, annotations) {
		t.Errorf("ServiceAccount rewritten: labels %v, annotations %v", sa.Labels, sa.Annotations)
	}
}

// A stale read misses the other controller's ServiceAccount, so CreateOrUpdate
// stamps the gateway's reference on it and its Create fails with AlreadyExists:
// the pass must not build the Deployment on a ServiceAccount whose write failed.
func TestReconcileInfrastructure_FailedServiceAccountWriteHoldsTheDeployment(t *testing.T) {
	ctx := context.Background()
	gw := makeGWWithJob("echo ok")
	c := fakeClientBuilder().WithObjects(gw, otherControllersServiceAccount(&gw.ObjectMeta)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ServiceAccount); ok {
					return apierrors.NewNotFound(corev1.Resource("serviceaccounts"), key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &KrakenDGatewayReconciler{Client: c, APIReader: c, Scheme: testScheme(), Recorder: fakeRecorder()}
	in := convergedInputs("abc123")
	in.configMapName = "gw-config-abc123"

	if _, err := reconcileInfrastructureOf(ctx, r, gw, in); err == nil {
		t.Errorf("expected the failed ServiceAccount write to be reported")
	}

	var d appsv1.Deployment
	err := c.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}, &d)
	if err == nil {
		t.Errorf("Deployment built with serviceAccountName %q after the ServiceAccount write failed",
			d.Spec.Template.Spec.ServiceAccountName)
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

// The decision uses the object the write left behind: a new gateway, whose
// ServiceAccount this pass creates, gets its Deployment in the same pass, and
// an unowned same-named ServiceAccount is still adopted.
func TestReconcileInfrastructure_ServiceAccountCreatedOrAdoptedIsUsedInTheSamePass(t *testing.T) {
	for name, existing := range map[string]*corev1.ServiceAccount{
		"created": nil,
		"adopted": {ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gw := makeGWWithJob("echo ok")
			b := fakeClientBuilder().WithObjects(gw)
			if existing != nil {
				b = b.WithObjects(existing)
			}
			c := b.Build()
			r := &KrakenDGatewayReconciler{Client: c, APIReader: c, Scheme: testScheme(), Recorder: fakeRecorder()}
			in := convergedInputs("abc123")
			in.configMapName = "gw-config-abc123"

			if _, err := reconcileInfrastructureOf(ctx, r, gw, in); err != nil {
				t.Fatalf("reconcileInfrastructure: %v", err)
			}

			var d appsv1.Deployment
			if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "gw"}, &d); err != nil {
				t.Fatalf("the Deployment must be created in the same pass: %v", err)
			}
		})
	}
}

func TestGatewayReconcile_NewConfigUnderAForeignServiceAccountReportsNoRollout(t *testing.T) {
	gw := reconciledGateway()
	c := fakeClientBuilder().WithObjects(gw, otherControllersServiceAccount(&gw.ObjectMeta)).
		WithStatusSubresource(gw).Build()
	rec := fakeRecorder()
	r := acceptanceReconciler(c, rec, &renderer.RenderOutput{JSON: []byte(`{"version":3}`), Checksum: "cs1"})

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Errorf("expected the hold to be reported as an error")
	}

	if events := drainEvents(rec); hasEventReason(events, v1alpha1.ReasonConfigDeployed) {
		t.Errorf("events = %q, want no ConfigDeployed: the held Deployment starts no rollout", events)
	}
}

// reconcileInfrastructureOf runs the core resources, then the infrastructure
// stage on their outcome, as Reconcile does.
func reconcileInfrastructureOf(
	ctx context.Context, r *KrakenDGatewayReconciler, gw *v1alpha1.KrakenDGateway, in infraInputs,
) (deploymentObservation, error) {
	saControlled, coreErr := r.reconcileCoreResources(ctx, gw, in)
	return r.reconcileInfrastructure(ctx, gw, in, saControlled, coreErr)
}
