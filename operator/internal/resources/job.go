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
	"maps"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RULE (review id 3811443573, #4; scope corrected per review id 3812030505,
// round-5 #2, so its letter matches its spirit): any default value consumed
// by an effective* helper (effectivePostRestartCommand,
// effectivePostRestartImage, effectivePostRestartWorkingDir,
// effectivePostRestartServiceAccountName) is baked into
// postRestartJobProjection and therefore part of the post-restart Job's
// identity checksum (see PostRestartJobChecksum). TWO kinds of change mint a
// new checksum for every gateway that left the field unset — i.e. a
// fleet-wide re-trigger of the post-restart script on the next reconcile —
// and BOTH require a docs/upgrade-guide.md entry:
//  1. Changing the VALUE of an existing default (DefaultPostRestartJobImage,
//     the ["bash", "-c"] literal in effectivePostRestartCommand,
//     postRestartWorkingDir, or the gw.Name fallback in
//     effectivePostRestartServiceAccountName).
//  2. Switching a field's hash policy between RAW and EFFECTIVE — i.e.
//     whether "unset" and "explicitly set to the default" converge on one
//     checksum. This is exactly what the round-4 Image/ServiceAccountName
//     fix did (see postRestartJobProjection's field comments below and
//     docs/upgrade-guide.md item 10): no default VALUE changed, but it has
//     the same fleet-wide shape as #1 because it changes WHICH checksum an
//     unset field's gateway computes on its next reconcile.
//
// Any PR making either kind of change MUST add a docs/upgrade-guide.md entry
// calling it out — see the "v0.13.4 — postRestartJob hardening" section's
// item 3 (run-count semantics) and item 10 (a worked #2 example) for the
// shape such an entry takes.
const (
	// DefaultPostRestartJobImage is the fallback image for the post-restart
	// Job container when the user does not override it.
	DefaultPostRestartJobImage = "bash:5.2"

	// PostRestartJobChecksumAnnotation records the raw rendered krakend.json
	// config checksum (output.Checksum), unchanged in meaning across both
	// the Deployment pod template and the post-restart Job: it always
	// answers "which config revision produced this resource?" and is
	// therefore invertible/traceable back to a krakend.json revision.
	// deployment.go must use this const (not the literal string) so the two
	// call sites can never drift.
	PostRestartJobChecksumAnnotation = "krakend.io/checksum-config"

	// PostRestartJobCombinedChecksumAnnotation records the combined identity
	// checksum returned by PostRestartJobChecksum (a projection of the
	// execution-relevant config + postRestartJob spec fields). This is the
	// value Job naming/idempotency is keyed on; it is deliberately a
	// separate annotation from PostRestartJobChecksumAnnotation so the two
	// meanings ("what config produced this" vs "what identity gates
	// re-execution") never collide under one key.
	PostRestartJobCombinedChecksumAnnotation = "krakend.io/checksum-postrestart"

	defaultPostRestartBackoffLimit            = int32(2)
	defaultPostRestartActiveDeadlineSeconds   = int64(600)
	defaultPostRestartTTLSecondsAfterFinished = int32(86400)

	// postRestartWorkingDir is the default working directory: pods run as
	// runAsUser 1000 (or a user override, e.g. prod's runAsUser:0), so the
	// root-owned image root "/" needs a writable CWD for relative-path
	// writes. "/home/node" was rejected since bash:5.2 lacks it (would be
	// created root-owned). /tmp is backed by the "tmp" emptyDir volume
	// (see postRestartTmpVolume) so it stays writable under
	// readOnlyRootFilesystem. Overridable via spec.postRestartJob.workingDir.
	postRestartWorkingDir = "/tmp"

	// PostRestartTmpVolumeName is the emptyDir volume backing /tmp so the
	// container can write there under readOnlyRootFilesystem: true. Exported
	// so the controller can locate this volume on a built or existing Job
	// (e.g. to compare tmpSizeLimit between a desired and an existing failed
	// Job — see krakendgateway_controller.go's failure-signal re-create
	// gate).
	PostRestartTmpVolumeName = "tmp"

	// PostRestartTmpMountPath is the mount path of the /tmp emptyDir volume
	// backing the post-restart Job's default working directory. Exported
	// (mirrors postRestartWorkingDir) so GatewayValidator can warn when
	// spec.postRestartJob.workingDir is overridden to a path outside this
	// mount while readOnlyRootFilesystem is still effectively true — see
	// the docs/upgrade-guide.md ROFS/workingDir note.
	PostRestartTmpMountPath = postRestartWorkingDir

	// PostRestartContainerName is the name of the post-restart Job's single
	// container within its pod template. Exported so the controller can
	// locate this container's effective (post-merge) SecurityContext
	// directly off a BUILT Job (see krakendgateway_controller.go's
	// postRestartJobContainerSecurityContext / recordPostRestartJobROFSCondition,
	// review id 3807285633 #3d) rather than re-deriving it from the raw
	// user spec, which could drift from what mergeContainerSecurityContext
	// actually produced.
	PostRestartContainerName = "post-restart"

	// defaultPostRestartTmpSizeLimit bounds the /tmp emptyDir so a runaway
	// script (e.g. a large openapi.json download) cannot exhaust node
	// ephemeral storage. Mirrors the deployment's /tmp volume (internal/
	// resources/deployment.go) but adds a limit since the Job's write
	// pattern (openapi export/upload) is less bounded than the gateway's
	// own use. Overridable via spec.postRestartJob.tmpSizeLimit.
	defaultPostRestartTmpSizeLimit = "256Mi"
)

// PostRestartJobName returns a deterministic Job name that embeds a short
// prefix of the Job identity checksum (see PostRestartJobChecksum), ensuring
// each unique (config, postRestartJob-spec) revision maps to exactly one
// Job. The result is capped at 63 characters so it remains valid as a
// Kubernetes label value (Job controllers mirror the name into pod labels).
func PostRestartJobName(gw *v1alpha1.KrakenDGateway, checksum string) string {
	short := checksum
	if len(short) > 12 {
		short = short[:12]
	}
	// Fixed suffix: "-postrestart-" (13) + checksum (12) = 25 chars.
	const maxPrefix = 63 - 13 - 12 // 38
	prefix := gw.Name
	if len(prefix) > maxPrefix {
		prefix = prefix[:maxPrefix]
	}
	return fmt.Sprintf("%s-postrestart-%s", prefix, short)
}

// postRestartJobProjection is the execution-relevant subset of
// PostRestartJobSpec hashed by PostRestartJobChecksum. Deliberately NOT the
// full API type: purely cosmetic fields (PodLabels, PodAnnotations,
// TTLSecondsAfterFinished) never change what the script does when it runs,
// so editing them must not re-trigger the Job (a new checksum/name).
//
// BackoffLimit, ActiveDeadlineSeconds, and TmpSizeLimit are ALSO excluded
// here — but NOT because they're cosmetic like the fields above. Correction
// (review id 3805157426, #2): these three DO affect whether the script can
// actually complete (a too-short ActiveDeadlineSeconds, too-low
// BackoffLimit, or too-small TmpSizeLimit all produce a failure
// indistinguishable from a genuine script bug). They're excluded from the
// checksum/name so a knob-only edit never mints a brand-new Job under a new
// name for a Job that already SUCCEEDED (that would be the over-trigger
// this projection design supersedes a naive whole-spec hash to avoid) —
// but the controller's failure-signal gate (see
// krakendgateway_controller.go reconcileExistingPostRestartRevision)
// separately re-creates the SAME-named Job in place when one of these three
// knobs changes on a Job that FAILED, precisely because they can be the fix
// for that failure. Encoded via json.Marshal into a dedicated, versioned
// struct (not the live API type) so a cosmetic field reorder, json-tag
// rename, or new operational knob on PostRestartJobSpec cannot silently
// change the projection's hash — only a deliberate edit to this struct can.
//
// Direction-aware audit (review id 3811443558, #2; summary corrected per
// review id 3812030509, round-5 #3): every field below is hashed either RAW
// (verbatim from the user spec) or EFFECTIVE (normalized through the same
// effectivePostRestart* helper BuildPostRestartJob applies) — the choice is
// deliberate per field, recorded inline, and follows TWO distinct policies,
// not one:
//
//   - EFFECTIVE, for scalar fallback fields (Command, Image, WorkingDir,
//     ServiceAccountName): the builder-applied default is a single fixed
//     value the field converges TO, so hashing the post-default value makes
//     "unset" and "explicitly set to the documented default" produce the
//     same checksum.
//
//   - RAW, for merge-baseline (securityContext-shaped) fields
//     (SecurityContext, PodSecurityContext): these ALSO have a
//     builder-applied default — the hardened merge baseline applied by
//     mergeContainerSecurityContext/mergePodSecurityContext — so "RAW only
//     applies to a field with no builder-applied default" is false for
//     them. RAW is still the right choice here, for a different reason: the
//     merge baseline is not a fixed target value but an internal default
//     that can itself change in a future release (e.g. a new dropped
//     capability), and hashing the post-merge EFFECTIVE value would mint a
//     new checksum for every existing gateway fleet-wide when that
//     baseline changes. RAW-hashing means only the user's OWN spec edit
//     re-triggers. Accepted residual: a user who spells out exactly the
//     hardened baseline by hand gets a byte-identical container with a
//     DIFFERENT checksum than leaving the field unset (RAW sees two
//     distinct spec values where EFFECTIVE would have seen one).
//
// Guidance for placing a NEW field: if it has a fixed scalar default the
// builder substitutes when unset, hash it EFFECTIVE (scalar-fallback
// policy). If the builder instead MERGES the user's value with an internal,
// independently-evolving default rather than substituting a fixed value,
// hash it RAW (merge-baseline policy) and accept the same
// explicit-baseline-vs-unset residual documented above.
type postRestartJobProjection struct {
	// RAW: Script has no builder-applied default — the user always supplies
	// it (validated non-empty by the webhook).
	Script string `json:"script"`
	// EFFECTIVE: BuildPostRestartJob defaults an unset Command to
	// ["bash", "-c"]; hashing raw would diverge unset vs. explicit-default.
	Command []string `json:"command"`
	// EFFECTIVE: BuildPostRestartJob defaults an unset Image to
	// DefaultPostRestartJobImage; hashing raw would diverge unset vs.
	// explicit-default (the bug this round-4 fix addresses).
	Image string `json:"image"`
	// EFFECTIVE: BuildPostRestartJob defaults an unset WorkingDir to
	// postRestartWorkingDir; see effectivePostRestartCommand's doc for why.
	WorkingDir string `json:"workingDir"`
	// RAW: Env/EnvFrom have no builder-applied default — an unset slice is
	// rendered as an unset slice, so raw and effective coincide.
	Env     []corev1.EnvVar        `json:"env"`
	EnvFrom []corev1.EnvFromSource `json:"envFrom"`
	// RAW, DELIBERATELY: SecurityContext/PodSecurityContext ARE merged with
	// hardened defaults by BuildPostRestartJob (mergeContainerSecurityContext
	// / mergePodSecurityContext), so hashing the raw user override rather
	// than the post-merge effective value is a conscious choice, not an
	// oversight — normalizing to the merged value would mean any future
	// change to the hardened-default baseline itself (e.g. adding a new
	// dropped capability) mints a new checksum for every existing gateway
	// fleet-wide, re-running every post-restart script on the next
	// reconcile. Raw-hashing means only a user's OWN spec edit re-triggers.
	SecurityContext    *corev1.SecurityContext    `json:"securityContext"`
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext"`
	// EFFECTIVE: BuildPostRestartJob defaults an unset ServiceAccountName to
	// gw.Name; hashing raw would diverge unset vs. explicit-default (the bug
	// this round-4 fix addresses — see effectivePostRestartServiceAccountName).
	ServiceAccountName string `json:"serviceAccountName"`
	// RAW: Resources has no builder-applied default — BuildPostRestartJob
	// only sets container.Resources when spec.Resources is non-nil, leaving
	// it the zero value otherwise; raw and effective coincide.
	Resources *corev1.ResourceRequirements `json:"resources"`
}

// effectivePostRestartCommand returns the post-default Command that
// BuildPostRestartJob will actually put on the container: the user's
// spec.Command if set, otherwise the same ["bash", "-c"] default
// BuildPostRestartJob applies. Shared by BuildPostRestartJob and
// PostRestartJobChecksum's projection so "Command unset" and "Command set
// explicitly to the default" can never diverge between what's hashed and
// what's built (see postRestartJobProjection's WorkingDir/Command fields).
func effectivePostRestartCommand(spec *v1alpha1.PostRestartJobSpec) []string {
	if len(spec.Command) == 0 {
		return []string{"bash", "-c"}
	}
	return spec.Command
}

// effectivePostRestartWorkingDir returns the post-default WorkingDir that
// BuildPostRestartJob will actually put on the container: the user's
// spec.WorkingDir if set, otherwise the same postRestartWorkingDir default
// BuildPostRestartJob applies. Shared with PostRestartJobChecksum's
// projection for the same reason as effectivePostRestartCommand above.
func effectivePostRestartWorkingDir(spec *v1alpha1.PostRestartJobSpec) string {
	if spec.WorkingDir != "" {
		return spec.WorkingDir
	}
	return postRestartWorkingDir
}

// effectivePostRestartImage returns the post-default Image that
// BuildPostRestartJob will actually put on the container: the user's
// spec.Image if set, otherwise the same DefaultPostRestartJobImage default
// BuildPostRestartJob applies. Shared with PostRestartJobChecksum's
// projection for the same reason as effectivePostRestartCommand above
// (review id 3811443558, #2 — an explicit-default Image previously hashed
// differently from an omitted one, minting a spurious new checksum/Job
// name for a byte-identical rendered Job).
func effectivePostRestartImage(spec *v1alpha1.PostRestartJobSpec) string {
	if spec.Image != "" {
		return spec.Image
	}
	return DefaultPostRestartJobImage
}

// effectivePostRestartServiceAccountName returns the post-default
// ServiceAccountName that BuildPostRestartJob will actually put on the pod
// template: the user's spec.ServiceAccountName if set, otherwise the
// gateway-derived default (gw.Name) BuildPostRestartJob applies. Shared with
// PostRestartJobChecksum's projection for the same reason as
// effectivePostRestartCommand above (review id 3811443558, #2). Unlike the
// other effective* helpers this one needs gw, since its default is
// gateway-derived rather than a compile-time constant.
func effectivePostRestartServiceAccountName(spec *v1alpha1.PostRestartJobSpec, gw *v1alpha1.KrakenDGateway) string {
	if spec.ServiceAccountName != "" {
		return spec.ServiceAccountName
	}
	return gw.Name
}

// PostRestartJobChecksum combines the rendered krakend.json configChecksum
// with a checksum of the execution-relevant projection of the
// PostRestartJobSpec (see postRestartJobProjection), so the Job's identity
// (see PostRestartJobName) changes whenever EITHER the gateway config OR a
// field of the postRestartJob spec that actually affects script execution
// (script, command, image, workingDir, env, both securityContexts,
// serviceAccountName, resources) changes. Cosmetic fields
// (ttlSecondsAfterFinished, podAnnotations, podLabels) are excluded on
// purpose: editing them must never re-run the script. backoffLimit,
// activeDeadlineSeconds, and tmpSizeLimit are also excluded from this
// checksum (see postRestartJobProjection) despite being able to affect
// script completion — the controller's separate failure-signal gate, not
// this checksum, is what makes an edit to one of them retry a FAILED Job.
//
// Before this, the Job name/trigger was keyed on configChecksum alone: a
// spec-only edit (e.g. a script fix) was invisible to the reconciler until
// the next unrelated config change happened to roll a new checksum (nhig).
// A later revision keyed on json.Marshal of the entire spec instead, which
// over-corrected: any cosmetic edit (or operational-knob edit) also
// re-triggered the Job.
//
// The config checksum and the projection checksum are hashed as two
// separate, length-prefixed digests rather than naively concatenated bytes
// (see hash.CombineHex) so the combination is unambiguous regardless of the
// length or content of either input — no caller precondition about a fixed
// checksum length is required.
func PostRestartJobChecksum(
	spec *v1alpha1.PostRestartJobSpec, gw *v1alpha1.KrakenDGateway, configChecksum string,
) (string, error) {
	// Command, WorkingDir, Image, and ServiceAccountName are normalized to
	// their EFFECTIVE post-default values (via the effectivePostRestart*
	// helpers — the SAME default logic BuildPostRestartJob applies), not
	// hashed verbatim from the raw spec. Otherwise "unset" and "explicitly
	// set to the default" hash to two different checksums while
	// BuildPostRestartJob renders a byte-identical Job for both, causing a
	// spurious re-trigger (round-3 cleanup covered Command/WorkingDir;
	// round-4 review id 3811443558 #2 extended this to Image/
	// ServiceAccountName, which round-3 missed). See postRestartJobProjection
	// for the field-by-field raw-vs-effective audit.
	projection := postRestartJobProjection{
		Script:             spec.Script,
		Command:            effectivePostRestartCommand(spec),
		Image:              effectivePostRestartImage(spec),
		WorkingDir:         effectivePostRestartWorkingDir(spec),
		Env:                spec.Env,
		EnvFrom:            spec.EnvFrom,
		SecurityContext:    spec.SecurityContext,
		PodSecurityContext: spec.PodSecurityContext,
		ServiceAccountName: effectivePostRestartServiceAccountName(spec, gw),
		Resources:          spec.Resources,
	}
	projectionJSON, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("marshaling postRestartJob projection for checksum: %w", err)
	}
	return hash.CombineHex(configChecksum, hash.SHA256Hex(projectionJSON)), nil
}

