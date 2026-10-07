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

	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// AutoConfigValidator validates KrakenDAutoConfig resources.
type AutoConfigValidator struct {
	client.Client
	// Tracer records the rules as a structural span; nil records none.
	Tracer trace.Tracer
}

// ValidateCreate validates a new KrakenDAutoConfig.
func (v *AutoConfigValidator) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (_ admission.Warnings, retErr error) {
	ac, ok := obj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", obj)
	}
	ctx, span := tracing.Start(ctx, v.Tracer, "admission.structural")
	defer func() { tracing.End(span, retErr) }()
	errs, err := v.validateGatewayRef(ctx, ac)
	if err != nil {
		return nil, unavailable(err)
	}
	errs = append(errs, validateFields(ac)...)
	warnings, err := v.policyRefWarnings(ctx, ac)
	if err != nil {
		return nil, unavailable(err)
	}
	return warnings, invalid("KrakenDAutoConfig", ac.Name, errs)
}

// ValidateUpdate validates an updated KrakenDAutoConfig. The gateway
// reference is checked only when it changes, and a field rule rejects the
// update only for errors the stored object did not already have.
func (v *AutoConfigValidator) ValidateUpdate(
	ctx context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (_ admission.Warnings, retErr error) {
	if terminatingWithUnchangedSpec(oldObj, newObj) {
		return nil, nil
	}
	ac, ok := newObj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", newObj)
	}
	old, ok := oldObj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", oldObj)
	}
	if equality.Semantic.DeepEqual(old.Spec, ac.Spec) {
		return nil, nil
	}
	ctx, span := tracing.Start(ctx, v.Tracer, "admission.structural")
	defer func() { tracing.End(span, retErr) }()
	var errs field.ErrorList
	if old.Spec.GatewayRef != ac.Spec.GatewayRef {
		refErrs, err := v.validateGatewayRef(ctx, ac)
		if err != nil {
			return nil, unavailable(err)
		}
		errs = refErrs
	}
	errs = append(errs, newErrors(validateFields(ac), validateFields(old))...)
	warnings, err := v.policyRefWarnings(ctx, ac)
	if err != nil {
		return nil, unavailable(err)
	}
	return warnings, invalid("KrakenDAutoConfig", ac.Name, errs)
}

