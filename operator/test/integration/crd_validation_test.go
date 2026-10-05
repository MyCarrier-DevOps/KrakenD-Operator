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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// expectInvalid creates obj and requires a 422 whose message contains want.
func expectInvalid(t *testing.T, obj client.Object, want string) {
	t.Helper()
	err := k8sClient.Create(ctx, obj)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), want) {
		t.Errorf("create %s: err = %v, want Invalid containing %q", obj.GetName(), err, want)
	}
}

// The real API server runs these rules, so this proves the generated CRD is
// accepted and enforced. The test cluster runs Kubernetes 1.32, below the
// documented 1.33 floor; the rules used here do not depend on ratcheting, which
// (like optionalOldSelf) is beta and on by default in 1.32.
func TestCRD_EndpointRules(t *testing.T) {
	ns := testNamespace(t)
	ep := func(name string, entries ...v1alpha1.EndpointEntry) *v1alpha1.KrakenDEndpoint {
		return &v1alpha1.KrakenDEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}, Endpoints: entries},
		}
	}
	backends := []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}
	entry := func(path string) v1alpha1.EndpointEntry {
		return v1alpha1.EndpointEntry{Endpoint: path, Method: "GET", Backends: backends}
	}

	expectInvalid(t, ep("no-slash", entry("a/b")), "spec.endpoints[0].endpoint")
	// A nil list would marshal as null and fail as a missing value whatever
	// the minimum is, so send an explicitly empty one.
	expectInvalid(t, ep("empty", []v1alpha1.EndpointEntry{}...), "should have at least 1 items")
	expectInvalid(t, ep("dup", entry("/a"), entry("/a")), "Duplicate value")
	expectInvalid(t, ep("no-backends", v1alpha1.EndpointEntry{Endpoint: "/a", Method: "GET",
		Backends: []v1alpha1.BackendSpec{}}), "spec.endpoints[0].backends")
	if err := k8sClient.Create(ctx, ep("valid", entry("/a/{id}"), entry("/files/*"))); err != nil {
		t.Errorf("valid endpoint rejected: %v", err)
	}
}
