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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EndpointReady derives a KrakenDEndpoint's Ready condition from two others:
// ResolvedRefs, written by the endpoint controller, and Accepted, written by
// the gateway controller. Ready is True only when ResolvedRefs is True and
// Accepted is True, for the generation ResolvedRefs was computed for, with
// reason Accepted or SchemaNameConflict. A schema name collision affects only
// the published docs, never whether the endpoint renders or serves, so Ready
// stays True and carries that reason to keep it visible. Otherwise Ready
// reports the first failing condition's reason and message, ResolvedRefs
// before Accepted. It is Unknown with reason Pending until the gateway
// reports on the endpoint's current generation.
func EndpointReady(conds []metav1.Condition) (status metav1.ConditionStatus, reason, message string) {
	refs := meta.FindStatusCondition(conds, ConditionResolvedRefs)
	if refs == nil {
		return metav1.ConditionUnknown, ReasonPending, "References have not been resolved yet"
	}
	if refs.Status != metav1.ConditionTrue {
		return metav1.ConditionFalse, refs.Reason, refs.Message
	}
	accepted := meta.FindStatusCondition(conds, ConditionAccepted)
	if accepted == nil || accepted.ObservedGeneration != refs.ObservedGeneration {
		return metav1.ConditionUnknown, ReasonPending,
			fmt.Sprintf("Waiting for the gateway to accept generation %d", refs.ObservedGeneration)
	}
	if accepted.Status != metav1.ConditionTrue {
		return metav1.ConditionFalse, accepted.Reason, accepted.Message
	}
	switch accepted.Reason {
	case ReasonAccepted:
		return metav1.ConditionTrue, ReasonReady, "References resolved and accepted by the gateway"
	case ReasonSchemaNameConflict:
		return metav1.ConditionTrue, ReasonSchemaNameConflict, accepted.Message
	default:
		return metav1.ConditionFalse, accepted.Reason, accepted.Message
	}
}

// EndpointPhaseFromReady returns the compatibility phase for a KrakenDEndpoint
// whose Ready condition has the given status and reason.
func EndpointPhaseFromReady(status metav1.ConditionStatus, reason string) EndpointPhase {
	switch {
	case status == metav1.ConditionTrue:
		return EndpointPhaseActive
	case status == metav1.ConditionUnknown:
		return EndpointPhasePending
	case reason == ReasonGatewayNotFound:
		return EndpointPhaseDetached
	case reason == ReasonEndpointConflict, reason == ReasonPartiallyAccepted:
		return EndpointPhaseConflicted
	}
	return ""
}