// BuildPostRestartJob mutates job in place with a complete Job definition
// that runs the user-provided bash script after the gateway has restarted.
// configChecksum is the raw rendered krakend.json config checksum (stamped
// under PostRestartJobChecksumAnnotation, invertible/traceable back to a
// config revision); checksum is the combined Job-identity checksum from
// PostRestartJobChecksum (stamped under
// PostRestartJobCombinedChecksumAnnotation, drives naming/idempotency).
// Callers must pass both — see reconcilePostRestartJob.
func BuildPostRestartJob(
	job *batchv1.Job,
	gw *v1alpha1.KrakenDGateway,
	configChecksum string,
	checksum string,
) {
	spec := gw.Spec.PostRestartJob
	labels := StandardLabels(gw)
	labels["app.kubernetes.io/component"] = "post-restart-job"

	job.Labels = labels
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[PostRestartJobChecksumAnnotation] = configChecksum
	job.Annotations[PostRestartJobCombinedChecksumAnnotation] = checksum

	// Image and ServiceAccountName default resolution is delegated to the
	// same effective* helpers PostRestartJobChecksum's projection uses (see
	// review id 3811443558, #2), so the built Job and the checksum can never
	// disagree about what "unset" resolves to.
	image := effectivePostRestartImage(spec)
	saName := effectivePostRestartServiceAccountName(spec, gw)

	backoffLimit := defaultPostRestartBackoffLimit
	if spec.BackoffLimit != nil {
		backoffLimit = *spec.BackoffLimit
	}
	activeDeadline := defaultPostRestartActiveDeadlineSeconds
	if spec.ActiveDeadlineSeconds != nil {
		activeDeadline = *spec.ActiveDeadlineSeconds
	}
	ttl := defaultPostRestartTTLSecondsAfterFinished
	if spec.TTLSecondsAfterFinished != nil {
		ttl = *spec.TTLSecondsAfterFinished
	}
	tmpSizeLimit := resourcePtr(defaultPostRestartTmpSizeLimit)
	if spec.TmpSizeLimit != nil {
		tmpSizeLimit = spec.TmpSizeLimit
	}

	podAnnotations := make(map[string]string, len(spec.PodAnnotations)+2)
	maps.Copy(podAnnotations, spec.PodAnnotations)
	// Set the reserved checksum annotations last so user-provided
	// annotations cannot overwrite them.
	podAnnotations[PostRestartJobChecksumAnnotation] = configChecksum
	podAnnotations[PostRestartJobCombinedChecksumAnnotation] = checksum

	cmd := effectivePostRestartCommand(spec)
	workingDir := effectivePostRestartWorkingDir(spec)

	container := corev1.Container{
		Name:            PostRestartContainerName,
		Image:           image,
		Command:         append(cmd, spec.Script),
		Env:             spec.Env,
		EnvFrom:         spec.EnvFrom,
		SecurityContext: mergeContainerSecurityContext(spec.SecurityContext),
		WorkingDir:      workingDir,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      PostRestartTmpVolumeName,
				MountPath: postRestartWorkingDir,
			},
		},
	}
	if spec.Resources != nil {
		container.Resources = *spec.Resources
	}

	job.Spec = batchv1.JobSpec{
		BackoffLimit:            &backoffLimit,
		ActiveDeadlineSeconds:   &activeDeadline,
		TTLSecondsAfterFinished: &ttl,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      podLabels(labels, spec),
				Annotations: podAnnotations,
			},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyOnFailure,
				ServiceAccountName: saName,
				SecurityContext:    mergePodSecurityContext(spec.PodSecurityContext),
				Containers:         []corev1.Container{container},
				Volumes: []corev1.Volume{
					{
						Name: PostRestartTmpVolumeName,
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{
								SizeLimit: tmpSizeLimit,
							},
						},
					},
				},
			},
		},
	}
}

