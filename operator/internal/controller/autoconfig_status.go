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
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

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

// plural returns one when n is 1, many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// counted formats n of noun, pluralized: "1 operation", "2 operations".
func counted(n int, noun string) string {
	return fmt.Sprintf("%d %s", n, plural(n, noun, noun+"s"))
}

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
		c.Message = fmt.Sprintf("%d of %s ready", r.ready, counted(r.total, "endpoint"))
		return c
	}
	c.Status, c.Reason = metav1.ConditionFalse, v1alpha1.ReasonEndpointsNotReady
	c.Message = fmt.Sprintf("%d of %s not ready: %s", len(r.notReady), counted(r.total, "endpoint"), listed(r.notReady))
	return c
}

// inputWarnings collects the Warning events about one reconcile's inputs —
// DuplicateOperationId, AdditionalEndpointOverride and SpecWarning — and holds
// them until the reconcile's terminal status write succeeds. A reconcile that
// read a stale AutoConfig and then loses that write to a conflict records
// none of them; its retry records them if they still apply. Warnings are
// collected only when inputsChanged.
type inputWarnings struct {
	inputsChanged bool
	pending       []inputWarning
}

// inputWarning is one buffered Warning event.
type inputWarning struct {
	reason, message string
}

// add buffers a Warning event with the given reason and message, if the
// reconcile's inputs changed.
func (w *inputWarnings) add(reason, message string) {
	if w.inputsChanged {
		w.pending = append(w.pending, inputWarning{reason: reason, message: message})
	}
}

// emit records the first maxStatusListLen buffered events on ac, whatever
// their reasons. The recorder's per-object budget is shared by every Warning
// event, so the cap leaves room for the failure event of a failing pass.
func (w *inputWarnings) emit(recorder record.EventRecorder, ac *v1alpha1.KrakenDAutoConfig) {
	for _, ev := range capList(w.pending) {
		recorder.Event(ac, "Warning", ev.reason, ev.message)
	}
}

// syncResult is what a pipeline pass that reached its endpoint writes
// produced, for recordSync.
type syncResult struct {
	// checksum is the pass's combined input checksum.
	checksum string
	// generated counts the endpoints the pass generated.
	generated int
	// skipped lists the operations the pass generated no endpoint for.
	skipped []v1alpha1.OperationStatus
	// failed lists the operations the pass could not converge.
	failed []v1alpha1.OperationStatus
	// warnings lists every distinct problem that does not stop a sync; the
	// status lists the first maxStatusListLen.
	warnings []string
	changes  endpointChanges
	// readiness summarizes the endpoints the AutoConfig controls afterwards.
	readiness endpointReadiness
}

// recordSync records a sync that reached its endpoint writes: the combined
// checksum, the endpoint counts and lists, and the Synced condition (True, or
// False with reason OperationsFailed while res.failed is not empty) with the
// Ready, phase and observedGeneration derived from it. The synced gauge
// follows Synced. LastSyncTime and the EndpointsGenerated event mark a sync
// that changed something: new inputs (a different combined checksum) or
// endpoint writes, so a steady-state reconcile leaves both alone. Status is
// written only when it differs from orig, the status read at the start of the
// reconcile, and an OperationsFailed Warning event is recorded only when the
// Synced condition or status.failedOperations changed (failureChanged). The
// buffered input warnings are recorded once that write succeeds,
// before EndpointsGenerated.
func (r *KrakenDAutoConfigReconciler) recordSync(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	orig *v1alpha1.KrakenDAutoConfigStatus,
	res syncResult,
	warnings *inputWarnings,
) error {
	changed := res.checksum != orig.SpecChecksum || res.changes.total() > 0
	ac.Status.SpecChecksum = res.checksum
	if changed {
		now := metav1.Now()
		ac.Status.LastSyncTime = &now
	}
	ac.Status.GeneratedEndpoints = res.generated
	ac.Status.ReadyEndpoints = res.readiness.ready
	ac.Status.SkippedOperations = len(res.skipped)
	ac.Status.Skipped = capList(res.skipped)
	ac.Status.FailedOperations = capList(res.failed)
	ac.Status.Warnings = capList(res.warnings)
	synced := syncedCondition(res, ac.Generation)
	meta.SetStatusCondition(&ac.Status.Conditions, synced)
	meta.SetStatusCondition(&ac.Status.Conditions, endpointsReadyCondition(res.readiness, ac.Generation))
	setAutoConfigReadiness(ac)
	statusChanged := autoConfigStatusChanged(orig, &ac.Status)
	if statusChanged {
		if err := r.Status().Update(ctx, ac); err != nil {
			return fmt.Errorf("updating final status: %w", err)
		}
	}
	gauge := 1.0
	if synced.Status != metav1.ConditionTrue {
		gauge = 0
	}
	autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name).Set(gauge)

	warnings.emit(r.Recorder, ac)
	if synced.Status != metav1.ConditionTrue && failureChanged(orig, &ac.Status) {
		r.Recorder.Event(ac, "Warning", v1alpha1.ReasonOperationsFailed, synced.Message)
	}
	if changed {
		r.Recorder.Eventf(ac, "Normal", v1alpha1.ReasonEndpointsGenerated,
			"Generated %s (%d created, %d updated, %d deleted, %d skipped)",
			counted(res.generated, "endpoint"), res.changes.created, res.changes.updated, res.changes.deleted, len(res.skipped))
	}
	return nil
}

