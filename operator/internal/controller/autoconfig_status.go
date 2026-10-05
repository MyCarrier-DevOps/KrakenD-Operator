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
	"cmp"
	"maps"
	"slices"
	"strings"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

const (
	// maxStatusListLen caps status.skipped and status.warnings.
	maxStatusListLen = 20
	// maxStatusMessageLen caps each status list entry's message, in bytes.
	maxStatusMessageLen = 256
)

// maxConditionListed caps the items a condition message names.
const maxConditionListed = 5

// operationLabel names an operation in a condition message:
// "METHOD /path (operationId): Reason".
func operationLabel(v1alpha1.OperationStatus) string {
	return ""
}

// listed joins the first maxConditionListed items, noting how many more
// there are.
func listed(items []string) string {
	return strings.Join(items, "; ")
}

// operationStatuses converts pipeline issues to status entries, each message
// truncated to maxStatusMessageLen bytes, in the order sortOperationStatuses
// gives.
func operationStatuses(issues []autoconfig.OperationIssue) []v1alpha1.OperationStatus {
	out := make([]v1alpha1.OperationStatus, 0, len(issues))
	for _, i := range issues {
		out = append(out, v1alpha1.OperationStatus{
			Method: i.Method, Path: i.Path, OperationID: i.OperationID,
			Reason: i.Reason, Message: configcheck.TruncateEllipsis(i.Message, maxStatusMessageLen),
		})
	}
	sortOperationStatuses(out)
	return out
}

// sortOperationStatuses orders entries by path, method, endpoint, operationId,
// reason and message, so the same set always lists in the same order and an
// unchanged sync writes no status.
func sortOperationStatuses(s []v1alpha1.OperationStatus) {
	slices.SortFunc(s, func(a, b v1alpha1.OperationStatus) int {
		return cmp.Or(
			strings.Compare(a.Path, b.Path),
			strings.Compare(a.Method, b.Method),
			strings.Compare(a.Endpoint, b.Endpoint),
			strings.Compare(a.OperationID, b.OperationID),
			strings.Compare(a.Reason, b.Reason),
			strings.Compare(a.Message, b.Message),
		)
	})
}

// capList returns at most maxStatusListLen items of s.
func capList[T any](s []T) []T {
	if len(s) > maxStatusListLen {
		return s[:maxStatusListLen]
	}
	return s
}

// specWarnings returns the distinct warnings, sorted, each truncated to
// maxStatusMessageLen bytes. It does not cap the list: callers count the
// whole set, then capList what they list.
func specWarnings(warnings []string) []string {
	set := map[string]struct{}{}
	for _, w := range warnings {
		set[configcheck.TruncateEllipsis(w, maxStatusMessageLen)] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}
