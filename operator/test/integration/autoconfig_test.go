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
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	specConfigMapName = "pets-openapi"
	specConfigMapKey  = "openapi.json"
	autoConfigName    = "pets"
)

// initialOperations is the spec the fixture AutoConfig starts from
// (path -> operationId of its GET operation).
var initialOperations = map[string]string{
	"/pets":   "listPets",
	"/owners": "listOwners",
}

// initialEndpointNames are the KrakenDEndpoints generated from initialOperations.
var initialEndpointNames = []string{"pets-listpets", "pets-listowners"}

// consistently fails the test if check returns an error at any point during d.
func consistently(t *testing.T, d time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if err := check(); err != nil {
			t.Fatalf("condition stopped holding: %v", err)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// openAPISpec returns a minimal OpenAPI 3 JSON document with one GET
// operation per path in operations (path -> operationId).
func openAPISpec(t *testing.T, operations map[string]string) string {
	t.Helper()
	paths := map[string]any{}
	for path, operationID := range operations {
		paths[path] = map[string]any{
			"get": map[string]any{
				"operationId": operationID,
				"responses":   map[string]any{"200": map[string]any{"description": "OK"}},
			},
		}
	}
	raw, err := json.Marshal(map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": "pets", "version": "1.0.0"},
		"paths":   paths,
	})
	if err != nil {
		t.Fatalf("marshal OpenAPI spec: %v", err)
	}
	return string(raw)
}

// newSyncedAutoConfig creates a gateway, a spec ConfigMap and an OnChange
// AutoConfig sourcing that ConfigMap in a fresh namespace, and waits until the
// AutoConfig is Synced and Ready and every generated endpoint is Active.
func newSyncedAutoConfig(t *testing.T) *v1alpha1.KrakenDAutoConfig {
	t.Helper()
	ns := testNamespace(t)

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-autoconfig", Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}

	// Generated endpoints look their gateway up in the manager's cache; wait
	// until the gateway controller has seen it so they go straight to Active.
	eventually(t, func() error {
		var cur v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &cur); err != nil {
			return err
		}
		if cur.Status.Phase == "" {
			return fmt.Errorf("gateway not yet reconciled")
		}
		return nil
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: specConfigMapName, Namespace: ns},
		Data:       map[string]string{specConfigMapKey: openAPISpec(t, initialOperations)},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("create spec configmap: %v", err)
	}

	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: autoConfigName, Namespace: ns},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: gw.Name},
			OpenAPI: v1alpha1.OpenAPISource{
				ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: specConfigMapName, Key: specConfigMapKey},
			},
			// The embedded CUE definitions default the backend host to
			// http://localhost when the spec has no URL to derive it from.
			URLTransform: &v1alpha1.URLTransformSpec{
				HostMapping: []v1alpha1.HostMappingEntry{
					{From: "http://localhost", To: "http://pets.default.svc.cluster.local:8080"},
				},
			},
			Trigger: v1alpha1.TriggerOnChange,
		},
	}
	if err := k8sClient.Create(ctx, ac); err != nil {
		t.Fatalf("create autoconfig: %v", err)
	}

	eventually(t, func() error {
		var cur v1alpha1.KrakenDAutoConfig
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ac), &cur); err != nil {
			return err
		}
		if cur.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
			return fmt.Errorf("expected phase Synced, got %q", cur.Status.Phase)
		}
		ready := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue || cur.Status.ObservedGeneration != cur.Generation {
			return fmt.Errorf("expected Ready=True at generation %d, got %+v (observedGeneration %d)",
				cur.Generation, ready, cur.Status.ObservedGeneration)
		}
		if cur.Status.GeneratedEndpoints != len(initialEndpointNames) {
			return fmt.Errorf("expected %d generated endpoints, got %d",
				len(initialEndpointNames), cur.Status.GeneratedEndpoints)
		}
		for _, name := range initialEndpointNames {
			var ep v1alpha1.KrakenDEndpoint
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &ep); err != nil {
				return fmt.Errorf("waiting for endpoint %s: %w", name, err)
			}
			if ep.Status.Phase != v1alpha1.EndpointPhaseActive {
				return fmt.Errorf("expected endpoint %s Active, got %q", name, ep.Status.Phase)
			}
		}
		return nil
	})
	return ac
}

// getOwnedEndpoint fetches the named endpoint and checks the AutoConfig controls it.
func getOwnedEndpoint(ac *v1alpha1.KrakenDAutoConfig, name string) (*v1alpha1.KrakenDEndpoint, error) {
	var ep v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ac.Namespace}, &ep); err != nil {
		return nil, fmt.Errorf("getting endpoint %s: %w", name, err)
	}
	if !metav1.IsControlledBy(&ep, ac) {
		return nil, fmt.Errorf("endpoint %s is not controlled by autoconfig %s", name, ac.Name)
	}
	return &ep, nil
}

