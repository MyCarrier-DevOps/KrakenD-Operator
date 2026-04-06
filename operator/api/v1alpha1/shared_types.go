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

// GatewayRef references a KrakenDGateway by name (same namespace).
type GatewayRef struct {
	Name string `json:"name"`
}

// PolicyRef references a KrakenDBackendPolicy by name (same namespace).
type PolicyRef struct {
	Name string `json:"name"`
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
