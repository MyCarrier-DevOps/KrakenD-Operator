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
	return "", "", ""
}
