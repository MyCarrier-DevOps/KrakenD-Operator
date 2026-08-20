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

package resources

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// strategicMergeSecurityContext merges user on top of base using a
// Kubernetes strategic-merge-patch (k8s.io/apimachinery/pkg/util/
// strategicpatch), instead of a hand-rolled field-by-field copy. base is
// marshaled to JSON as the "original"; user is marshaled to JSON (its
// `omitempty` tags mean an unset field is simply absent from the JSON) and
// applied as the "patch". A field the user never set stays absent from the
// patch and therefore keeps base's value; a field the user does set
// overrides it — matching the previous hand-rolled semantics, but now
// covering the FULL field set of T automatically. Unlike the previous
// per-field code, this survives future k8s API additions (new fields on
// SecurityContext/PodSecurityContext) with zero operator code changes: any
// field neither hand-picked-out here nor explicitly overridden by the user
// is preserved from base by construction, not by an exhaustive but
// hand-maintained if-chain that silently stops being exhaustive on the next
// k8s API bump.
//
// Nested pointer-to-struct fields (e.g. Capabilities) are also merged
// key-by-key rather than replaced wholesale: setting only
// `capabilities.add` leaves the base's `capabilities.drop` intact, since
// strategic-merge-patch recurses into nested objects and only replaces the
// sub-fields actually present in the patch.
//
// Known limitation — unset vs. empty on plain (non-patchMergeKey) list
// fields: because `user` is marshaled with `omitempty`, an unset list field
// and a deliberately-emptied list field (e.g.
// `capabilities: {drop: []}` to explicitly clear a default) are
// indistinguishable on the wire — both come out as "absent from the patch",
// so base's value always wins. This is currently safe only because
// Capabilities.Drop is the sole list field defaultPostRestartContainer
// SecurityContext/defaultPostRestartPodSecurityContext populate by default;
// a user cannot clear it via an empty override, but nothing today needs to.
// If a future hardened default populates another list field (e.g.
// Capabilities.Add, PodSecurityContext.Sysctls, or
// PodSecurityContext.SupplementalGroups), this limitation would invert:
// there would be no way for a caller to explicitly override that default
// back to empty, since {field: []} and an absent field are the same patch.
func strategicMergeSecurityContext[T corev1.SecurityContext | corev1.PodSecurityContext](base T, user *T) T {
	if user == nil {
		return base
	}
	baseJSON, err := json.Marshal(base)
	if err != nil {
		panic(fmt.Sprintf("marshaling security context default: %v", err))
	}
	userJSON, err := json.Marshal(user)
	if err != nil {
		panic(fmt.Sprintf("marshaling user-provided security context: %v", err))
	}
	mergedJSON, err := strategicpatch.StrategicMergePatch(baseJSON, userJSON, base)
	if err != nil {
		panic(fmt.Sprintf("strategic-merging security context: %v", err))
	}
	var merged T
	if err := json.Unmarshal(mergedJSON, &merged); err != nil {
		panic(fmt.Sprintf("unmarshaling merged security context: %v", err))
	}
	return merged
}

// userRequestsRootWithoutOptingOut reports whether a scope's own
// runAsUser/runAsNonRoot pair asks to run as root (runAsUser: 0) without
// also explicitly setting runAsNonRoot at that same scope — the exact
// shape every post-merge uid0 fixup in this file and in dragonfly.go
// inspects before dropping an inherited runAsNonRoot default. Extracted
// (fix-round review 2, T2 DRY) from mergePodSecurityContext,
// mergeDragonflyContainerSecurityContext, and mergeDragonflyPodSecurityContext,
// which each independently reimplemented this identical predicate against a
// different pair of fields; behavior at every call site is unchanged (see
// each site's own comment for why this predicate is the right condition
// there).
func userRequestsRootWithoutOptingOut(runAsUser *int64, runAsNonRoot *bool) bool {
	return runAsUser != nil && *runAsUser == 0 && runAsNonRoot == nil
}
