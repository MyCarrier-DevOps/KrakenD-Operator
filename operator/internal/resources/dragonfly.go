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
	"fmt"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DragonflyGVR returns the GroupVersionResource for the Dragonfly CRD.
func DragonflyGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "dragonflydb.io",
		Version:  "v1alpha1",
		Resource: "dragonflies",
	}
}

// DragonflyName returns the conventional name for a gateway's Dragonfly instance.
func DragonflyName(gw *v1alpha1.KrakenDGateway) string {
	return fmt.Sprintf("%s-dragonfly", gw.Name)
}

// DragonflyServiceDNS returns the in-cluster DNS for the Dragonfly service.
func DragonflyServiceDNS(gw *v1alpha1.KrakenDGateway) string {
	return fmt.Sprintf("%s-dragonfly.%s.svc.cluster.local:6379", gw.Name, gw.Namespace)
}

// BuildDragonfly mutates the unstructured Dragonfly CR in place from the gateway spec.
func BuildDragonfly(df *unstructured.Unstructured, gw *v1alpha1.KrakenDGateway) {
	df.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "dragonflydb.io",
		Version: "v1alpha1",
		Kind:    "Dragonfly",
	})
	df.SetLabels(DragonflyLabels(gw))

	spec := gw.Spec.Dragonfly
	dfSpec := map[string]interface{}{}

	if spec.Replicas != nil {
		dfSpec["replicas"] = int64(*spec.Replicas)
	}
	if spec.Image != "" {
		dfSpec["image"] = spec.Image
	}
	if spec.Resources != nil {
		dfSpec["resources"] = buildResourceRequirements(spec.Resources)
	}
	if spec.Snapshot != nil {
		snapshot := map[string]interface{}{}
		if spec.Snapshot.Cron != "" {
			snapshot["cron"] = spec.Snapshot.Cron
		}
		if spec.Snapshot.PersistentVolumeClaimSpec != nil {
			pvcSpec := map[string]interface{}{}
			modes := make([]interface{}, 0, len(spec.Snapshot.PersistentVolumeClaimSpec.AccessModes))
			for _, m := range spec.Snapshot.PersistentVolumeClaimSpec.AccessModes {
				modes = append(modes, string(m))
			}
			pvcSpec["accessModes"] = modes
			if spec.Snapshot.PersistentVolumeClaimSpec.Resources.Requests != nil {
				if storage, ok := spec.Snapshot.PersistentVolumeClaimSpec.Resources.Requests["storage"]; ok {
					pvcSpec["resources"] = map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": storage.String(),
						},
					}
				}
			}
			snapshot["persistentVolumeClaimSpec"] = pvcSpec
		}
		dfSpec["snapshot"] = snapshot
	}
	if spec.Authentication != nil && spec.Authentication.PasswordFromSecret != nil {
		dfSpec["authentication"] = map[string]interface{}{
			"passwordFromSecret": map[string]interface{}{
				"name": spec.Authentication.PasswordFromSecret.Name,
				"key":  spec.Authentication.PasswordFromSecret.Key,
			},
		}
	}
	if len(spec.Args) > 0 {
		args := make([]interface{}, 0, len(spec.Args))
		for _, a := range spec.Args {
			args = append(args, a)
		}
		dfSpec["args"] = args
	}

	podSecCtx := mergeDragonflyPodSecurityContext(spec.PodSecurityContext, spec.ContainerSecurityContext)
	containerSecCtx := mergeDragonflyContainerSecurityContext(spec.ContainerSecurityContext)
	dfSpec["podSecurityContext"] = buildPodSecurityContext(podSecCtx)
	dfSpec["containerSecurityContext"] = buildSecurityContext(containerSecCtx)

	df.Object["spec"] = dfSpec
}

// defaultDragonflyPodSecurityContext returns the hardened pod-level
// securityContext defaults applied when the user leaves a field unset. The
// Dragonfly image ships a built-in "dfly" user at uid/gid 999.
func defaultDragonflyPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: new(true),
		RunAsUser:    new(int64(999)),
		RunAsGroup:   new(int64(999)),
		// FSGroup replicates the upstream dragonfly-operator's built-in
		// fsGroup:999 default, which is nil-guarded and only applied when
		// spec.PodSecurityContext == nil. Since we set a non-nil
		// PodSecurityContext here, we must set FSGroup explicitly or fresh
		// PVCs (e.g. root-owned Azure disks) end up group-unwritable and
		// the uid999 process crashloops on --dir=/dragonfly/snapshots.
		FSGroup: new(int64(999)),
	}
}