// forceReconcile makes the AutoConfig reconcile with unchanged inputs: an
// annotation change passes the AutoConfig watch predicate.
func forceReconcile(t *testing.T, ac *v1alpha1.KrakenDAutoConfig) {
	t.Helper()
	var cur v1alpha1.KrakenDAutoConfig
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ac), &cur); err != nil {
		t.Fatal(err)
	}
	forced := cur.DeepCopy()
	if forced.Annotations == nil {
		forced.Annotations = map[string]string{}
	}
	forced.Annotations["krakend.io/resync"] = strconv.FormatInt(time.Now().Unix(), 10)
	if err := k8sClient.Patch(ctx, forced, client.MergeFrom(cur.DeepCopy())); err != nil {
		t.Fatalf("annotate autoconfig: %v", err)
	}
}

// endpointVersion is the metadata that records writes to a generated endpoint:
// generation moves on spec changes, resourceVersion on any persisted change.
type endpointVersion struct {
	generation      int64
	resourceVersion string
}

// generatedEndpointVersions returns the version of every endpoint the
// AutoConfig generated, keyed by name.
func generatedEndpointVersions(ac *v1alpha1.KrakenDAutoConfig) (map[string]endpointVersion, error) {
	var list v1alpha1.KrakenDEndpointList
	if err := k8sClient.List(ctx, &list,
		client.InNamespace(ac.Namespace),
		client.MatchingLabels{"gateway.krakend.io/autoconfig": ac.Name},
	); err != nil {
		return nil, fmt.Errorf("listing generated endpoints: %w", err)
	}
	versions := make(map[string]endpointVersion, len(list.Items))
	for i := range list.Items {
		ep := &list.Items[i]
		versions[ep.Name] = endpointVersion{generation: ep.Generation, resourceVersion: ep.ResourceVersion}
	}
	return versions, nil
}

// endpointsGeneratedEvents returns the count of every EndpointsGenerated event
// recorded for the AutoConfig, keyed by event name. The recorder folds a
// repeated identical event into the existing one by bumping its count, so a
// new such event shows up either as a new name or as a higher count.
func endpointsGeneratedEvents(ac *v1alpha1.KrakenDAutoConfig) (map[string]int32, error) {
	var list corev1.EventList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ac.Namespace)); err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	counts := map[string]int32{}
	for i := range list.Items {
		ev := &list.Items[i]
		if ev.InvolvedObject.UID == ac.UID && ev.Reason == v1alpha1.ReasonEndpointsGenerated {
			counts[ev.Name] = ev.Count
		}
	}
	return counts, nil
}

func TestAutoConfig_SteadyStateDoesNotChurnStatus(t *testing.T) {
	ac := newSyncedAutoConfig(t)

	var synced v1alpha1.KrakenDAutoConfig
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ac), &synced); err != nil {
		t.Fatal(err)
	}

	// A synced AutoConfig whose inputs do not change must not be written
	// again: a reconcile that re-enqueues itself through its own status
	// writes bumps the resourceVersion several times per second.
	consistently(t, 10*time.Second, func() error {
		var cur v1alpha1.KrakenDAutoConfig
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ac), &cur); err != nil {
			return err
		}
		if cur.ResourceVersion != synced.ResourceVersion {
			return fmt.Errorf("autoconfig resourceVersion changed from %s to %s (phase %q)",
				synced.ResourceVersion, cur.ResourceVersion, cur.Status.Phase)
		}
		return nil
	})
}

