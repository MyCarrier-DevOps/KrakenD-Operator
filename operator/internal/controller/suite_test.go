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

package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointGateway, fieldindex.EndpointGatewayKeys).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointPolicy, fieldindex.EndpointPolicyKeys)
}

func fakeRecorder() *record.FakeRecorder {
	return record.NewFakeRecorder(100)
}

// countStatusWrites returns interceptor funcs that count every status update
// and status patch of an object of type T in n, then pass it through.
func countStatusWrites[T client.Object](n *int) interceptor.Funcs {
	return interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			if _, ok := obj.(T); ok {
				*n++
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(
			ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			if _, ok := obj.(T); ok {
				*n++
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
}
