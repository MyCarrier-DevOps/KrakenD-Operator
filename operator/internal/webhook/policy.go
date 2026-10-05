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
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// PolicyValidator validates KrakenDBackendPolicy resources.
type PolicyValidator struct {
	client.Client
	// Checker renders the policy alone and in every gateway that uses it.
	Checker ConfigChecker
}

// ValidateCreate validates a new KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	policy, ok := obj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", obj)
	}
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()
	return checkPolicyRender(ctx, v.Client, v.Checker, nil, policy)
}

// ValidateUpdate validates an updated KrakenDBackendPolicy. An update that
// leaves the spec alone, such as the protection finalizer, is never validated.
func (v *PolicyValidator) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	if terminatingWithUnchangedSpec(oldObj, newObj) {
		return nil, nil
	}
	policy, ok := newObj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", newObj)
	}
	old, ok := oldObj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", oldObj)
	}
	if equality.Semantic.DeepEqual(old.Spec, policy.Spec) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()
	return checkPolicyRender(ctx, v.Client, v.Checker, old, policy)
}

// ValidateDelete is required by admission.CustomValidator. The policy webhook is
// not registered for DELETE: the policy-protection finalizer keeps a referenced
// policy until nothing references it.
func (v *PolicyValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
