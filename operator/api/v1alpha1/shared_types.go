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

package v1alpha1

// GatewayRef references a KrakenDGateway by name.
// When Namespace is empty the gateway is assumed to live in the same namespace
// as the referencing resource.
type GatewayRef struct {
	// +kubebuilder:validation:MinLength=1
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
	// +kubebuilder:validation:MinLength=1
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
	ConditionPostRestartJobSkipped    = "PostRestartJobSkipped"

	// ConditionCEFallbackApplied is True while an EE gateway serves its
	// CE-fallback render; its message lists the Enterprise-only features that
	// render removed. Only the gateway controller writes it.
	ConditionCEFallbackApplied = "CEFallbackApplied"

	// ConditionEndpointsReady reports whether every KrakenDEndpoint a
	// KrakenDAutoConfig controls is Ready. Only the AutoConfig controller
	// writes it.
	ConditionEndpointsReady = "EndpointsReady"

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
	// (named for what it reports, whether an observed root request lacks an
	// explicit acknowledgment, not as a factual assertion that the container
	// is running as root) set
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

	// ConditionReady is every kind's summary condition. Each kind's own
	// controller is its only writer.
	ConditionReady = "Ready"

	// ConditionResolvedRefs reports whether a KrakenDEndpoint's gateway and
	// every policy it references exist. Only the endpoint controller writes it.
	ConditionResolvedRefs = "ResolvedRefs"

	// ConditionAccepted reports whether a KrakenDEndpoint is part of its
	// gateway's validated configuration. Only the gateway controller writes it.
	ConditionAccepted = "Accepted"

	// ConditionPluginsResolved reports whether every plugin ConfigMap the
	// gateway mounts exists. Only the gateway controller writes it.
	ConditionPluginsResolved = "PluginsResolved"
)

