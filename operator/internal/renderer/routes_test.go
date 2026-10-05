package renderer

import (
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
