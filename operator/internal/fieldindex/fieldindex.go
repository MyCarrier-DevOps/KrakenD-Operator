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

// Package fieldindex defines the KrakenDEndpoint field indexes shared by the
// controllers, the admission webhooks and the config checker.
package fieldindex

import (
	"context"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

const (
	// EndpointGateway indexes KrakenDEndpoints by "namespace/name" of their gateway.
	EndpointGateway = ".spec.gatewayRef.namespacedName"

	// EndpointPolicy indexes KrakenDEndpoints by "namespace/name" of every policy they reference.
	EndpointPolicy = ".spec.endpoints.backends.policyRef.namespacedName"

	// EndpointController indexes KrakenDEndpoints by the UID of their
	// controller owner reference, so a KrakenDAutoConfig lists the endpoints
	// it controls whatever their labels say.
	EndpointController = ".metadata.ownerReferences.controller.uid"
)

// EndpointGatewayKeys returns the EndpointGateway index value of obj.
func EndpointGatewayKeys(obj client.Object) []string {
	ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil
	}
	return []string{ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace) + "/" + ep.Spec.GatewayRef.Name}
}

// EndpointPolicyKeys returns the EndpointPolicy index values of obj, each policy once.
func EndpointPolicyKeys(obj client.Object) []string {
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
}

// EndpointControllerKeys returns the EndpointController index value of obj:
// the UID of its controller owner reference, if it has one.
func EndpointControllerKeys(obj client.Object) []string {
	if ref := metav1.GetControllerOf(obj); ref != nil {
		return []string{string(ref.UID)}
	}
	return nil
}

// registration tracks one in-flight or completed index registration attempt
// for a specific field indexer.
type registration struct {
	ready chan struct{}
	err   error
}

// registry tracks which managers have had endpoint field indexes registered,
// scoped per-manager so multiple managers (e.g. in tests) each get their own
// registrations.
var registry sync.Map // map[client.FieldIndexer]*registration

// EnsureEndpointIndexes registers every endpoint field index.
// It is safe to call from multiple controllers and the webhook package sharing
// the same manager; indexes are registered exactly once per manager instance.
func EnsureEndpointIndexes(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	reg := &registration{ready: make(chan struct{})}

	actual, loaded := registry.LoadOrStore(indexer, reg)
	if loaded {
		existing, ok := actual.(*registration)
		if !ok {
			return fmt.Errorf("unexpected type in index registry")
		}
		<-existing.ready
		return existing.err
	}

	defer close(reg.ready)
	reg.err = register(indexer)
	return reg.err
}

func register(indexer client.FieldIndexer) error {
	if err := indexer.IndexField(
		context.Background(), &v1alpha1.KrakenDEndpoint{}, EndpointGateway, EndpointGatewayKeys,
	); err != nil {
		return fmt.Errorf("indexing %s: %w", EndpointGateway, err)
	}
	if err := indexer.IndexField(
		context.Background(), &v1alpha1.KrakenDEndpoint{}, EndpointPolicy, EndpointPolicyKeys,
	); err != nil {
		return fmt.Errorf("indexing %s: %w", EndpointPolicy, err)
	}
	if err := indexer.IndexField(
		context.Background(), &v1alpha1.KrakenDEndpoint{}, EndpointController, EndpointControllerKeys,
	); err != nil {
		return fmt.Errorf("indexing %s: %w", EndpointController, err)
	}
	return nil
}