// defaultDragonflyContainerSecurityContext returns the hardened
// container-level securityContext defaults applied when the user leaves a
// field unset, matching defaultDragonflyPodSecurityContext.
func defaultDragonflyContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot: new(true),
		RunAsUser:    new(int64(999)),
		RunAsGroup:   new(int64(999)),
	}
}

// mergeDragonflyPodSecurityContext merges a user-provided PodSecurityContext
// on top of the hardened defaults via strategicMergeSecurityContext (see
// job.go's mergeContainerSecurityContext for the general rationale — 8qln,
// mirrored here for the Dragonfly scope per docs/upgrade-guide.md item 7).
// Previously spec.dragonfly.podSecurityContext/containerSecurityContext were
// a full-replace: setting any single field (e.g. runAsUser) silently
// discarded the entire hardened default, including fsGroup: 999 — a live
// ownership hazard for PVC-backed instances (the uid/gid 999 "dfly" process
// loses group-write access to --dir=/dragonfly/snapshots and crashloops).
//
// Fix-round review 1 (asymmetry with job.go, see
// mergeDragonflyContainerSecurityContext's fixup): unlike
// job.go's defaultPostRestartContainerSecurityContext, which leaves runAs*
// UNSET at container scope, defaultDragonflyContainerSecurityContext PINS
// RunAsUser/RunAsGroup/RunAsNonRoot at container scope to match the dfly
// image's built-in uid. Because container-scope security-context fields
// always override pod-scope per-field at the kubelet, pod-scope
// runAsUser/runAsNonRoot NEVER change the Dragonfly container's effective
// uid — the container default's pin wins regardless. For that reason
// review-1 removed this function's pod-scope-unset self-heal fixup
// entirely: there was no real capability to preserve, only a spec the
// validator should reject outright (see validateDragonflyRunAsRoot).
//
// Fix-round review 2 (T1/R1 — restored, now CROSS-SCOPE AWARE): that
// removal was too broad. Grandfathered CRs can carry a uid0 request from
// before this merge-semantics change ever existed, and they reach
// BuildDragonfly WITHOUT ever passing through validateDragonflyRunAsRoot —
// either because the webhook's update-ratchet (runAsFieldsUnchanged)
// deliberately exempts an unchanged spec, or because the webhook is
// bypassed entirely (disabled, cert-manager absent, downtime — see
// docs/upgrade-guide.md item 7's webhook-bypass warning). On the PRE-merge
// main branch, a non-nil user PodSecurityContext fully REPLACED the
// default wholesale, so a user pod-scope uid0 request was rendered exactly
// as written — {runAsUser: 0}, no runAsNonRoot key at all — because the
// default runAsNonRoot: true was never in the picture to begin with. After
// the merge change, that default now merges in ALONGSIDE the user's uid0
// and re-introduces the kubelet-invalid {0, true} pair, via two distinct
// shapes:
//   - directly, when the USER's own PodSecurityContext sets RunAsUser: 0
//     (podUid0 below);
//   - indirectly, when the user's ContainerSecurityContext sets
//     RunAsUser: 0 with RunAsNonRoot left unset (containerUid0 below): the
//     container-scope fixup in mergeDragonflyContainerSecurityContext
//     clears the CONTAINER's own inherited runAsNonRoot default in that
//     case, and per-field kubelet resolution then falls back to whatever
//     the POD level renders for runAsNonRoot — which, without this fixup,
//     would be the newly-merged-in true default, reintroducing the exact
//     same broken pair one level up.
//
// This fixup restores the main-parity render for both of those shapes: it
// drops the inherited RunAsNonRoot default from the POD-level result too,
// so the pod-level pair never contradicts a uid0 request the user made at
// either scope. It is DELIBERATELY NOT applied when the user's own
// PodSecurityContext is nil (user == nil below): that legacy shape
// (container-scope uid0 with pod scope entirely unset) already rendered
// the broken {0, true-from-default} pair on main too — full-replace only
// protected a NON-nil user PodSecurityContext, since a nil one never
// participated in the replace and fell through to the hardcoded default
// either way. Parity there means leaving it exactly as broken as main
// left it, not silently granting root the fixup was never asked to grant.
// An explicit user RunAsNonRoot value at EITHER scope (true or false) is
// never overridden by this fixup — see the user.RunAsNonRoot == nil guard
// and the containerUid0 helper call (which itself requires the
// container's own RunAsNonRoot to be nil, not just the pod's).
func mergeDragonflyPodSecurityContext(
	user *corev1.PodSecurityContext, userContainer *corev1.SecurityContext,
) *corev1.PodSecurityContext {
	base := *defaultDragonflyPodSecurityContext()
	merged := strategicMergeSecurityContext(base, user)

	if user != nil && user.RunAsNonRoot == nil {
		var containerRunAsUser *int64
		var containerRunAsNonRoot *bool
		if userContainer != nil {
			containerRunAsUser = userContainer.RunAsUser
			containerRunAsNonRoot = userContainer.RunAsNonRoot
		}
		podUID0 := userRequestsRootWithoutOptingOut(user.RunAsUser, user.RunAsNonRoot)
		containerUID0 := userRequestsRootWithoutOptingOut(containerRunAsUser, containerRunAsNonRoot)
		if podUID0 || containerUID0 {
			merged.RunAsNonRoot = nil
		}
	}

	return &merged
}

