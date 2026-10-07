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

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
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
		{
			Type:               "Available",
			Status:             metav1.ConditionFalse,
			Reason:             "Degraded",
			Message:            "not ready",
			ObservedGeneration: 2,
		},
	}
	b := []metav1.Condition{
		{
			Type:               "Available",
			Status:             metav1.ConditionFalse,
			Reason:             "Degraded",
			Message:            "not ready",
			ObservedGeneration: 2,
		},
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

func TestDistinctMethods(t *testing.T) {
	tests := []struct {
		name    string
		entries []v1alpha1.EndpointEntry
		want    string
	}{
		{name: "empty", entries: nil, want: ""},
		{name: "single", entries: []v1alpha1.EndpointEntry{
			{Method: "GET"},
		}, want: "GET"},
		{name: "multiple sorted", entries: []v1alpha1.EndpointEntry{
			{Method: "POST"}, {Method: "GET"}, {Method: "DELETE"},
		}, want: "DELETE,GET,POST"},
		{name: "duplicates removed", entries: []v1alpha1.EndpointEntry{
			{Method: "GET"}, {Method: "GET"}, {Method: "POST"},
		}, want: "GET,POST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := distinctMethods(tt.entries)
			if got != tt.want {
				t.Errorf("distinctMethods() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFieldIndexesReturnCorrectValues(t *testing.T) {
	// Use fakeClientBuilder which registers the canonical index functions,
	// rather than duplicating closures inline.
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw1"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET", Backends: []v1alpha1.BackendSpec{
					{Host: []string{"http://svc:80"}, URLPattern: "/",
						PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
					{Host: []string{"http://svc:80"}, URLPattern: "/alt",
						PolicyRef: &v1alpha1.PolicyRef{Name: "pol1"}},
				}},
			},
		},
	}
	c := fakeClientBuilder().WithObjects(ep).Build()

	// Verify the gateway index works.
	var list v1alpha1.KrakenDEndpointList
	if err := c.List(context.Background(), &list,
		client.MatchingFields{fieldindex.EndpointGateway: "default/gw1"},
	); err != nil {
		t.Fatalf("gateway index lookup failed: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 endpoint, got %d", len(list.Items))
	}

	// Verify the policy index query returns the endpoint.
	if err := c.List(context.Background(), &list,
		client.MatchingFields{fieldindex.EndpointPolicy: "default/pol1"},
	); err != nil {
		t.Fatalf("policy index lookup failed: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected 1 endpoint, got %d", len(list.Items))
	}

	// Verify non-matching lookup returns 0.
	if err := c.List(context.Background(), &list,
		client.MatchingFields{fieldindex.EndpointPolicy: "default/nonexistent"},
	); err != nil {
		t.Fatalf("policy index lookup failed: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected 0 endpoints for nonexistent policy, got %d", len(list.Items))
	}
}

func TestTruncateMessage(t *testing.T) {
	if got := truncateMessage("short"); got != "short" {
		t.Errorf("a message within the bound must be unchanged, got %q", got)
	}
	long := "x" + strings.Repeat("é", 5000) // one line; rune starts fall on odd offsets
	got := truncateMessage(long)
	if len(got) > 4096 || !utf8.ValidString(got) {
		t.Errorf("got %d bytes, valid UTF-8 = %v; want at most 4096 valid bytes", len(got), utf8.ValidString(got))
	}

	line := strings.Repeat("l", 100)
	lines := truncateMessage(strings.Repeat(line+"\n", 100)) // 100 lines, 101 bytes each
	// 4096-64 bytes hold 39 whole lines; the empty string after the final
	// newline is not a line, so 61 of the 100 remain.
	if want := "(output truncated, 61 more lines)"; !strings.HasSuffix(lines, want) {
		t.Errorf("got ending %q, want it to end with %q", lines[max(0, len(lines)-50):], want)
	}
}

func TestRecordConditionTransition(t *testing.T) {
	obj := &v1alpha1.KrakenDEndpoint{ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "default"}}
	notAccepted := func(reason string) metav1.Condition {
		return metav1.Condition{Type: "Accepted", Status: metav1.ConditionFalse, Reason: reason, Message: reason + " message"}
	}
	accepted := metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", Message: "included"}
	tests := []struct {
		name string
		prev *metav1.Condition
		next metav1.Condition
		want []string
	}{
		{"first set True", nil, accepted, nil},
		{"first set False", nil, notAccepted("EndpointConflict"),
			[]string{"Warning EndpointConflict EndpointConflict message"}},
		{"False unchanged", new(notAccepted("EndpointConflict")), notAccepted("EndpointConflict"), nil},
		{"False with a new reason", new(notAccepted("EndpointConflict")), notAccepted("EndpointInvalid"),
			[]string{"Warning EndpointInvalid EndpointInvalid message"}},
		{"True to False", &accepted, notAccepted("EndpointConflict"),
			[]string{"Warning EndpointConflict EndpointConflict message"}},
		{"False to True", new(notAccepted("EndpointConflict")), accepted, []string{"Normal Accepted included"}},
		{"True unchanged", &accepted, accepted, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := record.NewFakeRecorder(10)
			recordConditionTransition(rec, obj, tt.prev, tt.next)
			if got := drainEvents(rec); !slices.Equal(got, tt.want) {
				t.Errorf("events = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSameCondition(t *testing.T) {
	base := metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", Message: "m",
		ObservedGeneration: 1}
	later := base
	later.LastTransitionTime = metav1.Now()
	otherGen := base
	otherGen.ObservedGeneration = 2
	tests := []struct {
		name string
		a, b *metav1.Condition
		want bool
	}{
		{"both absent", nil, nil, true},
		{"one absent", &base, nil, false},
		{"only lastTransitionTime differs", &base, &later, true},
		{"observedGeneration differs", &base, &otherGen, false},
	}
	for _, tt := range tests {
		if got := sameCondition(tt.a, tt.b); got != tt.want {
			t.Errorf("%s: sameCondition = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestExistencePredicate(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 1}}
	changed := gw.DeepCopy()
	changed.Generation = 2
	p := existencePredicate()
	if !p.Create(event.CreateEvent{Object: gw}) || !p.Delete(event.DeleteEvent{Object: gw}) {
		t.Error("create and delete events must pass: they change whether the object exists")
	}
	if p.Update(event.UpdateEvent{ObjectOld: gw, ObjectNew: changed}) {
		t.Error("update events must not pass: an update never changes whether the object exists")
	}
}
