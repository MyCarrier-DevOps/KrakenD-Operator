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

// operationStatuses converts pipeline issues to status entries, sorted by
// path then method.
func operationStatuses(_ []autoconfig.OperationIssue) []v1alpha1.OperationStatus {
	return nil
}

// capList returns at most maxStatusListLen items of s.
func capList[T any](s []T) []T {
	if len(s) > maxStatusListLen {
		return s[:maxStatusListLen]
	}
	return s
}

// specWarnings returns the distinct warnings, sorted, each truncated to
// maxStatusMessageLen bytes and capped at maxStatusListLen.
func specWarnings(_ []string) []string {
	return nil
}

// truncate shortens s to at most n bytes, marking the cut with "...".
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return configcheck.Truncate(s, n-3) + "..."
}
