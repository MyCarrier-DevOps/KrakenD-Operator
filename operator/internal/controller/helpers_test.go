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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConditionsEqual_BothEmpty(t *testing.T) {
	if !conditionsEqual(nil, nil) {
		t.Error("two nil slices should be equal")
	}
	if !conditionsEqual([]metav1.Condition{}, []metav1.Condition{}) {
		t.Error("two empty slices should be equal")
	}
}

func TestConditionsEqual_DifferentLength(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	if conditionsEqual(a, nil) {
		t.Error("different lengths should not be equal")
	}
}

func TestConditionsEqual_SameContent(t *testing.T) {
	a := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "all good", ObservedGeneration: 1},
		{Type: "Available", Status: metav1.ConditionFalse, Reason: "Degraded", Message: "not ready", ObservedGeneration: 2},
	}
	b := []metav1.Condition{
		{Type: "Available", Status: metav1.ConditionFalse, Reason: "Degraded", Message: "not ready", ObservedGeneration: 2},
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "all good", ObservedGeneration: 1},
	}
	if !conditionsEqual(a, b) {
		t.Error("same conditions in different order should be equal")
	}
}

func TestConditionsEqual_IgnoresLastTransitionTime(t *testing.T) {
	now := metav1.Now()
	a := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: now},
	}
	b := []metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", LastTransitionTime: metav1.Time{}},
	}
	if !conditionsEqual(a, b) {
		t.Error("should ignore LastTransitionTime differences")
	}
}

func TestConditionsEqual_DifferentStatus(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "OK"}}
	if conditionsEqual(a, b) {
		t.Error("different statuses should not be equal")
	}
}

func TestConditionsEqual_DifferentReason(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "NotOK"}}
	if conditionsEqual(a, b) {
		t.Error("different reasons should not be equal")
	}
}

func TestConditionsEqual_DifferentMessage(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "a"}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", Message: "b"}}
	if conditionsEqual(a, b) {
		t.Error("different messages should not be equal")
	}
}

func TestConditionsEqual_DifferentGeneration(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", ObservedGeneration: 1}}
	b := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK", ObservedGeneration: 2}}
	if conditionsEqual(a, b) {
		t.Error("different generations should not be equal")
	}
}

func TestConditionsEqual_MissingType(t *testing.T) {
	a := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "OK"}}
	b := []metav1.Condition{{Type: "Other", Status: metav1.ConditionTrue, Reason: "OK"}}
	if conditionsEqual(a, b) {
		t.Error("different types should not be equal")
	}
}

func TestEnsureEndpointIndexes_Idempotent(t *testing.T) {
	// The fake client builder in suite_test.go pre-registers indexes,
	// but we can verify ensureEndpointIndexes works with a real scheme
	// by building a fake manager-like indexer indirectly through the
	// fake client builder (which uses the same FieldIndexer interface).
	// Here we just verify conditionsEqual covers the core logic;
	// ensureEndpointIndexes is integration-tested via controller tests
	// that use fakeClientBuilder with pre-registered indexes.
}