func TestAutoConfig_RecreatesDeletedEndpoint(t *testing.T) {
	ac := newSyncedAutoConfig(t)

	deleted, err := getOwnedEndpoint(ac, initialEndpointNames[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Delete(ctx, deleted); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}

	eventuallyWithin(t, 30*time.Second, func() error {
		ep, err := getOwnedEndpoint(ac, deleted.Name)
		if err != nil {
			return err
		}
		if ep.UID == deleted.UID {
			return fmt.Errorf("endpoint %s not yet deleted", deleted.Name)
		}
		return nil
	})
}

func TestAutoConfig_SpecConfigMapChangePropagates(t *testing.T) {
	ac := newSyncedAutoConfig(t)

	var cm corev1.ConfigMap
	cmKey := types.NamespacedName{Name: specConfigMapName, Namespace: ac.Namespace}
	if err := k8sClient.Get(ctx, cmKey, &cm); err != nil {
		t.Fatal(err)
	}
	operations := maps.Clone(initialOperations)
	operations["/toys"] = "listToys"
	cm.Data[specConfigMapKey] = openAPISpec(t, operations)
	if err := k8sClient.Update(ctx, &cm); err != nil {
		t.Fatalf("update spec configmap: %v", err)
	}

	eventuallyWithin(t, 30*time.Second, func() error {
		_, err := getOwnedEndpoint(ac, "pets-listtoys")
		return err
	})
}

func TestAutoConfig_ForcedReconcileInSteadyStateWritesNothing(t *testing.T) {
	ac := newSyncedAutoConfig(t)

	// Events are written asynchronously; wait for the initial sync's
	// EndpointsGenerated event so it is part of the baseline.
	var eventsBefore map[string]int32
	eventually(t, func() error {
		var err error
		if eventsBefore, err = endpointsGeneratedEvents(ac); err != nil {
			return err
		}
		if len(eventsBefore) == 0 {
			return fmt.Errorf("waiting for the initial EndpointsGenerated event")
		}
		return nil
	})

	var before v1alpha1.KrakenDAutoConfig
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ac), &before); err != nil {
		t.Fatal(err)
	}
	endpointsBefore, err := generatedEndpointVersions(ac)
	if err != nil {
		t.Fatal(err)
	}
	if len(endpointsBefore) != len(initialEndpointNames) {
		t.Fatalf("expected %d generated endpoints, got %v", len(initialEndpointNames), endpointsBefore)
	}

	// An annotation change passes the AutoConfig watch predicate, so this
	// forces a reconcile with unchanged inputs.
	forceReconcile(t, ac)

	// The forced reconcile must change nothing, so there is nothing to wait
	// for; keep checking while it runs. That also means the test cannot
	// observe whether the forced reconcile ran at all. It does discriminate:
	// with the semantic endpoint comparison removed (the desired spec always
	// assigned in reconcileEndpoints), it fails on a spurious
	// EndpointsGenerated event reporting updated endpoints.
	consistently(t, 5*time.Second, func() error {
		var cur v1alpha1.KrakenDAutoConfig
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ac), &cur); err != nil {
			return err
		}
		if !cur.Status.LastSyncTime.Equal(before.Status.LastSyncTime) {
			return fmt.Errorf("lastSyncTime changed from %v to %v", before.Status.LastSyncTime, cur.Status.LastSyncTime)
		}
		if !equality.Semantic.DeepEqual(cur.Status, before.Status) {
			return fmt.Errorf("status changed from %+v to %+v", before.Status, cur.Status)
		}
		endpoints, err := generatedEndpointVersions(ac)
		if err != nil {
			return err
		}
		if !maps.Equal(endpoints, endpointsBefore) {
			return fmt.Errorf("generated endpoints changed from %+v to %+v", endpointsBefore, endpoints)
		}
		events, err := endpointsGeneratedEvents(ac)
		if err != nil {
			return err
		}
		if !maps.Equal(events, eventsBefore) {
			return fmt.Errorf("EndpointsGenerated events changed from %v to %v", eventsBefore, events)
		}
		return nil
	})
}

func TestAutoConfig_RestoresStrippedLabelPromptly(t *testing.T) {
	ac := newSyncedAutoConfig(t)
	ep, err := getOwnedEndpoint(ac, initialEndpointNames[0])
	if err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(ep.DeepCopy())
	delete(ep.Labels, "gateway.krakend.io/autoconfig")
	if err := k8sClient.Patch(ctx, ep, patch); err != nil {
		t.Fatalf("strip label: %v", err)
	}

	// A label change bumps no generation: only the Owns predicate's label
	// clause reconciles the AutoConfig before the 5-minute resync.
	eventuallyWithin(t, 30*time.Second, func() error {
		cur, err := getOwnedEndpoint(ac, ep.Name)
		if err != nil {
			return err
		}
		if cur.Labels["gateway.krakend.io/autoconfig"] != ac.Name {
			return fmt.Errorf("label not restored yet: %v", cur.Labels)
		}
		return nil
	})
}

func TestAutoConfig_AdoptsAndRemovesALabelledOrphan(t *testing.T) {
	ac := newSyncedAutoConfig(t)
	// An endpoint recreated from an old generated manifest: both managed
	// labels, no controller, and no longer generated.
	template, err := getOwnedEndpoint(ac, initialEndpointNames[0])
	if err != nil {
		t.Fatal(err)
	}
	orphan := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pets-gone",
			Namespace: ac.Namespace,
			Labels:    maps.Clone(template.Labels),
		},
		Spec: *template.Spec.DeepCopy(),
	}
	orphan.Spec.Endpoints[0].Endpoint = "/gone"
	if err := k8sClient.Create(ctx, orphan); err != nil {
		t.Fatalf("create orphan: %v", err)
	}

	// Nothing watches an endpoint without a controller, so force a reconcile
	// the way the steady-state test does. Admission does not block the
	// adoption write.
	forceReconcile(t, ac)

	eventuallyWithin(t, 30*time.Second, func() error {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(orphan), &v1alpha1.KrakenDEndpoint{})
		if err == nil {
			return fmt.Errorf("orphan %s not yet removed", orphan.Name)
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
}
