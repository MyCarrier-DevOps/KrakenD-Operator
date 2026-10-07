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
	"encoding/json"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendbackendpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=create;update,versions=v1alpha1,name=vkrakendbackendpolicy.kb.io,admissionReviewVersions=v1,timeoutSeconds=15
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendautoconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendautoconfigs,verbs=create;update,versions=v1alpha1,name=vkrakendautoconfig.kb.io,admissionReviewVersions=v1,timeoutSeconds=15

// Validators are the validating webhooks SetupWebhooks registers.
type Validators struct {
	Gateway    *GatewayValidator
	Endpoint   *EndpointValidator
	Policy     *PolicyValidator
	AutoConfig *AutoConfigValidator
}

// NewValidators builds the validators over c. apiReader reads uncached; see
// EndpointValidator.APIReader. checker is the config checker the gateway
// controller uses too, so the validators and the controller share its
// validation slots.
// operatorUsername is the username of the operator's own requests; see
// EndpointValidator.OperatorUsername.
func NewValidators(
	c client.Client, apiReader client.Reader, checker ConfigChecker, operatorUsername string,
) Validators {
	memo := newAdmissionMemo()
	return Validators{
		Gateway: &GatewayValidator{Client: c, Checker: checker},
		Endpoint: &EndpointValidator{
			Client: c, APIReader: apiReader, Checker: checker, OperatorUsername: operatorUsername, Memo: memo,
		},
		Policy:     &PolicyValidator{Client: c, Checker: checker, Memo: memo},
		AutoConfig: &AutoConfigValidator{Client: c},
	}
}

// SetupWebhooks registers all validating webhooks with the manager. validators
// come from NewValidators, built over the config checker the gateway
// controller uses too, so both share its validation slots.
func SetupWebhooks(mgr ctrl.Manager, validators Validators) error {
	// Ensure field indexes are registered — needed for the duplicate-route
	// check and the policy fan-out even when running webhook-only.
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
