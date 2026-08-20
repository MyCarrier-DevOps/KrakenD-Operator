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
	ConditionPostRestartJobSkipped    = "PostRestartJobSkipped"

	// ConditionPostRestartJobReadOnlyRootFilesystem is an informational
	// condition (review id 3805157497, #9) set unconditionally whenever a
	// post-restart Job is created (or re-created), reporting the Job
	// container's effective readOnlyRootFilesystem posture and which mount
	// is writable. Unlike the admission-time workingDir warning (which only
	// fires when workingDir is overridden outside /tmp), this covers the
	// unconditional/default case too (prod's unset workingDir, script does
	// e.g. `npm install -g` under ROFS) without needing to analyze the
	// script — and it lands in `kubectl describe krakendgateway`/Events,
	// which GitOps appliers (that swallow admission.Warnings) do surface.
	ConditionPostRestartJobReadOnlyRootFilesystem = "PostRestartJobReadOnlyRootFilesystem"

	// ConditionDragonflyRunAsRootUnacknowledged is an informational condition
	// (review round 3, C2; renamed review round 4, D5 — the previous name
	// "DragonflyRunAsRoot" read as a factual assertion that the container is
	// running as root, when what the condition actually reports is whether
	// an observed root request lacks an explicit acknowledgment) set
	// whenever a Dragonfly is reconciled, reporting whether the BUILT
	// Dragonfly CR's rendered securityContext maps carry an unacknowledged
	// runAsUser: 0 request (see resources.DragonflyRunAsRootUnacknowledged).
	// Mirrors ConditionPostRestartJobReadOnlyRootFilesystem's rationale: the
	// admission-time check in internal/webhook/webhook.go
	// (validateDragonflyRunAsRoot) only covers Create/Update through an
	// actively-enforcing webhook — a grandfathered spec (update-ratchet) or
	// a webhook-bypass path (disabled, cert-manager absent, downtime) never
	// hits that check, so this condition is the only signal visible via
	// `kubectl describe krakendgateway`/Events for those paths.
	ConditionDragonflyRunAsRootUnacknowledged = "DragonflyRunAsRootUnacknowledged"
)

// Event reason constants for the EventRecorder.
const (
	ReasonConfigDeployed                = "ConfigDeployed"
	ReasonConfigValidationFailed        = "ConfigValidationFailed"
	ReasonLicenseExpiringSoon           = "LicenseExpiringSoon"
	ReasonLicenseFallbackCE             = "LicenseFallbackCE"
	ReasonLicenseExpiredNoFallback      = "LicenseExpiredNoFallback"
	ReasonLicenseRestored               = "LicenseRestored"
	ReasonDragonflyNotReady             = "DragonflyNotReady"
	ReasonIstioVSCreated                = "IstioVirtualServiceCreated"
	ReasonEndpointConflict              = "EndpointConflict"
	ReasonEndpointInvalid               = "EndpointInvalid"
	ReasonLicenseSecretSyncFailed       = "LicenseSecretSyncFailed"
	ReasonLicenseSecretMissing          = "LicenseSecretMissing"
	ReasonSpecFetched                   = "SpecFetched"
	ReasonSpecFetchFailed               = "SpecFetchFailed"
	ReasonEndpointsGenerated            = "EndpointsGenerated"
	ReasonOperationFiltered             = "OperationFiltered"
	ReasonMissingOperationId            = "MissingOperationId"
	ReasonDuplicateOperationId          = "DuplicateOperationId"
	ReasonRolloutFailed                 = "RolloutFailed"
	ReasonCUEEvaluationFailed           = "CUEEvaluationFailed"
	ReasonAdditionalEndpointOverride    = "AdditionalEndpointOverride"
	ReasonAdditionalEndpointScopeFailed = "AdditionalEndpointScopeFailed"
	ReasonPostRestartJobAlreadyRun      = "PostRestartJobAlreadyRun"
	ReasonPostRestartJobCreated         = "PostRestartJobCreated"
	// ReasonPostRestartJobAdopted covers the "Job for this revision's
	// checksum already exists" branch of reconcilePostRestartJob — as
	// opposed to ReasonPostRestartJobCreated, which means this reconcile
	// actually issued the Create call. Split out (review id 3811443603,
	// #7) because the exists-branch was previously (mis)reported under
	// ReasonPostRestartJobCreated for every adoption, including the
	// interrupted-recreate case where the "adopted" Job is still the
	// FAILED Job awaiting re-creation (a Delete failure in
	// reconcileExistingPostRestartRevision's recreate path left it in
	// place) — the "Created"/"already exists" wording implied a healthy
	// outcome for a Job that had not, in fact, successfully re-run.
	ReasonPostRestartJobAdopted      = "PostRestartJobAdopted"
	ReasonPostRestartJobROFSEnabled  = "ReadOnlyRootFilesystemEnabled"
	ReasonPostRestartJobROFSDisabled = "ReadOnlyRootFilesystemDisabled"

	// ReasonDragonflyRunAsRootUnacknowledged/ReasonDragonflyRunAsRootAcknowledged
	// back ConditionDragonflyRunAsRootUnacknowledged's True/False states
	// respectively. ReasonDragonflyRunAsRootNoRequest (review round 4, D5)
	// is a third, distinct False-state reason: ReasonDragonflyRunAsRootAcknowledged
	// previously overloaded the False state for both an acknowledged root
	// request AND the (far more common) no-root-request-at-all case,
	// collapsing "someone explicitly opted into root and acknowledged it"
	// and "this gateway never asked for root" into one indistinguishable
	// reason string.
	ReasonDragonflyRunAsRootUnacknowledged = "RunAsRootUnacknowledged"
	ReasonDragonflyRunAsRootAcknowledged   = "RunAsRootAcknowledged"
	ReasonDragonflyRunAsRootNoRequest      = "NoRunAsRootRequest"
)
