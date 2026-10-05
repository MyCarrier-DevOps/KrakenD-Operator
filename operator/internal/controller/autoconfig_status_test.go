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
	"fmt"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
)

func TestCapList_KeepsTheFirstTwentyItems(t *testing.T) {
	items := make([]int, maxStatusListLen+5)
	for i := range items {
		items[i] = i
	}
	got := capList(items)
	if len(got) != maxStatusListLen || got[0] != 0 || got[maxStatusListLen-1] != maxStatusListLen-1 {
		t.Errorf("capList kept %v, want the first %d items", got, maxStatusListLen)
	}
	if short := capList([]int{1, 2}); len(short) != 2 {
		t.Errorf("capList(short) = %v, want it unchanged", short)
	}
}

func TestSpecWarnings_AreDistinctSortedAndTruncated(t *testing.T) {
	got := specWarnings([]string{"b warning", "a warning", "b warning"})
	if want := []string{"a warning", "b warning"}; !slices.Equal(got, want) {
		t.Fatalf("specWarnings = %q, want %q", got, want)
	}

	long := specWarnings([]string{strings.Repeat("x", 1000)})
	if len(long) != 1 || len(long[0]) > maxStatusMessageLen {
		t.Fatalf("a long warning gave %q, want one entry of at most %d bytes", long, maxStatusMessageLen)
	}

	var many []string
	for i := range 2 * maxStatusListLen {
		many = append(many, fmt.Sprintf("warning %02d", i))
	}
	if got := specWarnings(many); len(got) != 2*maxStatusListLen {
		t.Errorf("specWarnings kept %d entries, want all %d: the caller caps the list", len(got), 2*maxStatusListLen)
	}
}

func TestOperationStatuses_AreSortedAndTheirMessagesBounded(t *testing.T) {
	issue := func(method, path, opID, msg string) autoconfig.OperationIssue {
		return autoconfig.OperationIssue{
			Operation: autoconfig.Operation{Method: method, Path: path, OperationID: opID},
			Reason:    v1alpha1.ReasonDuplicateOperationId,
			Message:   msg,
		}
	}
	got := operationStatuses([]autoconfig.OperationIssue{
		issue("POST", "/b", "createB", "m"),
		issue("GET", "/b", "z", "same route"),
		issue("GET", "/b", "a", "same route"),
		issue("GET", "/a", "getA", strings.Repeat("x", 1000)),
	})
	var order []string
	for _, s := range got {
		order = append(order, s.Method+" "+s.Path+" "+s.OperationID)
	}
	want := []string{"GET /a getA", "GET /b a", "GET /b z", "POST /b createB"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %q, want %q", order, want)
	}
	if first := got[0]; len(first.Message) > maxStatusMessageLen || first.Reason != v1alpha1.ReasonDuplicateOperationId {
		t.Errorf("entry = %+v, want a message of at most %d bytes and the issue's reason", first, maxStatusMessageLen)
	}
}

func TestListed_JoinsAtMostFiveItems(t *testing.T) {
	if got, want := listed([]string{"a", "b", "c", "d", "e"}), "a; b; c; d; e"; got != want {
		t.Errorf("listed = %q, want %q", got, want)
	}
}

func TestListed_NamesFiveThenCountsTheRest(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e", "f", "g"}
	if got, want := listed(items), "a; b; c; d; e; and 2 more"; got != want {
		t.Errorf("listed = %q, want %q", got, want)
	}
}

func TestOperationLabel_NamesMethodPathOperationAndReason(t *testing.T) {
	got := operationLabel(v1alpha1.OperationStatus{
		Method: "GET", Path: "/b", OperationID: "getB", Reason: v1alpha1.ReasonCUEEvaluationFailed,
	})
	if want := "GET /b (getB): CUEEvaluationFailed"; got != want {
		t.Errorf("operationLabel = %q, want %q", got, want)
	}
}

func TestOperationLabel_OmitsAMissingOperationID(t *testing.T) {
	got := operationLabel(v1alpha1.OperationStatus{
		Method: "GET", Path: "/b", Reason: v1alpha1.ReasonCUEEvaluationFailed,
	})
	if want := "GET /b: CUEEvaluationFailed"; got != want {
		t.Errorf("operationLabel = %q, want %q", got, want)
	}
}
