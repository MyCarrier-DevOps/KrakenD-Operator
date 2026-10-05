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
	"unicode/utf8"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
)

func TestTruncate_BoundsBytesAndMarksTheCut(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short) = %q, want it unchanged", got)
	}
	// 100 three-byte runes: 300 bytes, over the cap by byte count and well
	// under it by rune count.
	got := truncate(strings.Repeat("€", 100), maxStatusMessageLen)
	if len(got) > maxStatusMessageLen {
		t.Errorf("len = %d bytes, want at most %d", len(got), maxStatusMessageLen)
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate = %q, want valid UTF-8 ending in \"...\"", got)
	}
}

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

func TestSpecWarnings_AreDistinctSortedAndBounded(t *testing.T) {
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
	got = specWarnings(many)
	if len(got) != maxStatusListLen || got[0] != "warning 00" {
		t.Errorf("specWarnings kept %d entries %q, want %d starting at \"warning 00\"", len(got), got, maxStatusListLen)
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
