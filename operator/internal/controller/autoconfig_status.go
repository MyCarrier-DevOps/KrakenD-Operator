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
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
func operationLabel(s v1alpha1.OperationStatus) string {
	label := s.Path
	if s.Method != "" {
		label = s.Method + " " + label
	}
	if s.OperationID != "" {
		label += " (" + s.OperationID + ")"
	}
	return configcheck.TruncateEllipsis(label, maxStatusMessageLen) + ": " + s.Reason
}

// listed joins the first maxConditionListed items, noting how many more
// there are.
func listed(items []string) string {
	if len(items) <= maxConditionListed {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(items[:maxConditionListed], "; "),
		len(items)-maxConditionListed)
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

// rejectedStatuses converts rejected endpoint writes to status entries,
// naming each operation by its endpoint's single entry and opIDs (keyed by
// "path:METHOD"). The entries are in no particular order: the caller sorts the
// whole failed list once.
func rejectedStatuses(rejected map[string]rejection, opIDs map[string]string) []v1alpha1.OperationStatus {
	out := make([]v1alpha1.OperationStatus, 0, len(rejected))
	for name, rej := range rejected {
		s := v1alpha1.OperationStatus{
			Endpoint: name, Reason: rej.reason, Message: configcheck.TruncateEllipsis(rej.message, maxStatusMessageLen),
		}
		if entries := rej.endpoint.Spec.Endpoints; len(entries) > 0 {
			s.Method, s.Path = entries[0].Method, entries[0].Endpoint
			s.OperationID = opIDs[s.Path+":"+s.Method]
		}
		out = append(out, s)
	}
	return out
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

// endpointReadiness summarizes the Ready condition of the endpoints an
// AutoConfig controls.
type endpointReadiness struct {
	total, ready int
	// notReady names each endpoint that is not ready and why
	// ("name: Reason"), sorted by name.
	notReady []string
}

// endpointPending is the not-ready reason of an endpoint whose current
// generation the endpoint controller has not reported on yet.
const endpointPending = "Pending"

// summarizeReadiness summarizes the endpoints the AutoConfig controls after
// a reconcile: controlled as the reconcile left them (a written one as the
// write returned it), minus those it deleted. An endpoint whose current
// generation the endpoint controller has not reported on, a new one or one
// whose spec just changed, is Pending.
func summarizeReadiness(controlled []v1alpha1.KrakenDEndpoint, deleted map[string]bool) endpointReadiness {
	var r endpointReadiness
	for _, ep := range slices.SortedFunc(slices.Values(controlled), func(a, b v1alpha1.KrakenDEndpoint) int {
		return strings.Compare(a.Name, b.Name)
	}) {
		if deleted[ep.Name] {
			continue
		}
		r.total++
		reason := endpointNotReadyReason(&ep)
		if reason == "" {
			r.ready++
			continue
		}
		r.notReady = append(r.notReady, ep.Name+": "+reason)
	}
	return r
}

// endpointNotReadyReason returns "" when ep's Ready condition is True for its
// current generation, otherwise why it is not ready: the condition's reason,
// or Pending while the endpoint controller has not observed this generation.
func endpointNotReadyReason(ep *v1alpha1.KrakenDEndpoint) string {
	cond := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionReady)
	switch {
	case ep.Status.ObservedGeneration != ep.Generation || cond == nil:
		return endpointPending
	case cond.Status != metav1.ConditionTrue:
		return cond.Reason
	default:
		return ""
	}
}

// endpointsReadyCondition is the EndpointsReady condition for r.
func endpointsReadyCondition(r endpointReadiness, generation int64) metav1.Condition {
	c := metav1.Condition{Type: v1alpha1.ConditionEndpointsReady, ObservedGeneration: generation}
	if len(r.notReady) == 0 {
		c.Status, c.Reason = metav1.ConditionTrue, v1alpha1.ReasonAllEndpointsReady
		c.Message = fmt.Sprintf("%d of %d endpoints ready", r.ready, r.total)
		return c
	}
	c.Status, c.Reason = metav1.ConditionFalse, v1alpha1.ReasonEndpointsNotReady
	c.Message = fmt.Sprintf("%d of %d endpoints not ready: %s", len(r.notReady), r.total, listed(r.notReady))
	return c
}
