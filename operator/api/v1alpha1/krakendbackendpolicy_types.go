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
	Interval        int  `json:"interval"`
	Timeout         int  `json:"timeout"`
	MaxErrors       int  `json:"maxErrors"`
	LogStatusChange bool `json:"logStatusChange,omitempty"`
}

// RateLimitSpec configures backend-level rate limiting.
type RateLimitSpec struct {
	MaxRate  int `json:"maxRate"`
	Capacity int `json:"capacity,omitempty"`
}

// CacheSpec configures backend response caching.
type CacheSpec struct {
	Shared bool `json:"shared,omitempty"`
}

// KrakenDBackendPolicyStatus defines the observed state of KrakenDBackendPolicy.
type KrakenDBackendPolicyStatus struct {
	ReferencedBy int                `json:"referencedBy,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=kbp
// +kubebuilder:printcolumn:name="ReferencedBy",type=integer,JSONPath=`.status.referencedBy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KrakenDBackendPolicy is the Schema for the krakendbackendpolicies API.
type KrakenDBackendPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KrakenDBackendPolicySpec   `json:"spec,omitempty"`
	Status KrakenDBackendPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KrakenDBackendPolicyList contains a list of KrakenDBackendPolicy.
type KrakenDBackendPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KrakenDBackendPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KrakenDBackendPolicy{}, &KrakenDBackendPolicyList{})
}
