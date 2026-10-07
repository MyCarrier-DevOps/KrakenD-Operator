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
	"maps"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
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
