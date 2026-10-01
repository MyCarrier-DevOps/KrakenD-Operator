//go:build integration

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

package integration

import (
	"fmt"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestStatusPatch_MergePatchReplacesConditionsUnlessLocked pins the API
// server behavior both endpoint status writers rely on. Even with
// status.conditions declared as a list map, a JSON merge patch replaces the
// whole list: a patch computed from a stale read drops a condition another
// writer added since, and the same patch with an optimistic lock is rejected.
func TestStatusPatch_MergePatchReplacesConditionsUnlessLocked(t *testing.T) {
	ns := testNamespace(t)
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-premise", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "no-such-gateway"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	key := client.ObjectKeyFromObject(ep)
	// Let the endpoint controller's first status write land so it cannot race
	// the patches below; nothing else re-triggers it for this endpoint.
	eventually(t, func() error {
		var cur v1alpha1.KrakenDEndpoint
		if err := k8sClient.Get(ctx, key, &cur); err != nil {
			return err
		}
		if cur.Status.ObservedGeneration != cur.Generation {
			return fmt.Errorf("endpoint status not written yet")
		}
		return nil
	})

	var stale v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &stale); err != nil {
		t.Fatal(err)
	}
	var fresh v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type: "ExampleWriterA", Status: metav1.ConditionTrue, Reason: "Example", Message: "written after the stale read",
	})
	if err := k8sClient.Status().Update(ctx, &fresh); err != nil {
		t.Fatalf("writer A: %v", err)
	}

	writerB := metav1.Condition{
		Type: "ExampleWriterB", Status: metav1.ConditionTrue, Reason: "Example", Message: "computed from the stale read",
	}
	locked := stale.DeepCopy()
	meta.SetStatusCondition(&locked.Status.Conditions, writerB)
	err := k8sClient.Status().Patch(ctx, locked,
		client.MergeFromWithOptions(&stale, client.MergeFromWithOptimisticLock{}))
	if !apierrors.IsConflict(err) {
		t.Fatalf("locked merge patch from a stale read: got %v, want a Conflict", err)
	}

	unlocked := stale.DeepCopy()
	meta.SetStatusCondition(&unlocked.Status.Conditions, writerB)
	if err := k8sClient.Status().Patch(ctx, unlocked, client.MergeFrom(&stale)); err != nil {
		t.Fatalf("unlocked merge patch: %v", err)
	}
	var after v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(after.Status.Conditions, "ExampleWriterA") != nil {
		t.Fatal("an unlocked merge patch kept ExampleWriterA: the list was merged by key, " +
			"so the premise behind the optimistic lock no longer holds")
	}
}
