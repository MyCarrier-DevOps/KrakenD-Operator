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

// Package webhook implements validating admission webhooks for KrakenD CRDs.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
)

// isTerminating reports whether obj is being deleted (has a deletionTimestamp).
func isTerminating(obj runtime.Object) bool {
	o, ok := obj.(metav1.Object)
	return ok && !o.GetDeletionTimestamp().IsZero()
}

// terminatingWithUnchangedSpec reports whether an UPDATE only touches the
// metadata of an object that is being deleted (removing a finalizer, for
// example). Rejecting such an update would leave the object stuck in
// Terminating, so validators admit it. A spec change is not skipped: the
// gateway still renders terminating endpoints and policies, so it must pass
// the usual rules. If the specs cannot be compared, it reports false.
func terminatingWithUnchangedSpec(oldObj, newObj runtime.Object) bool {
	if !isTerminating(newObj) {
		return false
	}
	oldMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(oldObj)
	if err != nil {
		return false
	}
	newMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newObj)
	if err != nil {
		return false
	}
	return equality.Semantic.DeepEqual(oldMap["spec"], newMap["spec"])
}

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
	return checkPolicyRender(ctx, v.Client, v.Checker, policy)
}

// ValidateUpdate validates an updated KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateUpdate(
	ctx context.Context, _, newObj runtime.Object,
) (admission.Warnings, error) {
	policy, ok := newObj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", newObj)
	}
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()
	return checkPolicyRender(ctx, v.Client, v.Checker, policy)
}

// ValidateDelete blocks deletion if the policy is still referenced by endpoints.
func (v *PolicyValidator) ValidateDelete(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	policy, ok := obj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", obj)
	}

	var endpoints v1alpha1.KrakenDEndpointList
	indexKey := policy.Namespace + "/" + policy.Name
	if err := v.List(ctx, &endpoints,
		client.MatchingFields{fieldindex.EndpointPolicy: indexKey},
	); err != nil {
		return nil, unavailable(fmt.Errorf("listing endpoints: %w", err))
	}

	var references []string
	for _, ep := range endpoints.Items {
		references = append(references, ep.Namespace+"/"+ep.Name)
	}
	sort.Strings(references)

	if len(references) > 0 {
		return nil, invalid("KrakenDBackendPolicy", policy.Name, field.ErrorList{
			field.Forbidden(
				field.NewPath("metadata", "name"),
				fmt.Sprintf("policy is referenced by endpoints: %s",
					strings.Join(references, ", ")),
			),
		})
	}
	return nil, nil
}

// AutoConfigValidator validates KrakenDAutoConfig resources.
type AutoConfigValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDAutoConfig.
func (v *AutoConfigValidator) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	ac, ok := obj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", obj)
	}
	errs, err := v.validateGatewayRef(ctx, ac)
	if err != nil {
		return nil, unavailable(err)
	}
	errs = append(errs, validateFields(ac)...)
	return nil, invalid("KrakenDAutoConfig", ac.Name, errs)
}

// ValidateUpdate validates an updated KrakenDAutoConfig. The gateway
// reference is checked only when it changes, and a field rule rejects the
// update only for errors the stored object did not already have.
func (v *AutoConfigValidator) ValidateUpdate(
	ctx context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
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
	var errs field.ErrorList
	if old.Spec.GatewayRef != ac.Spec.GatewayRef {
		refErrs, err := v.validateGatewayRef(ctx, ac)
		if err != nil {
			return nil, unavailable(err)
		}
		errs = refErrs
	}
	errs = append(errs, newErrors(validateFields(ac), validateFields(old))...)
	return nil, invalid("KrakenDAutoConfig", ac.Name, errs)
}

