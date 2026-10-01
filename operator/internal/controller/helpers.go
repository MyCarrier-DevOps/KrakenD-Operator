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
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// conditionsEqual returns true if two condition slices have the same semantic
// content, compared as a set keyed by Type. LastTransitionTime is ignored
// because meta.SetStatusCondition updates it on every call.
func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]metav1.Condition, len(a))
	for _, c := range a {
		index[c.Type] = c
	}
	for _, c := range b {
		prev, ok := index[c.Type]
		if !ok ||
			prev.Status != c.Status ||
			prev.Reason != c.Reason ||
			prev.Message != c.Message ||
			prev.ObservedGeneration != c.ObservedGeneration {
			return false
		}
	}
	return true
}

// sameCondition reports whether a and b are both absent, or carry the same
// status, reason, message and observedGeneration. LastTransitionTime is
// ignored.
func sameCondition(a, b *metav1.Condition) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Status == b.Status &&
		a.Reason == b.Reason &&
		a.Message == b.Message &&
		a.ObservedGeneration == b.ObservedGeneration
}

// maxConditionMessageBytes bounds validator output copied into a condition
// message or an event. The CRDs cap condition messages at 32768 characters,
// and krakend check can print far more for one bad policy used by many
// backends; the full output goes to the operator log instead.
const maxConditionMessageBytes = 4096

// truncationReserve is the room kept for the "(output truncated, N more
// lines)" marker.
const truncationReserve = 64

// truncateMessage returns msg unchanged when it fits in maxBytes. Otherwise
// it keeps as many whole leading lines as fit, cutting the first line at a
// rune boundary if even that one is too long, and appends a marker counting
// the lines it dropped.
func truncateMessage(msg string, maxBytes int) string {
	if len(msg) <= maxBytes {
		return msg
	}
	budget := maxBytes - truncationReserve
	lines := strings.Split(strings.TrimSuffix(msg, "\n"), "\n")
	var b strings.Builder
	kept := 0
	for _, line := range lines {
		if b.Len()+len(line)+1 > budget {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		kept++
	}
	if kept == 0 {
		b.WriteString(truncateAtRune(lines[0], budget))
		b.WriteByte('\n')
		kept = 1
	}
	fmt.Fprintf(&b, "(output truncated, %d more lines)", len(lines)-kept)
	return b.String()
}

// truncateAtRune returns the longest prefix of s that is at most n bytes and
// does not split a UTF-8 sequence.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// endpointIndexRegistration tracks one in-flight or completed index
// registration attempt for a specific field indexer.
type endpointIndexRegistration struct {
	ready chan struct{}
	err   error
}

// indexRegistry tracks which managers have had endpoint field indexes
// registered, scoped per-manager so multiple managers (e.g. in tests)
// each get their own registrations.
var indexRegistry sync.Map // map[client.FieldIndexer]*endpointIndexRegistration

// EnsureEndpointIndexes registers field indexes for KrakenDEndpoint lookups.
// It is safe to call from multiple controllers and the webhook package sharing
// the same manager; indexes are registered exactly once per manager instance.
func EnsureEndpointIndexes(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	reg := &endpointIndexRegistration{ready: make(chan struct{})}

	actual, loaded := indexRegistry.LoadOrStore(indexer, reg)
	if loaded {
		existing, ok := actual.(*endpointIndexRegistration)
		if !ok {
			return fmt.Errorf("unexpected type in index registry")
		}
		<-existing.ready
		return existing.err
	}

	defer close(reg.ready)
	reg.err = registerEndpointIndexes(indexer)
	return reg.err
}

func registerEndpointIndexes(indexer client.FieldIndexer) error {
	if err := indexer.IndexField(
		context.Background(), &v1alpha1.KrakenDEndpoint{}, EndpointGatewayIndex,
		func(obj client.Object) []string {
			ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
			if !ok {
				return nil
			}
			ns := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace)
			return []string{ns + "/" + ep.Spec.GatewayRef.Name}
		},
	); err != nil {
		return fmt.Errorf("indexing %s: %w", EndpointGatewayIndex, err)
	}

	if err := indexer.IndexField(
		context.Background(), &v1alpha1.KrakenDEndpoint{}, EndpointPolicyIndex,
		func(obj client.Object) []string {
			ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
			if !ok {
				return nil
			}
			var refs []string
			seen := make(map[string]struct{})
			for _, entry := range ep.Spec.Endpoints {
				for _, be := range entry.Backends {
					if be.PolicyRef == nil {
						continue
					}
					key := be.PolicyRef.PolicyKey(ep.Namespace)
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					refs = append(refs, key)
				}
			}
			return refs
		},
	); err != nil {
		return fmt.Errorf("indexing %s: %w", EndpointPolicyIndex, err)
	}

	return nil
}

// recordConditionTransition emits an event when next is a transition from
// prev. It emits a Warning when next is False and prev was absent or had
// another status or reason, and a Normal event when next is True and prev
// existed but was not True. Unchanged conditions, and a condition first set
// to True, emit nothing, so repeated reconciles do not repeat events.
func recordConditionTransition(
	recorder record.EventRecorder, obj runtime.Object, prev *metav1.Condition, next metav1.Condition,
) {
	switch {
	case next.Status == metav1.ConditionFalse &&
		(prev == nil || prev.Status != next.Status || prev.Reason != next.Reason):
		recorder.Event(obj, corev1.EventTypeWarning, next.Reason, next.Message)
	case next.Status == metav1.ConditionTrue && prev != nil && prev.Status != metav1.ConditionTrue:
		recorder.Event(obj, corev1.EventTypeNormal, next.Reason, next.Message)
	}
}
