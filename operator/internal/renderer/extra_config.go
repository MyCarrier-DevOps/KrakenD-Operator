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

package renderer

import (
	"encoding/json"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// buildBackendExtraConfig merges backend extra_config from policy and inline sources.
// Precedence (highest wins): inline extraConfig > policy typed fields > policy raw.
func buildBackendExtraConfig(
	backend v1alpha1.BackendSpec,
	policies map[string]*v1alpha1.KrakenDBackendPolicy,
) map[string]any {
	merged := make(map[string]any)

	// Layer 1 + 2: policy raw and typed fields
	applyPolicyExtraConfig(merged, backend.PolicyRef, policies)

	// Layer 3: inline backend extraConfig (highest precedence)
	if backend.ExtraConfig != nil && backend.ExtraConfig.Raw != nil {
		var inline map[string]any
		if err := json.Unmarshal(backend.ExtraConfig.Raw, &inline); err == nil {
			for k, v := range inline {
				merged[k] = v
			}
		}
	}

	if len(merged) == 0 {
		return nil
	}
	return merged
}

func applyPolicyExtraConfig(
	merged map[string]any,
	policyRef *v1alpha1.PolicyRef,
	policies map[string]*v1alpha1.KrakenDBackendPolicy,
) {
	if policyRef == nil {
		return
	}
	policy, ok := policies[policyRef.Name]
	if !ok {
		return
	}

	// Layer 1: policy raw (lowest precedence)
	if policy.Spec.Raw != nil && policy.Spec.Raw.Raw != nil {
		var raw map[string]any
		if err := json.Unmarshal(policy.Spec.Raw.Raw, &raw); err == nil {
			for k, v := range raw {
				merged[k] = v
			}
		}
	}

	// Layer 2: policy typed fields (overwrite raw keys)
	if policy.Spec.CircuitBreaker != nil {
		merged["qos/circuit-breaker"] = map[string]any{
			"interval":          policy.Spec.CircuitBreaker.Interval,
			"timeout":           policy.Spec.CircuitBreaker.Timeout,
			"max_errors":        policy.Spec.CircuitBreaker.MaxErrors,
			"log_status_change": policy.Spec.CircuitBreaker.LogStatusChange,
		}
	}
	if policy.Spec.RateLimit != nil {
		rl := map[string]any{
			"max_rate": policy.Spec.RateLimit.MaxRate,
		}
		if policy.Spec.RateLimit.Capacity > 0 {
			rl["capacity"] = policy.Spec.RateLimit.Capacity
		}
		merged["qos/ratelimit/proxy"] = rl
	}
	if policy.Spec.Cache != nil {
		merged["qos/http-cache"] = map[string]any{
			"shared": policy.Spec.Cache.Shared,
		}
	}
}

// buildEndpointExtraConfig extracts endpoint-level extra_config from the entry.
func buildEndpointExtraConfig(extraConfig []byte) map[string]any {
	if len(extraConfig) == 0 {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(extraConfig, &result); err != nil {
		return nil
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