// Event reason constants for the EventRecorder.
const (
	ReasonConfigDeployed                = "ConfigDeployed"
	ReasonConfigMapsFound               = "ConfigMapsFound"
	ReasonConfigMapNotFound             = "ConfigMapNotFound"
	ReasonConfigValidationFailed        = "ConfigValidationFailed"
	ReasonLicenseExpiringSoon           = "LicenseExpiringSoon"
	ReasonLicenseFallbackCE             = "LicenseFallbackCE"
	ReasonLicenseExpiredNoFallback      = "LicenseExpiredNoFallback"
	ReasonLicenseRestored               = "LicenseRestored"
	ReasonDragonflyNotReady             = "DragonflyNotReady"
	ReasonIstioVSCreated                = "IstioVirtualServiceCreated"
	ReasonEndpointConflict              = "EndpointConflict"
	ReasonLicenseSecretSyncFailed       = "LicenseSecretSyncFailed"
	ReasonLicenseSecretMissing          = "LicenseSecretMissing"
	ReasonCRDNotInstalled               = "CRDNotInstalled"
	ReasonSpecFetched                   = "SpecFetched"
	ReasonSpecFetchFailed               = "SpecFetchFailed"
	ReasonEndpointsGenerated            = "EndpointsGenerated"
	ReasonOperationFiltered             = "OperationFiltered"
	ReasonMissingOperationId            = "MissingOperationId"
	ReasonDuplicateOperationId          = "DuplicateOperationId"
	ReasonUnsupportedMethod             = "UnsupportedMethod"
	ReasonRolloutFailed                 = "RolloutFailed"
	ReasonCUEEvaluationFailed           = "CUEEvaluationFailed"
	ReasonSpecWarning                   = "SpecWarning"
	ReasonAdditionalEndpointOverride    = "AdditionalEndpointOverride"
	ReasonAdditionalEndpointScopeFailed = "AdditionalEndpointScopeFailed"
	ReasonUnmatchedOverride             = "UnmatchedOverride"
	ReasonAmbiguousOverride             = "AmbiguousOverride"
	ReasonEndpointReconcileFailed       = "EndpointReconcileFailed"
	ReasonEndpointRejected              = "EndpointRejected"
	ReasonOperationsFailed              = "OperationsFailed"
	ReasonAllEndpointsReady             = "AllEndpointsReady"
	ReasonEndpointsNotReady             = "EndpointsNotReady"
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
	// respectively. ReasonDragonflyRunAsRootNoRequest is a third, distinct
	// False-state reason, so "someone explicitly opted into root and
	// acknowledged it" (ReasonDragonflyRunAsRootAcknowledged) and "this
	// gateway never asked for root" are told apart.
	ReasonDragonflyRunAsRootUnacknowledged = "RunAsRootUnacknowledged"
	ReasonDragonflyRunAsRootAcknowledged   = "RunAsRootAcknowledged"
	ReasonDragonflyRunAsRootNoRequest      = "NoRunAsRootRequest"

	// ReasonValidatorUnavailable backs ConfigValid=Unknown: krakend check
	// could not run to completion or the validation copy could not be
	// prepared, so the rendered config was not judged.
	// The applied config is unchanged and the reconcile is retried with
	// backoff.
	ReasonValidatorUnavailable = "ValidatorUnavailable"

	// ReasonConfigPublishFailed backs ConfigValid=Unknown: the newest rendered
	// config passed validation but its ConfigMap could not be published, so it
	// is not the applied config. The previously applied config keeps serving
	// and the reconcile is retried with backoff.
	ReasonConfigPublishFailed = "ConfigPublishFailed"

	// ReasonGatewayRootInvalid backs ConfigValid=False when the gateway root,
	// rendered with no endpoint, fails krakend check on its own. No endpoint
	// is judged or blamed, and the applied config keeps serving.
	ReasonGatewayRootInvalid = "GatewayRootInvalid"

	// ReasonCombinedConfigInvalid backs ConfigValid=False when every endpoint
	// passes on its own but the gateway's config fails with them together. No
	// endpoint is blamed, the applied config keeps serving, and the check's
	// output is only in the operator log.
	ReasonCombinedConfigInvalid = "CombinedConfigInvalid"

	// ReasonConfigMapTampered is the event reason for a config ConfigMap that
	// the operator deleted because its krakend.json does not hash to the
	// checksum its name addresses. The operator then publishes the config
	// again.
	ReasonConfigMapTampered = "ConfigMapTampered"

	// ReasonPolicyDeletionBlocked is the event reason for a KrakenDBackendPolicy
	// that is being deleted while a KrakenDEndpoint still references it.
	ReasonPolicyDeletionBlocked = "DeletionBlocked"

	// ResolvedRefs reasons.
	ReasonRefsResolved    = "RefsResolved"
	ReasonGatewayNotFound = "GatewayNotFound"
	ReasonPolicyNotFound  = "PolicyNotFound"

	// Accepted reasons. ReasonEndpointConflict above is also one.
	ReasonAccepted              = "Accepted"
	ReasonPartiallyAccepted     = "PartiallyAccepted"
	ReasonGatewayConfigRejected = "GatewayConfigRejected"
	ReasonSchemaNameConflict    = "SchemaNameConflict"
	ReasonEEFeaturesStripped    = "EEFeaturesStripped"
	// ReasonEndpointInvalid: the endpoint fails krakend check on its own (the
	// gateway root with this endpoint and the policies it references). The
	// gateway leaves it out and serves its other endpoints.
	ReasonEndpointInvalid = "EndpointInvalid"
	// ReasonPolicyInvalid: a policy the endpoint references fails krakend
	// check on its own, or the endpoint fails only together with a policy of
	// another namespace. The gateway leaves it out; the message names the
	// policy and never quotes it.
	ReasonPolicyInvalid = "PolicyInvalid"

	// Ready reasons shared by every kind. A False Ready carries the reason of
	// the condition that keeps the object from being ready.
	ReasonReady   = "Ready"
	ReasonPending = "Pending"

	// ReasonConfigApplied is the reason of a gateway's ConfigValid=True: the
	// rendered configuration passed validation and is the applied one.
	ReasonConfigApplied = "ConfigApplied"

	// ReasonAwaitingAvailability is the reason of a gateway's Ready=False
	// while its Deployment has not reported available replicas.
	ReasonAwaitingAvailability = "AwaitingAvailability"
)

// Reasons of the LicenseValid, LicenseExpired and LicenseDegraded conditions.
const (
	// ReasonLicenseOK: the EE license is valid beyond the warning window.
	ReasonLicenseOK = "LicenseOK"
	// ReasonLicensePreExpiry: the EE license expires within the safety buffer;
	// the gateway already acts as if it had expired.
	ReasonLicensePreExpiry = "LicensePreExpiry"
	// ReasonLicenseExpired: the EE license certificate has expired.
	ReasonLicenseExpired = "LicenseExpired"
)
