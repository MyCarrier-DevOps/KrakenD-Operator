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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
		_, mapErr := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		switch {
		case mapErr == nil:
			installed = append(installed, gvk)
		case meta.IsNoMatchError(mapErr):
			missing = append(missing, gvk)
		default:
			return nil, nil, fmt.Errorf("checking whether the %s CRD is installed: %w", gvk.GroupKind(), mapErr)
		}
	}
	return installed, missing, nil
}
