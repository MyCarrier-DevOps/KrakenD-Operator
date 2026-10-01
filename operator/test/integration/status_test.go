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
	"strconv"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
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

// createGateway creates a CE KrakenDGateway named name in ns.
func createGateway(t *testing.T, ns, name string) client.ObjectKey {
	t.Helper()
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.9", Edition: v1alpha1.EditionCE, Config: v1alpha1.GatewayConfig{}},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway %s: %v", name, err)
	}
	return client.ObjectKeyFromObject(gw)
}

// createEndpoint creates a KrakenDEndpoint named name in ns on gateway, with
// one GET entry per path.
func createEndpoint(t *testing.T, ns, name, gateway string, paths ...string) client.ObjectKey {
	t.Helper()
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: gateway}},
	}
	for _, p := range paths {
		ep.Spec.Endpoints = append(ep.Spec.Endpoints, v1alpha1.EndpointEntry{
			Endpoint: p,
			Method:   "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: p}},
		})
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint %s: %v", name, err)
	}
	return client.ObjectKeyFromObject(ep)
}

// getEndpoint reads the endpoint key from the API server.
func getEndpoint(key client.ObjectKey) (*v1alpha1.KrakenDEndpoint, error) {
	var ep v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// expectCondition returns an error unless ep carries condition typ with the
// given status and reason, observed at ep's current generation.
func expectCondition(ep *v1alpha1.KrakenDEndpoint, typ string, status metav1.ConditionStatus, reason string) error {
	c := meta.FindStatusCondition(ep.Status.Conditions, typ)
	switch {
	case c == nil:
		return fmt.Errorf("%s: no %s condition (conditions %+v)", ep.Name, typ, ep.Status.Conditions)
	case c.Status != status || c.Reason != reason:
		return fmt.Errorf("%s: %s = %s/%s, want %s/%s", ep.Name, typ, c.Status, c.Reason, status, reason)
	case c.ObservedGeneration != ep.Generation:
		return fmt.Errorf("%s: %s observed generation %d, endpoint is at %d",
			ep.Name, typ, c.ObservedGeneration, ep.Generation)
	}
	return nil
}

// touchGateway sets a test annotation on the gateway, which re-runs its
// reconcile without changing its spec.
func touchGateway(t *testing.T, key client.ObjectKey, value string) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var gw v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, key, &gw); err != nil {
			return err
		}
		if gw.Annotations == nil {
			gw.Annotations = map[string]string{}
		}
		gw.Annotations["test.krakend.io/touch"] = value
		return k8sClient.Update(ctx, &gw)
	})
	if err != nil {
		t.Fatalf("annotating gateway %s: %v", key, err)
	}
}

// eventCount sums the counts of the events with the given reason recorded on
// the object named name in ns.
func eventCount(ns, name, reason string) (int32, error) {
	var list corev1.EventList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	var total int32
	for i := range list.Items {
		ev := &list.Items[i]
		if ev.InvolvedObject.Name == name && ev.Reason == reason {
			total += max(ev.Count, 1)
		}
	}
	return total, nil
}

func TestGatewayAcceptance_MarksIncludedAndConflictedEndpoints(t *testing.T) {
	ns := testNamespace(t)
	gw := createGateway(t, ns, "gw-accept")
	// ep-a is created first and sorts first, so it wins GET /users whether or
	// not both creation timestamps fall in the same second.
	older := createEndpoint(t, ns, "ep-a-older", gw.Name, "/users")
	eventually(t, func() error {
		ep, err := getEndpoint(older)
		if err != nil {
			return err
		}
		return expectCondition(ep, "Accepted", metav1.ConditionTrue, "Accepted")
	})
	newer := createEndpoint(t, ns, "ep-b-newer", gw.Name, "/users", "/only-b")
	eventually(t, func() error {
		ep, err := getEndpoint(newer)
		if err != nil {
			return err
		}
		return expectCondition(ep, "Accepted", metav1.ConditionFalse, "EndpointConflict")
	})
	oneConflictEvent := func() error {
		n, err := eventCount(ns, "ep-b-newer", "EndpointConflict")
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("EndpointConflict events on ep-b-newer = %d, want 1", n)
		}
		return nil
	}
	eventually(t, oneConflictEvent)

	// Each gateway reconcile re-evaluates acceptance; an unchanged verdict is
	// neither rewritten nor announced again.
	for i := range 3 {
		touchGateway(t, gw, strconv.Itoa(i))
	}
	consistently(t, 5*time.Second, func() error {
		if err := oneConflictEvent(); err != nil {
			return err
		}
		ep, err := getEndpoint(newer)
		if err != nil {
			return err
		}
		return expectCondition(ep, "Accepted", metav1.ConditionFalse, "EndpointConflict")
	})
}
