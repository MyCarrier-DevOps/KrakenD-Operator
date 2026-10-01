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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testCondition(typ string, status metav1.ConditionStatus, reason string, generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		Message:            reason + " message",
		ObservedGeneration: generation,
	}
}

func TestEndpointReady(t *testing.T) {
	refsOK := testCondition("ResolvedRefs", metav1.ConditionTrue, "RefsResolved", 2)
	tests := []struct {
		name        string
		conds       []metav1.Condition
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string // substring
	}{
		{"no conditions", nil, metav1.ConditionUnknown, "Pending", "not been resolved"},
		{
			"legacy Available only",
			[]metav1.Condition{testCondition("Available", metav1.ConditionTrue, "ReferencesValid", 1)},
			metav1.ConditionUnknown, "Pending", "not been resolved",
		},
		{
			"gateway missing",
			[]metav1.Condition{testCondition("ResolvedRefs", metav1.ConditionFalse, "GatewayNotFound", 2)},
			metav1.ConditionFalse, "GatewayNotFound", "GatewayNotFound message",
		},
		{
			"refs failure wins over acceptance",
			[]metav1.Condition{
				testCondition("ResolvedRefs", metav1.ConditionFalse, "PolicyNotFound", 2),
				testCondition("Accepted", metav1.ConditionTrue, "Accepted", 2),
			},
			metav1.ConditionFalse, "PolicyNotFound", "PolicyNotFound message",
		},
		{"not yet accepted", []metav1.Condition{refsOK}, metav1.ConditionUnknown, "Pending", "generation 2"},
		{
			"accepted for an older generation",
			[]metav1.Condition{refsOK, testCondition("Accepted", metav1.ConditionTrue, "Accepted", 1)},
			metav1.ConditionUnknown, "Pending", "generation 2",
		},
		{
			"accepted for a newer generation",
			[]metav1.Condition{refsOK, testCondition("Accepted", metav1.ConditionTrue, "Accepted", 3)},
			metav1.ConditionUnknown, "Pending", "generation 2",
		},
		{
			"refs unknown",
			[]metav1.Condition{
				testCondition("ResolvedRefs", metav1.ConditionUnknown, "Pending", 2),
				testCondition("Accepted", metav1.ConditionTrue, "Accepted", 2),
			},
			metav1.ConditionFalse, "Pending", "Pending message",
		},
		{
			"accepted",
			[]metav1.Condition{refsOK, testCondition("Accepted", metav1.ConditionTrue, "Accepted", 2)},
			metav1.ConditionTrue, "Ready", "",
		},
		{
			"conflict",
			[]metav1.Condition{refsOK, testCondition("Accepted", metav1.ConditionFalse, "EndpointConflict", 2)},
			metav1.ConditionFalse, "EndpointConflict", "EndpointConflict message",
		},
		{
			"configuration rejected",
			[]metav1.Condition{refsOK, testCondition("Accepted", metav1.ConditionFalse, "GatewayConfigRejected", 2)},
			metav1.ConditionFalse, "GatewayConfigRejected", "GatewayConfigRejected message",
		},
		{
			"accepted with a docs-only schema name conflict",
			[]metav1.Condition{refsOK, testCondition("Accepted", metav1.ConditionTrue, "SchemaNameConflict", 2)},
			metav1.ConditionTrue, "SchemaNameConflict", "SchemaNameConflict message",
		},
		{
			"accepted with a partial reason",
			[]metav1.Condition{refsOK, testCondition("Accepted", metav1.ConditionTrue, "PartiallyAccepted", 2)},
			metav1.ConditionFalse, "PartiallyAccepted", "PartiallyAccepted message",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, message := EndpointReady(tt.conds)
			if status != tt.wantStatus || reason != tt.wantReason || !strings.Contains(message, tt.wantMessage) {
				t.Errorf("EndpointReady() = (%s, %q, %q), want (%s, %q, a message containing %q)",
					status, reason, message, tt.wantStatus, tt.wantReason, tt.wantMessage)
			}
		})
	}
}

func TestEndpointPhaseFromReady(t *testing.T) {
	tests := []struct {
		status metav1.ConditionStatus
		reason string
		want   EndpointPhase
	}{
		{metav1.ConditionTrue, "Ready", "Active"},
		{metav1.ConditionUnknown, "Pending", "Pending"},
		{metav1.ConditionFalse, "GatewayNotFound", "Detached"},
		{metav1.ConditionFalse, "EndpointConflict", "Conflicted"},
		{metav1.ConditionFalse, "PartiallyAccepted", "Conflicted"},
		{metav1.ConditionTrue, "SchemaNameConflict", "Active"},
		{metav1.ConditionFalse, "PolicyNotFound", "Invalid"},
		{metav1.ConditionFalse, "GatewayConfigRejected", "Invalid"},
		{metav1.ConditionFalse, "EEFeaturesStripped", "Invalid"},
	}
	for _, tt := range tests {
		if got := EndpointPhaseFromReady(tt.status, tt.reason); got != tt.want {
			t.Errorf("EndpointPhaseFromReady(%s, %q) = %q, want %q", tt.status, tt.reason, got, tt.want)
		}
	}
}
