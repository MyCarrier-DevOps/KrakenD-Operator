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

package webhook

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func testGateway() *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: v1alpha1.EditionCE},
	}
}

func testEndpoint(name string, paths ...string) *v1alpha1.KrakenDEndpoint {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	for _, p := range paths {
		ep.Spec.Endpoints = append(ep.Spec.Endpoints, v1alpha1.EndpointEntry{
			Endpoint: p, Method: "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}},
		})
	}
	return ep
}

// An entry stored before the audience rule existed must not block edits to
// other entries, even when the update reorders the list.
func TestEndpointAdmission_RatchetsUnchangedEntriesAcrossReorder(t *testing.T) {
	old := testEndpoint("e", "/bad", "/good")
	old.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"documentation/openapi":{"audience":{"a":1}}}`),
	}
	edited := old.DeepCopy()
	edited.Spec.Endpoints[0], edited.Spec.Endpoints[1] = edited.Spec.Endpoints[1], edited.Spec.Endpoints[0]
	edited.Spec.Endpoints[0].Backends[0].URLPattern = "/v2" // /good changes, /bad only moves
	v := &EndpointValidator{Client: fakeClient(testGateway())}

	if resp := review(t, v, "alice", edited, old); !resp.Allowed {
		t.Fatalf("reorder plus unrelated edit denied: %+v", resp.Result)
	}

	touched := edited.DeepCopy()
	touched.Spec.Endpoints[1].Backends[0].URLPattern = "/v3" // now /bad itself changes
	resp := review(t, v, "alice", touched, old)
	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(resp.Result.Details.Causes[0].Field, "spec.endpoints[1].extraConfig") {
		t.Errorf("edit of the violating entry: %+v, want 422 on spec.endpoints[1].extraConfig", resp.Result)
	}
}

func TestEndpointAdmission_UnchangedReferencesAreNotRechecked(t *testing.T) {
	old := testEndpoint("e", "/a")
	old.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "gone"}
	edited := old.DeepCopy()
	edited.Spec.Endpoints = append(edited.Spec.Endpoints, testEndpoint("x", "/b").Spec.Endpoints...)
	v := &EndpointValidator{Client: fakeClient()} // neither the gateway nor the policy exists any more

	if resp := review(t, v, "alice", edited, old); !resp.Allowed {
		t.Errorf("edit with unchanged dangling refs denied: %+v", resp.Result)
	}

	newRef := edited.DeepCopy()
	newRef.Spec.Endpoints[1].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "missing"}
	if resp := review(t, v, "alice", newRef, old); resp.Allowed {
		t.Error("new reference to a missing policy admitted")
	}
	moved := edited.DeepCopy()
	moved.Spec.GatewayRef.Name = "other"
	if resp := review(t, v, "alice", moved, old); resp.Allowed {
		t.Error("gatewayRef changed to a missing gateway admitted")
	}
}

// Moving an endpoint to another gateway puts every stored entry in front of
// that gateway's rules, so an unchanged entry is judged again.
func TestEndpointAdmission_MovingToAnotherGatewayRechecksEveryEntry(t *testing.T) {
	old := testEndpoint("e", "/bad")
	old.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"documentation/openapi":{"audience":{"a":1}}}`),
	}
	other := testGateway()
	other.Name = "other"
	v := &EndpointValidator{Client: fakeClient(testGateway(), other)}

	moved := old.DeepCopy()
	moved.Spec.GatewayRef.Name = "other"
	resp := review(t, v, "alice", moved, old)
	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(resp.Result.Details.Causes[0].Field, "spec.endpoints[0].extraConfig") {
		t.Errorf("unchanged entry moved to another gateway: %+v, want 422 on spec.endpoints[0].extraConfig", resp.Result)
	}

	withNewEntry := old.DeepCopy()
	withNewEntry.Spec.Endpoints = append(withNewEntry.Spec.Endpoints, testEndpoint("x", "/ok").Spec.Endpoints...)
	if resp := review(t, v, "alice", withNewEntry, old); !resp.Allowed {
		t.Errorf("unrelated edit on the same gateway denied: %+v", resp.Result)
	}
}