// resourcePtr parses a resource.Quantity string (e.g. "256Mi") and returns a
// pointer to it. Panics on an invalid literal since the input is always an
// internal compile-time constant, never user input.
func resourcePtr(qty string) *resource.Quantity {
	q := resource.MustParse(qty)
	return &q
}

// podLabels merges user-provided pod labels on top of the standard labels.
func podLabels(base map[string]string, spec *v1alpha1.PostRestartJobSpec) map[string]string {
	if len(spec.PodLabels) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(spec.PodLabels))
	maps.Copy(merged, base)
	maps.Copy(merged, spec.PodLabels)
	return merged
}

// defaultPostRestartContainerSecurityContext returns the hardened container
// securityContext defaults applied when the user leaves a field unset.
// Mirrors the deployment's krakend container (internal/resources/
// deployment.go): readOnlyRootFilesystem + drop ALL capabilities + no
// privilege escalation.
//
// WARNING (security footgun, drop-list override): strategicMergeSecurityContext
// merges nested objects key-by-key, but Capabilities.Drop is itself a plain
// (non-patchMergeKey) list, so a user who explicitly sets
// securityContext.capabilities.drop (e.g. ["NET_ADMIN"]) has that list
// REPLACE this Drop: ["ALL"] baseline wholesale — it does not union with it.
// The hardened default is silently discarded the moment a user's spec sets
// any non-empty capabilities.drop of their own. This is intentional,
// documented behavior (not special-cased/merged — see docs/upgrade-guide.md);
// a user who wants to keep the ALL baseline while adding their own drops
// must include "ALL" in their own list.
func defaultPostRestartContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// strategicMergeSecurityContext and userRequestsRootWithoutOptingOut live in
// internal/resources/securitycontext.go (shared by this file's
// mergeContainerSecurityContext/mergePodSecurityContext and dragonfly.go's
// mergeDragonflyContainerSecurityContext/mergeDragonflyPodSecurityContext).

