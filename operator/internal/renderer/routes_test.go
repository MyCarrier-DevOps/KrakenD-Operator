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

package renderer

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestFlattenEndpoints_SameRouteShapeKeepsOldest(t *testing.T) {
	entry := func(path string) v1alpha1.EndpointEntry {
		return v1alpha1.EndpointEntry{
			Endpoint: path,
			Method:   "GET",
			Backends: []v1alpha1.BackendSpec{{URLPattern: "/u"}},
		}
	}
	eps := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
			Spec: v1alpha1.KrakenDEndpointSpec{Endpoints: []v1alpha1.EndpointEntry{entry("/users/{id}")}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns", CreationTimestamp: metav1.Now()},
			Spec:       v1alpha1.KrakenDEndpointSpec{Endpoints: []v1alpha1.EndpointEntry{entry("/users/{name}")}},
		},
	}

	flat, conflicted, _ := flattenEndpoints(eps, nil)

	if len(flat) != 1 || flat[0].Entry.Endpoint != "/users/{id}" {
		t.Fatalf("flat = %+v, want only the older /users/{id}", flat)
	}
	want := map[types.NamespacedName][]EntryConflict{
		{Namespace: "ns", Name: "b"}: {{
			Endpoint: "/users/{name}",
			Method:   "GET",
			Winner:   types.NamespacedName{Namespace: "ns", Name: "a"},
		}},
	}
	if !reflect.DeepEqual(conflicted, want) {
		t.Errorf("conflicted = %+v, want %+v", conflicted, want)
	}
}

func TestConflictKey(t *testing.T) {
	tests := map[string]string{
		"/a/{id}":       "/a/{}",
		"/a/{name}":     "/a/{}",
		"/a/{id}/b/{x}": "/a/{}/b/{}",
		"/a/b{id}":      "/a/b{id}",
		"/a/{id}.json":  "/a/{}.json",
		"/a//b":         "/a/b",
		"/a/":           "/a/",
		"/a":            "/a",
		"/*":            "/*",
		"/a/*":          "/a/*",
	}
	for in, want := range tests {
		if got := ConflictKey(in); got != want {
			t.Errorf("ConflictKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathParams(t *testing.T) {
	if got := PathParams("/a/{id}/b/{x-y}/c{z}"); !slices.Equal(got, []string{"id", "x-y"}) {
		t.Errorf("PathParams = %v, want [id x-y]", got)
	}
}

func TestFlattenEndpoints_SameShapeInOneEndpointKeepsTheEarlierEntry(t *testing.T) {
	// Above 12 entries sort.Slice stops being an insertion sort, so the larger
	// group proves the winner comes from the spec order and not the sort.
	for _, n := range []int{2, 20} {
		t.Run(fmt.Sprintf("%d entries", n), func(t *testing.T) {
			var entries []v1alpha1.EndpointEntry
			for i := range n {
				entries = append(entries, v1alpha1.EndpointEntry{
					Endpoint: fmt.Sprintf("/a/{p%d}", i),
					Method:   "GET",
					Backends: []v1alpha1.BackendSpec{{URLPattern: "/u"}},
				})
			}
			// A newer endpoint listed first leaves the sort real work to do.
			eps := []v1alpha1.KrakenDEndpoint{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", CreationTimestamp: metav1.Now()},
					Spec:       v1alpha1.KrakenDEndpointSpec{Endpoints: entries},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns",
						CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
					Spec: v1alpha1.KrakenDEndpointSpec{Endpoints: entries},
				},
			}

			flat, conflicted, _ := flattenEndpoints(eps, nil)

			if len(flat) != 1 || flat[0].Entry.Endpoint != "/a/{p0}" {
				t.Fatalf("flat = %+v, want only the first entry /a/{p0}", flat)
			}
			self := types.NamespacedName{Namespace: "ns", Name: "a"}
			if flat[0].Source != self {
				t.Errorf("winner source = %v, want ns/a", flat[0].Source)
			}
			if got := conflicted[self]; len(got) != n-1 {
				t.Fatalf("conflicted[ns/a] has %d entries, want %d", len(got), n-1)
			}
			for _, c := range conflicted[self] {
				if c.Endpoint == "/a/{p0}" || c.Method != "GET" || c.Winner != self {
					t.Errorf("conflict = %+v, want a later entry lost to ns/a", c)
				}
			}
		})
	}
}

func TestRouteClashDetail_SamePathNamesTheOwner(t *testing.T) {
	got := RouteClashDetail("GET", "/users/{id}", "/users/{id}", "KrakenDEndpoint default/pets-getuser")

	if want := "already defined by KrakenDEndpoint default/pets-getuser"; got != want {
		t.Errorf("RouteClashDetail = %q, want %q", got, want)
	}
}

func TestRouteClashDetail_SameShapeExplainsTheRouter(t *testing.T) {
	got := RouteClashDetail("GET", "/h/{b}", "/h/{a}", "KrakenDEndpoint ns/pets-a")

	want := "has the same route as GET /h/{a} in KrakenDEndpoint ns/pets-a: paths that differ only in " +
		"parameter names or repeated slashes cannot both be routed. Use the same parameter name, and " +
		"keep routes that share a parameterized prefix in one KrakenDEndpoint so they can be renamed together"
	if got != want {
		t.Errorf("RouteClashDetail = %q, want %q", got, want)
	}
}
