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
	"errors"
	"slices"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
)

func TestAutoConfigReconcile_PrecheckHoldsTheCandidatesThatFailOnTheirOwn(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	stale := ownedCopy(t, ac, generatedEndpoint("old", "/old"))
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b"))
	checker := &fakeChecker{
		group: func([]v1alpha1.KrakenDEndpoint) configcheck.Verdict { return configcheck.Verdict{Output: "together"} },
		endpoint: func(ep *v1alpha1.KrakenDEndpoint) configcheck.EndpointVerdict {
			if ep.Name == "test-ac-getb" {
				return configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: "'timeout' time: unknown unit"}
			}
			return configcheck.EndpointVerdict{OK: true}
		},
	}
	c := fakeClientBuilder().WithObjects(ac, cm, stale, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if endpointExists(t, c, "test-ac-getb") || !endpointExists(t, c, "test-ac-listusers") ||
		!endpointExists(t, c, "test-ac-old") {
		t.Error("want listusers written, getb held and old kept")
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Endpoint != "test-ac-getb" ||
		failed[0].Reason != v1alpha1.ReasonConfigValidationFailed ||
		failed[0].Message != "fails krakend check on its own: 'timeout' time: unknown unit" {
		t.Errorf("failedOperations = %+v, want getb held with its own output", failed)
	}
}

func TestAutoConfigReconcile_ARootThatFailsAloneHoldsNoCandidate(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	checker := &fakeChecker{rootFails: true, endpoint: func(*v1alpha1.KrakenDEndpoint) configcheck.EndpointVerdict {
		return configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: "fails only with the root"}
	}}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(checker.checks, []string{"root"}) || !endpointExists(t, c, "test-ac-listusers") {
		t.Errorf("checks = %v; want only the root checked and listusers written", checker.checks)
	}
}

func TestAutoConfigReconcile_AGroupThatPassesRunsNoEndpointCheck(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	checker := &fakeChecker{}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}

	if want := []string{"root", "group:test-ac-listusers"}; !slices.Equal(checker.checks, want) {
		t.Errorf("checks = %v, want %v: candidates that pass together are not checked one by one", checker.checks, want)
	}
}

// A group verdict says nothing about the entries its render left out: a
// candidate that lost an entry in the group's render is judged on its own
// even though the group passes, as the gateway controller judges it.
func TestAutoConfigReconcile_AMaskedCandidateThatFailsOnItsOwnIsHeld(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	g.output.Endpoints = append(g.output.Endpoints, generatedEndpoint("getB", "/b"))
	checker := &fakeChecker{
		group: func([]v1alpha1.KrakenDEndpoint) configcheck.Verdict {
			return configcheck.Verdict{OK: true, Masked: []types.NamespacedName{{Namespace: "default", Name: "test-ac-getb"}}}
		},
		endpoint: func(ep *v1alpha1.KrakenDEndpoint) configcheck.EndpointVerdict {
			if ep.Name == "test-ac-getb" {
				return configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid, Output: "its left-out entry is bad"}
			}
			return configcheck.EndpointVerdict{OK: true}
		},
	}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}

	want := []string{"root", "group:test-ac-listusers,test-ac-getb", "endpoint:test-ac-getb"}
	if !slices.Equal(checker.checks, want) {
		t.Errorf("checks = %v, want %v: only the masked candidate is judged on its own", checker.checks, want)
	}
	if endpointExists(t, c, "test-ac-getb") || !endpointExists(t, c, "test-ac-listusers") {
		t.Error("want listusers written and getb held")
	}
}

// While the gateway's render stops resolving router clashes at its cap, a new
// clash cannot be told apart, so every write is held before any krakend check.
func TestAutoConfigReconcile_ACappedRenderHoldsEveryWriteBeforeAnyCheck(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	checker := &fakeChecker{conflicts: func([]v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts {
		return configcheck.RouteConflicts{Capped: true}
	}}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}

	if len(checker.checks) != 0 || endpointExists(t, c, "test-ac-listusers") {
		t.Errorf("checks = %v; want listusers held before any check", checker.checks)
	}
	failed := getAC(t, c, ac).Status.FailedOperations
	if len(failed) != 1 || failed[0].Message != routerClashesCappedMessage {
		t.Errorf("failedOperations = %+v, want listusers held for the capped render", failed)
	}
}

func TestAutoConfigReconcile_AnUnchangedSyncThatHoldsRunsNoCheckAgain(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	bad := generatedEndpoint("getB", "/b")
	bad.Spec.Endpoints[0].Backends[0].Host = []string{"http://invalid.test"}
	g.output.Endpoints = append(g.output.Endpoints, bad)
	val := rejectsBadHosts()
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = newTestChecker(c, val)
	if _, err := reconcileAC(r, ac); err != nil {
		t.Fatal(err)
	}
	lints := val.lints

	if _, err := reconcileAC(r, getAC(t, c, ac)); err != nil {
		t.Fatal(err)
	}

	if val.lints != lints {
		t.Errorf("the unchanged sync ran %d checks again, want none", val.lints-lints)
	}
}

func TestAutoConfigReconcile_AnEndpointCheckThatCannotRunFailsTheSync(t *testing.T) {
	cm := testCUEDefinitionsCM()
	ac := syncedAutoConfig(cm)
	f, ce, fi, g := defaultMocks()
	checker := &fakeChecker{
		group:       func([]v1alpha1.KrakenDEndpoint) configcheck.Verdict { return configcheck.Verdict{Output: "x"} },
		endpointErr: errors.New("fork/exec krakend: resource temporarily unavailable"),
	}
	c := fakeClientBuilder().WithObjects(ac, cm, testGateway()).WithStatusSubresource(ac).Build()
	r := newACReconciler(c, f, ce, fi, g)
	r.Checker = checker

	if _, err := reconcileAC(r, ac); err == nil {
		t.Fatal("reconcile succeeded although an endpoint check could not run")
	}

	synced := meta.FindStatusCondition(getAC(t, c, ac).Status.Conditions, v1alpha1.ConditionSynced)
	if synced == nil || synced.Reason != v1alpha1.ReasonValidatorUnavailable || endpointExists(t, c, "test-ac-listusers") {
		t.Errorf("Synced = %+v; want ValidatorUnavailable and nothing written", synced)
	}
}
