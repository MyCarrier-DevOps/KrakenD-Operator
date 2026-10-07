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
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

const routerRefusal = "':id' in new path '/b/:id' conflicts with existing wildcard ':userId'"

// clashOf answers Conflicts with lost whenever the replace set holds an
// endpoint named name: the clash comes with that candidate.
func clashOf(name string, lost map[types.NamespacedName][]renderer.EntryConflict) func(
	[]v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
	return func(replace []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		if slices.ContainsFunc(replace, func(ep v1alpha1.KrakenDEndpoint) bool { return ep.Name == name }) {
			return configcheck.RouteConflicts{Lost: lost}
		}
		return configcheck.RouteConflicts{}
	}
}

func TestAutoConfigReconcile_HoldsACandidateThatWouldLoseARouterClash(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b/{id}"))
	other := types.NamespacedName{Namespace: "default", Name: "users"}
	checker := &fakeChecker{conflicts: clashOf("test-ac-getb", map[types.NamespacedName][]renderer.EntryConflict{
		{Namespace: "default", Name: "test-ac-getb"}: {{Endpoint: "/b/{id}", Method: "GET", Winner: other, Detail: routerRefusal}},
	})}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if endpointExists(t, c, "test-ac-getb") || !endpointExists(t, c, "test-ac-listusers") {
		t.Error("want getb held and listusers written")
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-getb" ||
		failed[0].Reason != v1alpha1.ReasonConfigValidationFailed ||
		!strings.Contains(failed[0].Message, "default/users") || !strings.Contains(failed[0].Message, routerRefusal) {
		t.Errorf("failedOperations = %+v, want getb held, naming default/users and the router's refusal", failed)
	}
}

func TestAutoConfigReconcile_HoldsACandidateThatWouldPushAnotherEndpointOut(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b/{id}"))
	checker := &fakeChecker{conflicts: clashOf("test-ac-getb", map[types.NamespacedName][]renderer.EntryConflict{
		{Namespace: "tenant-z", Name: "orders"}: {{Endpoint: "/b/{userId}/x", Method: "GET",
			Winner: types.NamespacedName{Namespace: "default", Name: "test-ac-getb"}, Detail: routerRefusal}},
	})}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if endpointExists(t, c, "test-ac-getb") {
		t.Error("getb was written although it would keep tenant-z/orders out of the router")
	}
	if !endpointExists(t, c, "test-ac-listusers") {
		t.Error("listusers was held although it takes part in no clash")
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-getb" ||
		failed[0].Reason != v1alpha1.ReasonConfigValidationFailed ||
		!strings.Contains(failed[0].Message, "tenant-z/orders") {
		t.Errorf("failedOperations = %+v, want only getb held, naming tenant-z/orders", failed)
	}
}

func TestAutoConfigReconcile_AStoredRouterClashHoldsNothing(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	lost := map[types.NamespacedName][]renderer.EntryConflict{
		{Namespace: "default", Name: "test-ac-listusers"}: {{Endpoint: "/api/users", Method: "GET",
			Winner: types.NamespacedName{Namespace: "default", Name: "older"}, Detail: routerRefusal}},
	}
	checker := &fakeChecker{conflicts: func([]v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		return configcheck.RouteConflicts{Lost: lost}
	}}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if failed := getAC(t, c, ac).Status.FailedOperations; len(failed) != 0 {
		t.Errorf("failedOperations = %+v, want none: the clash predates this sync", failed)
	}
}

func TestAutoConfigReconcile_HoldsEveryCandidateWhileClashResolutionIsCapped(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	// Only the render that carries the candidates is capped: the stored one,
	// asked with no replacements, is not.
	checker := &fakeChecker{conflicts: func(replace []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		return configcheck.RouteConflicts{Capped: len(replace) > 0}
	}}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if endpointExists(t, c, "test-ac-listusers") {
		t.Error("listusers was written while a new clash could not be told apart")
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) == 0 || !slices.ContainsFunc(failed, func(op v1alpha1.OperationStatus) bool {
		return op.Endpoint == "test-ac-listusers" && op.Reason == v1alpha1.ReasonConfigValidationFailed &&
			strings.Contains(op.Message, configcheck.ClashesCapped)
	}) {
		t.Errorf("failedOperations = %+v, want listusers held with the capped message", failed)
	}
}
