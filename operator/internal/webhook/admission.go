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

package webhook

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// invalid is the admission error for field errors: 422 Invalid with one
// status cause per error, so kubectl and API clients see each rejected field.
// It is nil when errs is empty.
func invalid(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: kind}, name, errs)
}

// unavailable is the admission error for a failed lookup or an unavailable
// validator: 500, a transient server error. Controllers and GitOps tools retry
// on their own; kubectl prints it and exits.
func unavailable(err error) error {
	return apierrors.NewInternalError(err)
}

// newErrors returns the errors in errs that old does not already have, matched
// on their full text (field, type, value and detail), so an update is rejected
// only for problems it introduces.
func newErrors(errs, old field.ErrorList) field.ErrorList {
	existing := make(map[string]struct{}, len(old))
	for _, e := range old {
		existing[e.Error()] = struct{}{}
	}
	var fresh field.ErrorList
	for _, e := range errs {
		if _, ok := existing[e.Error()]; !ok {
			fresh = append(fresh, e)
		}
	}
	return fresh
}
