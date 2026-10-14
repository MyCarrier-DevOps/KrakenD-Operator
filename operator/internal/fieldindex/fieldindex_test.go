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

package fieldindex

import (
	"context"
	"slices"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestEndpointControllerKeys(t *testing.T) {
	owned := &v1alpha1.KrakenDEndpoint{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
		{UID: "other", Name: "x", Kind: "ConfigMap", APIVersion: "v1"},
		{UID: "ac-uid", Name: "ac", Kind: "KrakenDAutoConfig", APIVersion: "gateway.krakend.io/v1alpha1",
			Controller: ptr.To(true)},
	}}}
	if got := EndpointControllerKeys(owned); !slices.Equal(got, []string{"ac-uid"}) {
		t.Errorf("controlled: %v, want [ac-uid]", got)
	}
	if got := EndpointControllerKeys(&v1alpha1.KrakenDEndpoint{}); got != nil {
		t.Errorf("uncontrolled: %v, want nil", got)
	}
}

func TestEnsureEndpointIndexes_Sequential(t *testing.T) {
	resetIndexRegistry()
	defer resetIndexRegistry()

	indexer := &stubIndexer{}
	mgr := &stubManager{indexer: indexer}

	// First call registers every index (3 IndexField calls).
	if err := EnsureEndpointIndexes(mgr); err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if indexer.callCount() != 3 {
		t.Fatalf("expected 3 IndexField calls after first call, got %d", indexer.callCount())
	}

	// Second call should be a no-op (idempotent — returns from cache).
	if err := EnsureEndpointIndexes(mgr); err != nil {
		t.Fatalf("second (idempotent) call failed: %v", err)
	}
	if indexer.callCount() != 3 {
		t.Errorf("expected still 3 IndexField calls after idempotent call, got %d", indexer.callCount())
	}
}

func TestEnsureEndpointIndexes_ConcurrentCallers(t *testing.T) {
	resetIndexRegistry()
	defer resetIndexRegistry()

	indexer := &stubIndexer{}
	mgr := &stubManager{indexer: indexer}

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if err := EnsureEndpointIndexes(mgr); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent EnsureEndpointIndexes failed: %v", err)
	}

	// register should have been called exactly once
	// (3 IndexField calls) regardless of how many goroutines raced.
	if indexer.callCount() != 3 {
		t.Errorf("expected exactly 3 IndexField calls (one registration), got %d", indexer.callCount())
	}
}

func TestPolicyIndexFunc_Deduplicates(t *testing.T) {
	resetIndexRegistry()
	defer resetIndexRegistry()

	indexer := &stubIndexer{}
	mgr := &stubManager{indexer: indexer}
	if err := EnsureEndpointIndexes(mgr); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	fn := indexer.funcs[EndpointPolicy]
	if fn == nil {
		t.Fatal("policy index function not captured")
	}

	t.Run("intra-entry", func(t *testing.T) {
		// Two backends in the same entry both reference "pol1".
		ep := &v1alpha1.KrakenDEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/api", Method: "GET", Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:80"}, URLPattern: "/",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
						{Host: []string{"http://svc:80"}, URLPattern: "/alt",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					}},
				},
			},
		}

		refs := fn(ep)
		if len(refs) != 1 {
			t.Errorf("expected 1 deduped policy ref, got %d: %v", len(refs), refs)
		}
		if len(refs) > 0 && refs[0] != "default/pol1" {
			t.Errorf("expected %q, got %q", "default/pol1", refs[0])
		}
	})

	t.Run("cross-entry", func(t *testing.T) {
		// Same policy referenced in two different entries.
		// Verifies the seen map is scoped outside the outer loop.
		ep := &v1alpha1.KrakenDEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "ep2", Namespace: "default"},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/a", Method: "GET", Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:80"}, URLPattern: "/",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					}},
					{Endpoint: "/b", Method: "POST", Backends: []v1alpha1.BackendSpec{
						{Host: []string{"http://svc:80"}, URLPattern: "/",
							PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					}},
				},
			},
		}

		refs := fn(ep)
		if len(refs) != 1 {
			t.Errorf("expected 1 deduped policy ref, got %d: %v", len(refs), refs)
		}
		if len(refs) > 0 && refs[0] != "default/pol1" {
			t.Errorf("expected %q, got %q", "default/pol1", refs[0])
		}
	})
}

// resetIndexRegistry clears the index registry for test isolation.
func resetIndexRegistry() {
	registry.Range(func(key, _ any) bool {
		registry.Delete(key)
		return true
	})
}

// stubIndexer implements client.FieldIndexer to track IndexField calls
// and capture the registered functions for direct invocation in tests.
type stubIndexer struct {
	mu    sync.Mutex
	calls int
	funcs map[string]client.IndexerFunc
}

func (s *stubIndexer) IndexField(
	_ context.Context, _ client.Object, field string, fn client.IndexerFunc,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.funcs == nil {
		s.funcs = make(map[string]client.IndexerFunc)
	}
	s.funcs[field] = fn
	return nil
}

func (s *stubIndexer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stubManager implements enough of ctrl.Manager for EnsureEndpointIndexes.
type stubManager struct {
	ctrl.Manager // embed to satisfy interface; nil methods panic if called
	indexer      client.FieldIndexer
}

func (m *stubManager) GetFieldIndexer() client.FieldIndexer { return m.indexer }

func TestEndpointGatewayKeys_ResolvesNamespace(t *testing.T) {
	same := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "apps"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	other := same.DeepCopy()
	other.Spec.GatewayRef.Namespace = "edge"
	if got := EndpointGatewayKeys(same); len(got) != 1 || got[0] != "apps/gw" {
		t.Errorf("same-namespace key = %v, want [apps/gw]", got)
	}
	if got := EndpointGatewayKeys(other); len(got) != 1 || got[0] != "edge/gw" {
		t.Errorf("cross-namespace key = %v, want [edge/gw]", got)
	}
	if got := EndpointGatewayKeys(&v1alpha1.KrakenDGateway{}); got != nil {
		t.Errorf("non-endpoint key = %v, want nil", got)
	}
}
