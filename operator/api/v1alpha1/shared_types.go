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

package v1alpha1

// GatewayRef references a KrakenDGateway by name.
// When Namespace is empty the gateway is assumed to live in the same namespace
// as the referencing resource.
type GatewayRef struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// ResolvedNamespace returns the explicit namespace if set, otherwise fallback.
func (r *GatewayRef) ResolvedNamespace(fallback string) string {
	if r.Namespace != "" {
		return r.Namespace
	}
	return fallback
}

// PolicyRef references a KrakenDBackendPolicy by name.
// When Namespace is empty the policy is assumed to live in the same namespace
// as the referencing resource.
type PolicyRef struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
}

// ResolvedNamespace returns the explicit namespace if set, otherwise fallback.
func (r *PolicyRef) ResolvedNamespace(fallback string) string {
	if r.Namespace != "" {
		return r.Namespace
	}
	return fallback
}

// PolicyKey returns the namespace-qualified key ("namespace/name") used to
// look up the policy in the gathered-policies map.
func (r *PolicyRef) PolicyKey(fallback string) string {
	return r.ResolvedNamespace(fallback) + "/" + r.Name
}

// ConfigMapKeyRef references a key within a ConfigMap.
type ConfigMapKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// Condition type constants for status conditions across all CRDs.
const (
	ConditionConfigValid              = "ConfigValid"
	ConditionAvailable                = "Available"
	ConditionLicenseValid             = "LicenseValid"
	ConditionLicenseDegraded          = "LicenseDegraded"
	ConditionDragonflyReady           = "DragonflyReady"
	ConditionIstioConfigured          = "IstioConfigured"
	ConditionLicenseSecretUnavailable = "LicenseSecretUnavailable"
	ConditionLicenseExpired           = "LicenseExpired"
	ConditionProgressing              = "Progressing"
	ConditionSpecAvailable            = "SpecAvailable"
	ConditionSynced                   = "Synced"
	ConditionPolicyValid              = "PolicyValid"
)

// Event reason constants for the EventRecorder.
const (
	ReasonConfigDeployed           = "ConfigDeployed"
	ReasonConfigValidationFailed   = "ConfigValidationFailed"
	ReasonLicenseExpiringSoon      = "LicenseExpiringSoon"
	ReasonLicenseFallbackCE        = "LicenseFallbackCE"
	ReasonLicenseExpiredNoFallback = "LicenseExpiredNoFallback"
	ReasonLicenseRestored          = "LicenseRestored"
	ReasonDragonflyNotReady        = "DragonflyNotReady"
	ReasonIstioVSCreated           = "IstioVirtualServiceCreated"
	ReasonEndpointConflict         = "EndpointConflict"
	ReasonEndpointInvalid          = "EndpointInvalid"
	ReasonLicenseSecretSyncFailed  = "LicenseSecretSyncFailed"
	ReasonLicenseSecretMissing     = "LicenseSecretMissing"
	ReasonSpecFetched              = "SpecFetched"
	ReasonSpecFetchFailed          = "SpecFetchFailed"
	ReasonEndpointsGenerated       = "EndpointsGenerated"
	ReasonOperationFiltered        = "OperationFiltered"
	ReasonMissingOperationId       = "MissingOperationId"
	ReasonDuplicateOperationId     = "DuplicateOperationId"
	ReasonRolloutFailed            = "RolloutFailed"
	ReasonCUEEvaluationFailed      = "CUEEvaluationFailed"
)
