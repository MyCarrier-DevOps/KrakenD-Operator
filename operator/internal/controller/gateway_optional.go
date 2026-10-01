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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// The third-party kinds a gateway creates when the matching feature is
// enabled. Their CRDs are optional.
var (
	dragonflyGVK      = schema.GroupVersionKind{Group: "dragonflydb.io", Version: "v1alpha1", Kind: "Dragonfly"}
	externalSecretGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	virtualServiceGVK = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "VirtualService"}

	optionalOwnedGVKs = []schema.GroupVersionKind{dragonflyGVK, externalSecretGVK, virtualServiceGVK}
)

// installedOptionalKinds splits the optional kinds into those whose CRDs the
// mapper knows now, and those it does not. It is evaluated once, at
// startup: a CRD installed later is watched only after an operator
// restart. Any lookup error other than "no such kind" is returned, so that
// the operator never starts without a watch it should have.
func installedOptionalKinds(mapper meta.RESTMapper) (installed, missing []schema.GroupVersionKind, err error) {
	for _, gvk := range optionalOwnedGVKs {
		ok, err := kindInstalled(mapper, gvk)
		if err != nil {
			return nil, nil, fmt.Errorf("checking whether the %s CRD is installed: %w", gvk.GroupKind(), err)
		}
		if ok {
			installed = append(installed, gvk)
		} else {
			missing = append(missing, gvk)
		}
	}
	return installed, missing, nil
}

// kindInstalled reports whether the mapper knows gvk. A kind the mapper has
// no match for is (false, nil); any other lookup error is returned.
func kindInstalled(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (bool, error) {
	_, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}

// deleteIfControlled deletes obj, looked up by its name and namespace, when
// it exists and gw controls it. An object someone else owns under the same
// name is left alone.
func (r *KrakenDGatewayReconciler) deleteIfControlled(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, obj client.Object,
) error {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, gw) {
		return nil
	}
	uid := obj.GetUID()
	err := client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid}))
	if err != nil {
		return fmt.Errorf("deleting %T %s: %w", obj, key, err)
	}
	return nil
}

// deleteOptionalIfControlled is deleteIfControlled for an optional kind.
// Without its CRD there is nothing to delete.
func (r *KrakenDGatewayReconciler) deleteOptionalIfControlled(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, gvk schema.GroupVersionKind, name string,
) error {
	available, err := r.crdAvailable(gvk)
	if err != nil || !available {
		return err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(gw.Namespace)
	return r.deleteIfControlled(ctx, gw, u)
}
