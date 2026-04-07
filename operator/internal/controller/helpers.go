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
// content, ignoring LastTransitionTime which is updated on every
// meta.SetStatusCondition call.
func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type ||
			a[i].Status != b[i].Status ||
			a[i].Reason != b[i].Reason ||
			a[i].Message != b[i].Message ||
			a[i].ObservedGeneration != b[i].ObservedGeneration {
			return false
		}
	}
	return true
}

var (
	endpointIndexOnce sync.Once
	endpointIndexErr  error
)

// ensureEndpointIndexes registers field indexes for KrakenDEndpoint lookups.
// It is safe to call from multiple controllers; the indexes are registered
// exactly once via sync.Once.
func ensureEndpointIndexes(mgr ctrl.Manager) error {
	endpointIndexOnce.Do(func() {
		indexer := mgr.GetFieldIndexer()

		if err := indexer.IndexField(
			context.Background(), &v1alpha1.KrakenDEndpoint{}, endpointGatewayIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				return []string{ep.Spec.GatewayRef.Name}
			},
		); err != nil {
			endpointIndexErr = fmt.Errorf("indexing %s: %w", endpointGatewayIndex, err)
			return
		}

		if err := indexer.IndexField(
			context.Background(), &v1alpha1.KrakenDEndpoint{}, endpointPolicyIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				var refs []string
				for _, entry := range ep.Spec.Endpoints {
					for _, be := range entry.Backends {
						if be.PolicyRef != nil {
							refs = append(refs, be.PolicyRef.Name)
						}
					}
				}
				return refs
			},
		); err != nil {
			endpointIndexErr = fmt.Errorf("indexing %s: %w", endpointPolicyIndex, err)
			return
		}
	})
	return endpointIndexErr
}
