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

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// These tests cover review round 4, D2/D5: the
// ConditionDragonflyRunAsRootUnacknowledged status condition's three
// possible states (True/RunAsRootUnacknowledged,
// False/RunAsRootAcknowledged, False/NoRunAsRootRequest) and D3's
// disable-clears behavior. (i)-(iii) exercise
// recordDragonflyRunAsRootCondition directly against a BUILT Dragonfly CR
// (resources.BuildDragonfly) — the webhook is not in the reconcile path, so
// the spec is set directly here rather than going through ValidateCreate,
// mirroring how a grandfathered/webhook-bypassed spec would actually reach
// the controller.

// TestRecordDragonflyRunAsRootCondition_Unacknowledged covers (i): a
// pod-scope runAsUser: 0 with runAsNonRoot left unset renders unacknowledged
// on the built CR (see resources.DragonflyRunAsRootUnacknowledged) — the
// condition must be True/RunAsRootUnacknowledged with
// ObservedGeneration == gw.Generation.
func TestRecordDragonflyRunAsRootCondition_Unacknowledged(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns", Generation: 3},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	df := &unstructured.Unstructured{}
	resources.BuildDragonfly(df, gw)

	r := &KrakenDGatewayReconciler{}
	r.recordDragonflyRunAsRootCondition(gw, df)

	cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionDragonflyRunAsRootUnacknowledged)
	if cond == nil {
		t.Fatal("expected ConditionDragonflyRunAsRootUnacknowledged to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected status True, got %v", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonDragonflyRunAsRootUnacknowledged {
		t.Errorf("expected reason %q, got %q", v1alpha1.ReasonDragonflyRunAsRootUnacknowledged, cond.Reason)
	}
	if cond.ObservedGeneration != gw.Generation {
		t.Errorf("expected ObservedGeneration %d, got %d", gw.Generation, cond.ObservedGeneration)
	}
}

// TestRecordDragonflyRunAsRootCondition_Acknowledged covers (ii): a
// container-scope runAsUser: 0 with an explicit runAsNonRoot: false
// acknowledges the request — the condition must be
// False/RunAsRootAcknowledged.
func TestRecordDragonflyRunAsRootCondition_Acknowledged(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns", Generation: 5},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	df := &unstructured.Unstructured{}
	resources.BuildDragonfly(df, gw)

	r := &KrakenDGatewayReconciler{}
	r.recordDragonflyRunAsRootCondition(gw, df)

	cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionDragonflyRunAsRootUnacknowledged)
	if cond == nil {
		t.Fatal("expected ConditionDragonflyRunAsRootUnacknowledged to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected status False, got %v", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonDragonflyRunAsRootAcknowledged {
		t.Errorf("expected reason %q, got %q", v1alpha1.ReasonDragonflyRunAsRootAcknowledged, cond.Reason)
	}
	if cond.ObservedGeneration != gw.Generation {
		t.Errorf("expected ObservedGeneration %d, got %d", gw.Generation, cond.ObservedGeneration)
	}
}

// TestRecordDragonflyRunAsRootCondition_NoRequest covers (iii): the default,
// no-root-requested case (the vast majority of gateways) — the condition
// must be False/NoRunAsRootRequest, DISTINCT from RunAsRootAcknowledged.
func TestRecordDragonflyRunAsRootCondition_NoRequest(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns", Generation: 1},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
			},
		},
	}
	df := &unstructured.Unstructured{}
	resources.BuildDragonfly(df, gw)

	r := &KrakenDGatewayReconciler{}
	r.recordDragonflyRunAsRootCondition(gw, df)

	cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionDragonflyRunAsRootUnacknowledged)
	if cond == nil {
		t.Fatal("expected ConditionDragonflyRunAsRootUnacknowledged to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected status False, got %v", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonDragonflyRunAsRootNoRequest {
		t.Errorf("expected reason %q, got %q", v1alpha1.ReasonDragonflyRunAsRootNoRequest, cond.Reason)
	}
	if cond.Reason == v1alpha1.ReasonDragonflyRunAsRootAcknowledged {
		t.Error("NoRunAsRootRequest must be distinct from RunAsRootAcknowledged (D5b reason split)")
	}
}

// TestReconcileOwnedResources_DragonflyDisabledClearsStaleRunAsRootCondition
// covers D3/D2(iv): when Dragonfly is deliberately off
// (gw.Spec.Dragonfly == nil or Enabled: false), a stale
// ConditionDragonflyRunAsRootUnacknowledged left over from a prior reconcile
// (while Dragonfly WAS enabled) must be removed — mirroring
// reconcilePostRestartJob's disabled/empty guard
// (TestReconcilePostRestartJob_DisabledSpecClearsStaleConditions).
func TestReconcileOwnedResources_DragonflyDisabledClearsStaleRunAsRootCondition(t *testing.T) {
	gw := testGateway()
	gw.Spec.Dragonfly = nil // deliberately off
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDragonflyRunAsRootUnacknowledged,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonDragonflyRunAsRootUnacknowledged,
		Message:            "stale from a prior reconcile while Dragonfly was enabled",
	})

	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	output := &renderer.RenderOutput{
		JSON:         []byte(`{"version":3}`),
		Checksum:     "abc123",
		DesiredImage: "krakend/krakend-ce:2.7.0",
	}
	if err := r.reconcileOwnedResources(context.Background(), gw, output); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionDragonflyRunAsRootUnacknowledged) != nil {
		t.Error("expected ConditionDragonflyRunAsRootUnacknowledged cleared when Dragonfly is disabled")
	}
}
