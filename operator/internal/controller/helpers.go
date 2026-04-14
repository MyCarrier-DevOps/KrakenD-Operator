/*
Copyright 2026.

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
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// conditionsEqual returns true if two condition slices have the same semantic
// content, compared as a set keyed by Type. LastTransitionTime is ignored
// because meta.SetStatusCondition updates it on every call.
func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]metav1.Condition, len(a))
	for _, c := range a {
		index[c.Type] = c
	}
	for _, c := range b {
		prev, ok := index[c.Type]
		if !ok ||
			prev.Status != c.Status ||
			prev.Reason != c.Reason ||
			prev.Message != c.Message ||
			prev.ObservedGeneration != c.ObservedGeneration {
			return false
		}
	}
	return true
}

// endpointIndexRegistration tracks one in-flight or completed index
// registration attempt for a specific field indexer.
type endpointIndexRegistration struct {
	ready chan struct{}
	err   error
}

// indexRegistry tracks which managers have had endpoint field indexes
// registered, scoped per-manager so multiple managers (e.g. in tests)
// each get their own registrations.
var indexRegistry sync.Map // map[client.FieldIndexer]*endpointIndexRegistration

// EnsureEndpointIndexes registers field indexes for KrakenDEndpoint lookups.
// It is safe to call from multiple controllers and the webhook package sharing
// the same manager; indexes are registered exactly once per manager instance.
func EnsureEndpointIndexes(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	reg := &endpointIndexRegistration{ready: make(chan struct{})}

	actual, loaded := indexRegistry.LoadOrStore(indexer, reg)
	if loaded {
		existing, ok := actual.(*endpointIndexRegistration)
		if !ok {
			return fmt.Errorf("unexpected type in index registry")
		}
		<-existing.ready
		return existing.err
	}

	defer close(reg.ready)
	reg.err = registerEndpointIndexes(indexer)
	return reg.err
}

func registerEndpointIndexes(indexer client.FieldIndexer) error {
	if err := indexer.IndexField(
		context.Background(), &v1alpha1.KrakenDEndpoint{}, EndpointGatewayIndex,
		func(obj client.Object) []string {
			ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
			if !ok {
				return nil
			}
			ns := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace)
			return []string{ns + "/" + ep.Spec.GatewayRef.Name}
		},
	); err != nil {
		return fmt.Errorf("indexing %s: %w", EndpointGatewayIndex, err)
	}

	if err := indexer.IndexField(
		context.Background(), &v1alpha1.KrakenDEndpoint{}, EndpointPolicyIndex,
		func(obj client.Object) []string {
			ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
			if !ok {
				return nil
			}
			var refs []string
			seen := make(map[string]struct{})
			for _, entry := range ep.Spec.Endpoints {
				for _, be := range entry.Backends {
					if be.PolicyRef == nil {
						continue
					}
					key := be.PolicyRef.PolicyKey(ep.Namespace)
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					refs = append(refs, key)
				}
			}
			return refs
		},
	); err != nil {
		return fmt.Errorf("indexing %s: %w", EndpointPolicyIndex, err)
	}

	return nil
}
