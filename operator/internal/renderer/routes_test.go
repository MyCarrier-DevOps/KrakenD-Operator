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
	if _, ok := conflicted[types.NamespacedName{Namespace: "ns", Name: "b"}]; !ok {
		t.Errorf("conflicted = %v, want ns/b", conflicted)
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
