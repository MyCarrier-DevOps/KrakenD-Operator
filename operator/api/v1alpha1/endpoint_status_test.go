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