// ValidateDelete is a no-op for autoconfigs.
func (v *AutoConfigValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
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

// validateExtraConfigAudience validates the shape of a
// documentation/openapi.audience value inside an ExtraConfig RawExtension.
// KrakenD's OpenAPI documentation plugin requires audience to be a list of
// strings; a malformed value (e.g. a YAML mapping coerced to a JSON object)
// passes CRD and CUE validation unchanged but fails `krakend check -t -n -c`,
// which blocks config updates for every service on the gateway. Invalid JSON
// and an absent documentation/openapi block or audience key are not this
// helper's concern.
func validateExtraConfigAudience(p *field.Path, ec *runtime.RawExtension) field.ErrorList {
	var errs field.ErrorList

	if ec == nil || len(ec.Raw) == 0 {
		return errs
	}

	var blocks map[string]json.RawMessage
	if err := json.Unmarshal(ec.Raw, &blocks); err != nil {
		return errs
	}

	openapiRaw, ok := blocks["documentation/openapi"]
	if !ok {
		return errs
	}

	var openapi map[string]json.RawMessage
	if err := json.Unmarshal(openapiRaw, &openapi); err != nil {
		return errs
	}

	audienceRaw, ok := openapi["audience"]
	if !ok {
		return errs
	}

	// Decode into pointers so JSON null — the whole value or an item —
	// is caught: KrakenD rejects both, but json.Unmarshal would turn them
	// into a nil slice and "" respectively.
	var audience []*string
	if err := json.Unmarshal(audienceRaw, &audience); err != nil || audience == nil || slices.Contains(audience, nil) {
		errs = append(errs, field.Invalid(
			p.Key(`"documentation/openapi"`).Child("audience"),
			string(audienceRaw),
			`must be a list of strings, e.g. ["internal"]`,
		))
	}

	return errs
}

// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendgateway,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendgateways,verbs=create;update,versions=v1alpha1,name=vkrakendgateway.kb.io,admissionReviewVersions=v1,timeoutSeconds=15
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendendpoint,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendendpoints,verbs=create;update,versions=v1alpha1,name=vkrakendendpoint.kb.io,admissionReviewVersions=v1,timeoutSeconds=15
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendbackendpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=create;update;delete,versions=v1alpha1,name=vkrakendbackendpolicy.kb.io,admissionReviewVersions=v1,timeoutSeconds=15
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendautoconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendautoconfigs,verbs=create;update,versions=v1alpha1,name=vkrakendautoconfig.kb.io,admissionReviewVersions=v1,timeoutSeconds=15

// Validators are the validating webhooks SetupWebhooks registers.
type Validators struct {
	Gateway    *GatewayValidator
	Endpoint   *EndpointValidator
	Policy     *PolicyValidator
	AutoConfig *AutoConfigValidator
}

// NewValidators builds the validators over c. checker is the config checker
// the gateway controller uses too, so the validators and the controller share
// its validation slots.
// operatorUsername is the username of the operator's own requests; see
// EndpointValidator.OperatorUsername.
func NewValidators(c client.Client, checker ConfigChecker, operatorUsername string) Validators {
	return Validators{
		Gateway:    &GatewayValidator{Client: c, Checker: checker},
		Endpoint:   &EndpointValidator{Client: c, Checker: checker, OperatorUsername: operatorUsername},
		Policy:     &PolicyValidator{Client: c},
		AutoConfig: &AutoConfigValidator{Client: c},
	}
}

// SetupWebhooks registers all validating webhooks with the manager. validators
// come from NewValidators, built over the config checker the gateway
// controller uses too, so both share its validation slots.
func SetupWebhooks(mgr ctrl.Manager, validators Validators) error {
	// Ensure field indexes are registered — needed for conflict detection
	// and policy-delete validation even when running webhook-only.
	if err := fieldindex.EnsureEndpointIndexes(mgr); err != nil {
		return fmt.Errorf("registering endpoint indexes: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDGateway{}).
		WithValidator(validators.Gateway).
		Complete(); err != nil {
		return fmt.Errorf("setting up gateway webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDEndpoint{}).
		WithValidator(validators.Endpoint).
		Complete(); err != nil {
		return fmt.Errorf("setting up endpoint webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDBackendPolicy{}).
		WithValidator(validators.Policy).
		Complete(); err != nil {
		return fmt.Errorf("setting up policy webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDAutoConfig{}).
		WithValidator(validators.AutoConfig).
		Complete(); err != nil {
		return fmt.Errorf("setting up autoconfig webhook: %w", err)
	}

	return nil
}