// failureChanged reports whether the Synced condition (status, reason or
// message) or status.failedOperations differ between orig and cur, so a held
// operation warns when its failure changes, not on every status change that
// goes with it, such as an endpoint turning ready.
func failureChanged(orig, cur *v1alpha1.KrakenDAutoConfigStatus) bool {
	if !slices.Equal(orig.FailedOperations, cur.FailedOperations) {
		return true
	}
	was := meta.FindStatusCondition(orig.Conditions, v1alpha1.ConditionSynced)
	now := meta.FindStatusCondition(cur.Conditions, v1alpha1.ConditionSynced)
	if was == nil || now == nil {
		return was != now
	}
	return was.Status != now.Status || was.Reason != now.Reason || was.Message != now.Message
}

// syncedCondition is the Synced condition for res: False with reason
// OperationsFailed, naming the first operations, while res.failed is not
// empty, otherwise True, counting what was skipped and warned about.
func syncedCondition(res syncResult, generation int64) metav1.Condition {
	if len(res.failed) > 0 {
		labels := make([]string, len(res.failed))
		for i, f := range res.failed {
			labels[i] = operationLabel(f)
		}
		return metav1.Condition{
			Type: v1alpha1.ConditionSynced, Status: metav1.ConditionFalse, ObservedGeneration: generation,
			Reason: v1alpha1.ReasonOperationsFailed,
			Message: fmt.Sprintf("%s failed; %s and no stale endpoint is deleted until %s "+
				"(see status.failedOperations): %s",
				counted(len(res.failed), "operation"),
				plural(len(res.failed), "it keeps its last-synced endpoint", "they keep their last-synced endpoints"),
				plural(len(res.failed), "it recovers", "they recover"), listed(labels)),
		}
	}
	c := metav1.Condition{
		Type: v1alpha1.ConditionSynced, Status: metav1.ConditionTrue, ObservedGeneration: generation, Reason: "Synced",
		Message: "Generated " + counted(res.generated, "endpoint"),
	}
	if n := len(res.skipped); n > 0 {
		c.Message += "; " + counted(n, "operation") + " skipped (see status.skipped)"
	}
	if n := len(res.warnings); n > 0 {
		c.Message += "; " + counted(n, "spec warning") + " (see status.warnings)"
	}
	return c
}

// autoConfigStatusChanged reports whether cur differs semantically from orig.
// Conditions are compared with conditionsEqual, which ignores
// LastTransitionTime.
func autoConfigStatusChanged(orig, cur *v1alpha1.KrakenDAutoConfigStatus) bool {
	return orig.Phase != cur.Phase ||
		orig.ObservedGeneration != cur.ObservedGeneration ||
		orig.SpecChecksum != cur.SpecChecksum ||
		orig.GeneratedEndpoints != cur.GeneratedEndpoints ||
		orig.ReadyEndpoints != cur.ReadyEndpoints ||
		orig.SkippedOperations != cur.SkippedOperations ||
		!slices.Equal(orig.Skipped, cur.Skipped) ||
		!slices.Equal(orig.FailedOperations, cur.FailedOperations) ||
		!slices.Equal(orig.Warnings, cur.Warnings) ||
		!orig.LastSyncTime.Equal(cur.LastSyncTime) ||
		!conditionsEqual(orig.Conditions, cur.Conditions)
}
