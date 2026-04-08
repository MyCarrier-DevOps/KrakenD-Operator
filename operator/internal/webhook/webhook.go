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

// Package webhook implements validating admission webhooks for KrakenD CRDs.
package webhook

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// GatewayValidator validates KrakenDGateway resources.
type GatewayValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDGateway.
func (v *GatewayValidator) ValidateCreate(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	gw, ok := obj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", obj)
	}
	return nil, v.validate(gw)
}

// ValidateUpdate validates an updated KrakenDGateway.
func (v *GatewayValidator) ValidateUpdate(
	_ context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	gw, ok := newObj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", newObj)
	}
	return nil, v.validate(gw)
}

// ValidateDelete is a no-op for gateways.
func (v *GatewayValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

func (v *GatewayValidator) validate(gw *v1alpha1.KrakenDGateway) error {
	var errs field.ErrorList

	if gw.Spec.Edition == v1alpha1.EditionEE {
		if gw.Spec.License == nil ||
			(!gw.Spec.License.ExternalSecret.Enabled && gw.Spec.License.SecretRef == nil) {
			errs = append(errs, field.Required(
				field.NewPath("spec", "license"),
				"edition EE requires license.externalSecret.enabled or license.secretRef",
			))
		}
	}

	if gw.Spec.Edition == v1alpha1.EditionCE && gw.Spec.License != nil {
		if gw.Spec.License.ExternalSecret.Enabled || gw.Spec.License.SecretRef != nil {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec", "license"),
				"CE edition does not require license configuration",
			))
		}
	}

	if gw.Spec.License != nil &&
		gw.Spec.License.ExternalSecret.Enabled && gw.Spec.License.SecretRef != nil {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "license"),
			"both",
			"externalSecret and secretRef are mutually exclusive",
		))
	}

	if gw.Spec.Plugins != nil {
		pvcCount := 0
		for _, src := range gw.Spec.Plugins.Sources {
			if src.PersistentVolumeClaimRef != nil {
				pvcCount++
			}
		}
		if pvcCount > 1 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "plugins", "sources"),
				pvcCount,
				"only one PVC plugin source is supported",
			))
		}
	}

	return errs.ToAggregate()
}

// EndpointValidator validates KrakenDEndpoint resources.
type EndpointValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDEndpoint.
func (v *EndpointValidator) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDEndpoint, got %T", obj)
	}
	errs, warnings := v.validate(ctx, ep)
	return warnings, errs.ToAggregate()
}

// ValidateUpdate validates an updated KrakenDEndpoint.
func (v *EndpointValidator) ValidateUpdate(
	ctx context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	ep, ok := newObj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDEndpoint, got %T", newObj)
	}
	errs, warnings := v.validate(ctx, ep)
	return warnings, errs.ToAggregate()
}

// ValidateDelete is a no-op for endpoints.
func (v *EndpointValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

func (v *EndpointValidator) validate(
	ctx context.Context,
	ep *v1alpha1.KrakenDEndpoint,
) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings

	gw := &v1alpha1.KrakenDGateway{}
	if err := v.Get(ctx, types.NamespacedName{
		Name:      ep.Spec.GatewayRef.Name,
		Namespace: ep.Namespace,
	}, gw); err != nil {
		errs = append(errs, field.NotFound(
			field.NewPath("spec", "gatewayRef", "name"),
			ep.Spec.GatewayRef.Name,
		))
	}

	for i, entry := range ep.Spec.Endpoints {
		for j, be := range entry.Backends {
			if be.PolicyRef != nil {
				policy := &v1alpha1.KrakenDBackendPolicy{}
				if err := v.Get(ctx, types.NamespacedName{
					Name:      be.PolicyRef.Name,
					Namespace: ep.Namespace,
				}, policy); err != nil {
					errs = append(errs, field.NotFound(
						field.NewPath("spec", "endpoints").Index(i).
							Child("backends").Index(j).
							Child("policyRef", "name"),
						be.PolicyRef.Name,
					))
				}
			}
		}
	}

	var existing v1alpha1.KrakenDEndpointList
	if err := v.List(ctx, &existing, client.InNamespace(ep.Namespace)); err == nil {
		for _, newEntry := range ep.Spec.Endpoints {
			for _, other := range existing.Items {
				if other.Name == ep.Name {
					continue
				}
				if other.Spec.GatewayRef.Name != ep.Spec.GatewayRef.Name {
					continue
				}
				for _, otherEntry := range other.Spec.Endpoints {
					if otherEntry.Endpoint == newEntry.Endpoint &&
						otherEntry.Method == newEntry.Method {
						warnings = append(warnings, fmt.Sprintf(
							"endpoint %s %s already exists on gateway %s "+
								"(defined by %s) — conflict resolved by creationTimestamp",
							newEntry.Method, newEntry.Endpoint,
							ep.Spec.GatewayRef.Name, other.Name,
						))
					}
				}
			}
		}
	}

	return errs, warnings
}

