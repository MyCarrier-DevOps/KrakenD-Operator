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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KrakenDBackendPolicySpec defines the desired state of KrakenDBackendPolicy.
type KrakenDBackendPolicySpec struct {
	// CircuitBreaker configures the circuit breaker for backends referencing this policy.
	CircuitBreaker *CircuitBreakerSpec `json:"circuitBreaker,omitempty"`

	// RateLimit configures backend-level rate limiting.
	RateLimit *RateLimitSpec `json:"rateLimit,omitempty"`

	// Cache configures backend response caching.
	Cache *CacheSpec `json:"cache,omitempty"`

	// Raw holds arbitrary backend extra_config JSON that is merged verbatim.
	Raw *runtime.RawExtension `json:"raw,omitempty"`
}

// CircuitBreakerSpec configures the circuit breaker pattern.
type CircuitBreakerSpec struct {
	// Interval is the window duration in seconds for error counting.
	// +kubebuilder:validation:Minimum=1
	Interval int `json:"interval"`

	// Timeout is the duration in seconds the circuit breaker stays open before
	// transitioning to half-open.
	// +kubebuilder:validation:Minimum=1
	Timeout int `json:"timeout"`

	// MaxErrors is the number of consecutive errors within Interval that triggers
	// the circuit breaker to open.
	// +kubebuilder:validation:Minimum=1
	MaxErrors int `json:"maxErrors"`

	// LogStatusChange enables logging when the circuit breaker changes state.
	LogStatusChange bool `json:"logStatusChange,omitempty"`
}

// RateLimitSpec configures backend-level rate limiting.
type RateLimitSpec struct {
	// MaxRate is the maximum number of requests per second allowed.
	// +kubebuilder:validation:Minimum=1
	MaxRate int `json:"maxRate"`

	// Capacity is the token bucket capacity for burst handling.
	// +kubebuilder:validation:Minimum=0
	Capacity int `json:"capacity,omitempty"`
}

// CacheSpec configures backend response caching.
type CacheSpec struct {
	Shared bool `json:"shared,omitempty"`
}

// KrakenDBackendPolicyStatus defines the observed state of KrakenDBackendPolicy.
type KrakenDBackendPolicyStatus struct {
	// ObservedGeneration is the metadata.generation this status was computed for.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	ReferencedBy       int   `json:"referencedBy,omitempty"`
	// Conditions are keyed by type. Ready is the summary condition: False
	// (InvalidCircuitBreaker, InvalidRateLimit) when a field is out of range,
	// False (PolicyInvalid) when the policy fails krakend check on its own,
	// with that output in the message, Unknown (ValidatorUnavailable) when the
	// check could not run, True otherwise.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kbp
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="ReferencedBy",type=integer,JSONPath=`.status.referencedBy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KrakenDBackendPolicy defines circuit-breaker, rate-limit and cache settings,
// plus raw backend extra_config, that KrakenDEndpoint backends reference
// through policyRef.
type KrakenDBackendPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KrakenDBackendPolicySpec   `json:"spec,omitempty"`
	Status KrakenDBackendPolicyStatus `json:"status,omitempty"`
}

// PolicyProtectionFinalizer keeps a KrakenDBackendPolicy while any
// KrakenDEndpoint references it, so deleting a policy never pulls it out from
// under a rendered backend. Deletion completes once nothing references it.
const PolicyProtectionFinalizer = "gateway.krakend.io/policy-protection"

// +kubebuilder:object:root=true

// KrakenDBackendPolicyList contains a list of KrakenDBackendPolicy.
type KrakenDBackendPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KrakenDBackendPolicy `json:"items"`
}