// mergeDragonflyContainerSecurityContext merges a user-provided container
// SecurityContext on top of the hardened Dragonfly defaults via
// strategicMergeSecurityContext (see mergeDragonflyPodSecurityContext for
// rationale).
func mergeDragonflyContainerSecurityContext(user *corev1.SecurityContext) *corev1.SecurityContext {
	base := *defaultDragonflyContainerSecurityContext()
	merged := strategicMergeSecurityContext(base, user)

	// Fix-round review 1, BLOCKER: container-scope uid0 fixup. Unlike
	// job.go's postRestartJob container default (defaultPostRestart
	// ContainerSecurityContext), which leaves runAsUser/runAsNonRoot UNSET
	// at container scope (so pod scope governs), defaultDragonflyContainer
	// SecurityContext deliberately PINS RunAsNonRoot: true at container
	// scope. Kubelet resolves the EFFECTIVE {runAsUser, runAsNonRoot} pair
	// per-field, and a container-scope value always wins over pod-scope for
	// that same field — so an explicit containerSecurityContext.runAsUser: 0
	// always merges against the inherited container-scope RunAsNonRoot:
	// true default, no matter what the pod-scope runAsNonRoot says. Without
	// this fixup that always renders the kubelet-rejected {0, true} pair
	// (CreateContainerConfigError, pod Pending forever — no
	// activeDeadlineSeconds unlike the Job), INCLUDING grandfathered
	// pre-upgrade root specs the webhook ratchet deliberately admits (see
	// validateDragonflyRunAsRoot). If the user set RunAsUser: 0 without also
	// setting RunAsNonRoot, drop the inherited RunAsNonRoot default so the
	// user's pod-scope runAsNonRoot: false (or unset, defaulting to
	// kubelet's implicit false) governs via normal kubelet inheritance.
	if user != nil && userRequestsRootWithoutOptingOut(user.RunAsUser, user.RunAsNonRoot) {
		merged.RunAsNonRoot = nil
	}

	return &merged
}

// buildPodSecurityContext converts a corev1.PodSecurityContext into the
// unstructured map shape expected by the Dragonfly CRD's
// spec.podSecurityContext field.
func buildPodSecurityContext(sc *corev1.PodSecurityContext) map[string]interface{} {
	out := map[string]interface{}{}
	if sc.RunAsNonRoot != nil {
		out["runAsNonRoot"] = *sc.RunAsNonRoot
	}
	if sc.RunAsUser != nil {
		out["runAsUser"] = *sc.RunAsUser
	}
	if sc.RunAsGroup != nil {
		out["runAsGroup"] = *sc.RunAsGroup
	}
	if sc.FSGroup != nil {
		out["fsGroup"] = *sc.FSGroup
	}
	return out
}

// buildSecurityContext converts a corev1.SecurityContext into the
// unstructured map shape expected by the Dragonfly CRD's
// spec.containerSecurityContext field.
func buildSecurityContext(sc *corev1.SecurityContext) map[string]interface{} {
	out := map[string]interface{}{}
	if sc.RunAsNonRoot != nil {
		out["runAsNonRoot"] = *sc.RunAsNonRoot
	}
	if sc.RunAsUser != nil {
		out["runAsUser"] = *sc.RunAsUser
	}
	if sc.RunAsGroup != nil {
		out["runAsGroup"] = *sc.RunAsGroup
	}
	if sc.AllowPrivilegeEscalation != nil {
		out["allowPrivilegeEscalation"] = *sc.AllowPrivilegeEscalation
	}
	return out
}

func buildResourceRequirements(r *corev1.ResourceRequirements) map[string]interface{} {
	resources := map[string]interface{}{}
	if r.Requests != nil {
		requests := map[string]interface{}{}
		for k, v := range r.Requests {
			requests[string(k)] = v.String()
		}
		resources["requests"] = requests
	}
	if r.Limits != nil {
		limits := map[string]interface{}{}
		for k, v := range r.Limits {
			limits[string(k)] = v.String()
		}
		resources["limits"] = limits
	}
	return resources
}