// PolicyValidator validates KrakenDBackendPolicy resources.
type PolicyValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateCreate(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	policy, ok := obj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", obj)
	}
	return nil, validatePolicyFields(policy)
}

// ValidateUpdate validates an updated KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateUpdate(
	_ context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	policy, ok := newObj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", newObj)
	}
	return nil, validatePolicyFields(policy)
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
	if err := v.List(ctx, &endpoints, client.InNamespace(policy.Namespace)); err != nil {
		return nil, fmt.Errorf("listing endpoints: %w", err)
	}

	var references []string
	for _, ep := range endpoints.Items {
		for _, entry := range ep.Spec.Endpoints {
			for _, be := range entry.Backends {
				if be.PolicyRef != nil && be.PolicyRef.Name == policy.Name {
					references = append(references, ep.Name)
					break
				}
			}
		}
	}

	if len(references) > 0 {
		return nil, field.ErrorList{
			field.Forbidden(
				field.NewPath("metadata", "name"),
				fmt.Sprintf("policy is referenced by endpoints: %s",
					strings.Join(references, ", ")),
			),
		}.ToAggregate()
	}
	return nil, nil
}

func validatePolicyFields(policy *v1alpha1.KrakenDBackendPolicy) error {
	var errs field.ErrorList

	if policy.Spec.CircuitBreaker != nil {
		if policy.Spec.CircuitBreaker.MaxErrors <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "circuitBreaker", "maxErrors"),
				policy.Spec.CircuitBreaker.MaxErrors,
				"must be greater than 0",
			))
		}
		if policy.Spec.CircuitBreaker.Interval <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "circuitBreaker", "interval"),
				policy.Spec.CircuitBreaker.Interval,
				"must be greater than 0",
			))
		}
		if policy.Spec.CircuitBreaker.Timeout <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "circuitBreaker", "timeout"),
				policy.Spec.CircuitBreaker.Timeout,
				"must be greater than 0",
			))
		}
	}

	if policy.Spec.RateLimit != nil {
		if policy.Spec.RateLimit.MaxRate <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "rateLimit", "maxRate"),
				policy.Spec.RateLimit.MaxRate,
				"must be greater than 0",
			))
		}
	}

	return errs.ToAggregate()
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
	return nil, v.validate(ctx, ac)
}

// ValidateUpdate validates an updated KrakenDAutoConfig.
func (v *AutoConfigValidator) ValidateUpdate(
	ctx context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	ac, ok := newObj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", newObj)
	}
	return nil, v.validate(ctx, ac)
}

// ValidateDelete is a no-op for autoconfigs.
func (v *AutoConfigValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

func (v *AutoConfigValidator) validate(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
) error {
	var errs field.ErrorList

	gw := &v1alpha1.KrakenDGateway{}
	if err := v.Get(ctx, types.NamespacedName{
		Name:      ac.Spec.GatewayRef.Name,
		Namespace: ac.Namespace,
	}, gw); err != nil {
		errs = append(errs, field.NotFound(
			field.NewPath("spec", "gatewayRef", "name"),
			ac.Spec.GatewayRef.Name,
		))
	}

	hasURL := ac.Spec.OpenAPI.URL != ""
	hasCM := ac.Spec.OpenAPI.ConfigMapRef != nil
	if hasURL && hasCM {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "openapi"),
			"both",
			"url and configMapRef are mutually exclusive",
		))
	}
	if !hasURL && !hasCM {
		errs = append(errs, field.Required(
			field.NewPath("spec", "openapi"),
			"one of url or configMapRef is required",
		))
	}

	if hasCM && !hasURL {
		if ac.Spec.URLTransform == nil || len(ac.Spec.URLTransform.HostMapping) == 0 {
			errs = append(errs, field.Required(
				field.NewPath("spec", "urlTransform", "hostMapping"),
				"hostMapping is required when using configMapRef",
			))
		}
	}

	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic {
		if ac.Spec.Periodic == nil || ac.Spec.Periodic.Interval.Duration == 0 {
			errs = append(errs, field.Required(
				field.NewPath("spec", "periodic", "interval"),
				"interval is required when trigger is Periodic",
			))
		}
	}

	if ac.Spec.OpenAPI.Auth != nil {
		if ac.Spec.OpenAPI.Auth.BearerTokenSecret != nil &&
			ac.Spec.OpenAPI.Auth.BasicAuthSecret != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "openapi", "auth"),
				"both",
				"bearerTokenSecret and basicAuthSecret are mutually exclusive",
			))
		}
	}

	return errs.ToAggregate()
}

// SetupWebhooks registers all validating webhooks with the manager.
func SetupWebhooks(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDGateway{}).
		WithValidator(&GatewayValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up gateway webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDEndpoint{}).
		WithValidator(&EndpointValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up endpoint webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDBackendPolicy{}).
		WithValidator(&PolicyValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up policy webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDAutoConfig{}).
		WithValidator(&AutoConfigValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up autoconfig webhook: %w", err)
	}

	return nil
}