func TestEndpointAdmission_MovingToAnotherNamespaceRechecksEveryEntry(t *testing.T) {
	old := testEndpoint("e", "/bad")
	old.Spec.Endpoints[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"documentation/openapi":{"audience":{"a":1}}}`),
	}
	elsewhere := testGateway()
	elsewhere.Namespace = "edge"
	v := &EndpointValidator{Client: fakeClient(testGateway(), elsewhere)}

	moved := old.DeepCopy()
	moved.Spec.GatewayRef.Namespace = "edge"
	resp := review(t, v, "alice", moved, old)
	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(resp.Result.Details.Causes[0].Field, "spec.endpoints[0].extraConfig") {
		t.Errorf("entry moved to a gateway of the same name in another namespace: %+v, want 422", resp.Result)
	}
}

func TestEndpointAdmission_RejectsRouteClaimedByAnotherEndpoint(t *testing.T) {
	tests := []struct {
		name, existing, candidate, detail string
	}{
		{"exact duplicate", "/users/{id}", "/users/{id}", "already defined by KrakenDEndpoint default/other"},
		{"same shape", "/users/{id}", "/users/{name}", "has the same route as GET /users/{id}"},
		{"collapsed slashes", "/a/b", "/a//b", "has the same route as GET /a/b in KrakenDEndpoint default/other: " +
			"paths that differ only in parameter names or repeated slashes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &EndpointValidator{Client: fakeClient(testGateway(), testEndpoint("other", tt.existing))}
			resp := review(t, v, "alice", testEndpoint("new", "/ok", tt.candidate), nil)
			if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
				t.Fatalf("response = %+v, want 422", resp.Result)
			}
			c := resp.Result.Details.Causes
			if len(c) != 1 || c[0].Field != "spec.endpoints[1]" || c[0].Type != metav1.CauseTypeFieldValueDuplicate ||
				!strings.Contains(c[0].Message, tt.detail) {
				t.Errorf("causes = %+v, want a Duplicate on spec.endpoints[1] with %q", c, tt.detail)
			}
		})
	}
}

func TestEndpointAdmission_RouteClashInsideOneEndpoint(t *testing.T) {
	v := &EndpointValidator{Client: fakeClient(testGateway())}
	resp := review(t, v, "alice", testEndpoint("new", "/a/{id}", "/a/{name}"), nil)
	if resp.Allowed || len(resp.Result.Details.Causes) != 2 {
		t.Errorf("response = %+v, want both entries rejected", resp.Result)
	}
}

func TestEndpointAdmission_StoredClashDoesNotBlockOtherEdits(t *testing.T) {
	old := testEndpoint("new", "/users/{name}", "/b")
	edited := old.DeepCopy()
	edited.Spec.Endpoints[1].Backends[0].URLPattern = "/v2"
	v := &EndpointValidator{Client: fakeClient(testGateway(), testEndpoint("other", "/users/{id}"), old)}
	if resp := review(t, v, "alice", edited, old); !resp.Allowed {
		t.Errorf("edit of an unrelated entry denied: %+v", resp.Result)
	}
}

// The same-controller exemption: while an AutoConfig renames an operation, its
// new endpoint and the old one on the same route exist together, and the
// admission cache may lag the deletion of the old one.
func TestEndpointAdmission_SameControllerMayShareARoute(t *testing.T) {
	owned := func(name string, uid types.UID) *v1alpha1.KrakenDEndpoint {
		ep := testEndpoint(name, "/users/{id}")
		ep.OwnerReferences = []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(),
			Kind: "KrakenDAutoConfig", Name: "pets", UID: uid, Controller: ptr.To(true)}}
		return ep
	}
	v := &EndpointValidator{Client: fakeClient(testGateway(), owned("pets-getuser", "pets-uid"))}

	if resp := review(t, v, "alice", owned("pets-getuserbyid", "pets-uid"), nil); !resp.Allowed {
		t.Errorf("same-controller route denied: %+v", resp.Result)
	}
	if resp := review(t, v, "alice", owned("other-getuser", "other-uid"), nil); resp.Allowed {
		t.Error("another controller's duplicate route admitted")
	}
}

func TestEndpointAdmission_DuplicateAcrossNamespacesOnOneGateway(t *testing.T) {
	other := testEndpoint("other", "/a")
	other.Namespace = "team-b"
	other.Spec.GatewayRef.Namespace = "default"
	v := &EndpointValidator{Client: fakeClient(testGateway(), other)}
	resp := review(t, v, "alice", testEndpoint("new", "/a"), nil)
	if resp.Allowed || resp.Result.Details == nil || len(resp.Result.Details.Causes) != 1 ||
		!strings.Contains(resp.Result.Details.Causes[0].Message, "team-b/other") {
		t.Errorf("response = %+v, want a duplicate naming team-b/other", resp.Result)
	}
}

func TestEndpointAdmission_SameRouteOnAnotherGatewayIsAdmitted(t *testing.T) {
	other := testEndpoint("other", "/a")
	other.Spec.GatewayRef.Name = "elsewhere"
	elsewhere := testGateway()
	elsewhere.Name = "elsewhere"
	v := &EndpointValidator{Client: fakeClient(testGateway(), elsewhere, other)}
	if resp := review(t, v, "alice", testEndpoint("new", "/a"), nil); !resp.Allowed {
		t.Errorf("route of another gateway denied: %+v", resp.Result)
	}
}

// Two endpoints claim the route: the denial names the one the renderer serves,
// the oldest, whatever order the informer lists them in.
func TestEndpointAdmission_NamesTheEndpointThatServesTheRoute(t *testing.T) {
	served := testEndpoint("z-served", "/a/{id}")
	served.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	lost := testEndpoint("a-lost", "/a/{name}")
	lost.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	for name, reverse := range map[string]bool{"listed in order": false, "listed reversed": true} {
		t.Run(name, func(t *testing.T) {
			funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList,
				opts ...client.ListOption) error {
				if err := c.List(ctx, l, opts...); err != nil {
					return err
				}
				if eps, ok := l.(*v1alpha1.KrakenDEndpointList); ok && reverse {
					slices.Reverse(eps.Items)
				}
				return nil
			}}
			v := &EndpointValidator{Client: fakeClientBuilderWith(funcs, testGateway(), served, lost)}
			resp := review(t, v, "alice", testEndpoint("new", "/a/{id}"), nil)
			c := resp.Result.Details.Causes
			const want = "already defined by KrakenDEndpoint default/z-served"
			if resp.Allowed || len(c) != 1 || !strings.Contains(c[0].Message, want) {
				t.Errorf("response = %+v, want a duplicate naming default/z-served", resp.Result)
			}
		})
	}
}

// A newer endpoint stored with the same route as an older one is the loser.
// Editing the served entry's body is not a new claim on the route, and the
// owner of the served entry must not be blocked by its loser.
func TestEndpointAdmission_ServedEntryCanBeEditedBesideItsLoser(t *testing.T) {
	served := testEndpoint("served", "/a/{id}")
	served.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	loser := testEndpoint("loser", "/a/{name}", "/c")
	loser.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	edited := served.DeepCopy()
	edited.Spec.Endpoints[0].Backends[0].URLPattern = "/v2"
	v := &EndpointValidator{Client: fakeClient(testGateway(), served, loser)}

	if resp := review(t, v, "alice", edited, served); !resp.Allowed {
		t.Errorf("edit of the served entry denied: %+v", resp.Result)
	}

	claiming := served.DeepCopy()
	claiming.Spec.Endpoints = append(claiming.Spec.Endpoints, testEndpoint("x", "/c").Spec.Endpoints...)
	if resp := review(t, v, "alice", claiming, served); resp.Allowed {
		t.Error("a new entry on a route another endpoint holds admitted")
	}
}

// Moving an endpoint puts every stored entry in front of the new gateway's
// routes, so an unchanged entry whose route is taken there is a duplicate.
func TestEndpointAdmission_MovingOntoATakenRouteIsADuplicate(t *testing.T) {
	old := testEndpoint("e", "/a")
	other := testGateway()
	other.Name = "other"
	holder := testEndpoint("holder", "/a")
	holder.Spec.GatewayRef.Name = "other"
	v := &EndpointValidator{Client: fakeClient(testGateway(), other, old, holder)}

	moved := old.DeepCopy()
	moved.Spec.GatewayRef.Name = "other"
	resp := review(t, v, "alice", moved, old)
	c := resp.Result.Details
	if resp.Allowed || c == nil || len(c.Causes) != 1 || c.Causes[0].Type != metav1.CauseTypeFieldValueDuplicate {
		t.Errorf("response = %+v, want a Duplicate on the unchanged entry", resp.Result)
	}
}

// Same-shape entries inside one KrakenDEndpoint are checked whenever an entry
// changes, even when the new entry has the route key of a stored one.
func TestEndpointAdmission_UpdateAddingAnEntryWithAStoredRouteKeyIsRejected(t *testing.T) {
	tests := []struct{ name, stored, added string }{
		{"parameter name", "/users/{id}", "/users/{name}"},
		{"repeated slashes", "/a/b", "/a//b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := testEndpoint("e", tt.stored)
			edited := testEndpoint("e", tt.stored, tt.added)
			v := &EndpointValidator{Client: fakeClient(testGateway(), old)}
			resp := review(t, v, "alice", edited, old)
			c := resp.Result.Details
			if resp.Allowed || c == nil || len(c.Causes) == 0 || c.Causes[0].Type != metav1.CauseTypeFieldValueDuplicate {
				t.Errorf("response = %+v, want a Duplicate on the added entry", resp.Result)
			}
		})
	}
}
