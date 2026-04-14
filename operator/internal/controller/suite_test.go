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
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestControllers(t *testing.T) {
	// Placeholder to ensure the package is testable.
	// Individual controller tests use the fake client pattern below.
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func fakeClientBuilder() *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, endpointGatewayIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				ns := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace)
				return []string{ns + "/" + ep.Spec.GatewayRef.Name}
			},
		).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, EndpointPolicyIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				var refs []string
				for _, entry := range ep.Spec.Endpoints {
					for _, be := range entry.Backends {
						if be.PolicyRef != nil {
							refs = append(refs, be.PolicyRef.PolicyKey(ep.Namespace))
						}
					}
				}
				return refs
			},
		)
}

func fakeRecorder() *record.FakeRecorder {
	return record.NewFakeRecorder(100)
}