// ValidateDelete is a no-op for autoconfigs.
func (v *AutoConfigValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

// policyRefWarnings warns about each policyRef that names no existing policy.
// They are warnings, not errors: a release may create the policy after the
// AutoConfig, and the generated endpoints are rejected until it exists.
func (v *AutoConfigValidator) policyRefWarnings(
	ctx context.Context, ac *v1alpha1.KrakenDAutoConfig,
) (admission.Warnings, error) {
	type ref struct {
		path string
		ref  *v1alpha1.PolicyRef
	}
	var refs []ref
	if ac.Spec.Defaults != nil && ac.Spec.Defaults.PolicyRef != nil {
		refs = append(refs, ref{"spec.defaults.policyRef", ac.Spec.Defaults.PolicyRef})
	}
	for i, ov := range ac.Spec.Overrides {
		if ov.PolicyRef != nil {
			refs = append(refs, ref{fmt.Sprintf("spec.overrides[%d].policyRef", i), ov.PolicyRef})
		}
	}
	for i, ae := range ac.Spec.AdditionalEndpoints {
		for j, be := range ae.Backends {
			if be.PolicyRef != nil {
				refs = append(refs,
					ref{fmt.Sprintf("spec.additionalEndpoints[%d].backends[%d].policyRef", i, j), be.PolicyRef})
			}
		}
	}
	var warnings admission.Warnings
	unwarned := 0
	for _, r := range refs {
		key := types.NamespacedName{Namespace: r.ref.ResolvedNamespace(ac.Namespace), Name: r.ref.Name}
		err := v.Get(ctx, key, &v1alpha1.KrakenDBackendPolicy{})
		switch {
		case apierrors.IsNotFound(err) && len(warnings) < maxPolicyWarnings:
			warnings = append(warnings, fmt.Sprintf("%s: KrakenDBackendPolicy %s not found; "+
				"generated endpoints that use it are rejected until it exists", r.path, key))
		case apierrors.IsNotFound(err):
			unwarned++
		case err != nil:
			return nil, fmt.Errorf("looking up policy %s: %w", key, err)
		}
	}
	if unwarned > 0 {
		warnings = append(warnings, fmt.Sprintf("%d more policyRefs name no existing KrakenDBackendPolicy", unwarned))
	}
	return warnings, nil
}

// validateGatewayRef checks that the gateway ac references exists.
func (v *AutoConfigValidator) validateGatewayRef(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
) (field.ErrorList, error) {
	var errs field.ErrorList

	gw := &v1alpha1.KrakenDGateway{}
	gwNS := ac.Spec.GatewayRef.ResolvedNamespace(ac.Namespace)
	if err := v.Get(ctx, types.NamespacedName{
		Name:      ac.Spec.GatewayRef.Name,
		Namespace: gwNS,
	}, gw); err != nil {
		if apierrors.IsNotFound(err) {
			refPath := field.NewPath("spec", "gatewayRef", "name")
			refValue := ac.Spec.GatewayRef.Name
			if gwNS != ac.Namespace {
				refPath = field.NewPath("spec", "gatewayRef", "namespace")
				refValue = gwNS
			}
			errs = append(errs, field.NotFound(refPath, refValue))
		} else {
			return nil, fmt.Errorf("looking up gateway: %w", err)
		}
	}

	return errs, nil
}

// validateFields runs the field rules for ac.
func validateFields(ac *v1alpha1.KrakenDAutoConfig) field.ErrorList {
	var errs field.ErrorList
	for i, ov := range ac.Spec.Overrides {
		errs = append(errs, validateExtraConfigAudience(
			field.NewPath("spec", "overrides").Index(i).Child("extraConfig"),
			ov.ExtraConfig,
		)...)
	}

	if ac.Spec.Defaults != nil && ac.Spec.Defaults.Endpoint != nil {
		errs = append(errs, validateExtraConfigAudience(
			field.NewPath("spec", "defaults", "endpoint", "extraConfig"),
			ac.Spec.Defaults.Endpoint.ExtraConfig,
		)...)
	}

	errs = append(errs, validateAdditionalEndpoints(ac)...)
	errs = append(errs, validateOverrideIDs(ac.Name, ac.Spec.Overrides)...)

	return errs
}

// validateOverrideIDs rejects overrides that target the same operation: an
// operationId listed twice, or two operationIds that generate one endpoint
// name (autoconfig.OperationEndpointName), which the generator cannot keep
// apart: it drops the second.
func validateOverrideIDs(acName string, overrides []v1alpha1.OperationOverride) field.ErrorList {
	var errs field.ErrorList
	first := map[string]string{}
	for i, ov := range overrides {
		p := field.NewPath("spec", "overrides").Index(i).Child("operationId")
		name := autoconfig.OperationEndpointName(acName, ov.OperationID)
		prev, seen := first[name]
		switch {
		case !seen:
			first[name] = ov.OperationID
		case prev == ov.OperationID:
			errs = append(errs, field.Duplicate(p, truncate(ov.OperationID, echoLimit)))
		default:
			errs = append(errs, field.Invalid(p, truncate(ov.OperationID, echoLimit),
				fmt.Sprintf("collides with operationId %q: both generate the endpoint %q",
					truncate(prev, echoLimit), truncate(name, echoLimit))))
		}
	}
	return errs
}

// validateAdditionalEndpoints validates the audience in each additional
// endpoint's extraConfig; the CRD enforces the rest.
func validateAdditionalEndpoints(ac *v1alpha1.KrakenDAutoConfig) field.ErrorList {
	var errs field.ErrorList
	for i, ae := range ac.Spec.AdditionalEndpoints {
		errs = append(errs, validateExtraConfigAudience(
			field.NewPath("spec", "additionalEndpoints").Index(i).Child("extraConfig"), ae.ExtraConfig)...)
	}
	return errs
}
