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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	_, err := r.reconcileInfrastructure(ctx, gw, in)

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
