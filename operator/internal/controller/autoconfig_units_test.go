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
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
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