// mergeContainerSecurityContext merges a user-provided container
// SecurityContext on top of the hardened defaults via
// strategicMergeSecurityContext. This replaces the previous verbatim
// replacement semantics, which meant a user setting e.g. runAsUser: 0 (as
// prod does, for `curl`/`rdme` needing root) silently discarded the
// hardened drop:ALL/ROFS/no-privilege-escalation defaults (8qln).
func mergeContainerSecurityContext(user *corev1.SecurityContext) *corev1.SecurityContext {
	base := *defaultPostRestartContainerSecurityContext()
	merged := strategicMergeSecurityContext(base, user)
	return &merged
}

// defaultPostRestartPodSecurityContext returns the hardened pod-level
// securityContext defaults applied when the user leaves a field unset.
func defaultPostRestartPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: new(true),
		RunAsUser:    new(int64(1000)),
		RunAsGroup:   new(int64(1000)),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// mergePodSecurityContext merges a user-provided PodSecurityContext on top
// of the hardened defaults via strategicMergeSecurityContext (see
// mergeContainerSecurityContext for rationale — 8qln). A prod override of
// runAsUser: 0 keeps runAsNonRoot's sibling defaults (runAsGroup,
// seccompProfile) intact unless the user also overrides them.
//
// Cross-reference (fix-round review 1, change #4): this pod-scope fixup is
// the RIGHT scope for the postRestartJob Job, because
// defaultPostRestartContainerSecurityContext leaves runAsUser/runAsNonRoot
// UNSET at container scope — pod scope genuinely governs the effective uid,
// so a pod-scope self-heal has a real capability to preserve. Dragonfly's
// analogous merge helpers (internal/resources/dragonfly.go) are
// deliberately asymmetric: mergeDragonflyContainerSecurityContext carries
// the fixup instead, because dragonfly's container default PINS
// RunAsNonRoot (container scope always wins over pod scope per-field at
// the kubelet). This function's own runtime behavior is unchanged by that
// fix-round.
func mergePodSecurityContext(user *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	base := *defaultPostRestartPodSecurityContext()
	merged := strategicMergeSecurityContext(base, user)

	// #1 (review id 3804144382, important/security): an explicit
	// podSecurityContext.runAsUser: 0 contradicts the inherited
	// runAsNonRoot:true default. Kubelet validates the pair together at
	// container start and refuses to start the container
	// (CreateContainerConfigError) when they conflict — but that failure
	// mode never fails the pod outright: it sits Waiting/Pending until
	// activeDeadlineSeconds (600s default) expires, silently. This is a
	// cross-field consistency rule the k8s API/strategic-merge-patch
	// cannot express structurally (RunAsUser and RunAsNonRoot are
	// independent fields), so it must be handled explicitly, after the
	// merge: if the user set RunAsUser to 0 without also setting
	// RunAsNonRoot, drop the inherited RunAsNonRoot default so only the
	// user's own explicit choice (if any) can re-assert it.
	if user != nil && userRequestsRootWithoutOptingOut(user.RunAsUser, user.RunAsNonRoot) {
		merged.RunAsNonRoot = nil
	}

	return &merged
}
