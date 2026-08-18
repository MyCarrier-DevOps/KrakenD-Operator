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

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	// DefaultPostRestartJobImage is the fallback image for the post-restart
	// Job container when the user does not override it.
	DefaultPostRestartJobImage = "bash:5.2"

	// PostRestartJobChecksumAnnotation records the checksum that triggered
	// the resource. On the Deployment pod template this is the rendered
	// krakend.json config checksum; on the post-restart Job's own
	// annotations this is the combined checksum returned by
	// PostRestartJobChecksum (config + postRestartJob spec). Used for
	// idempotent Job naming.
	PostRestartJobChecksumAnnotation = "krakend.io/checksum-config"

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

	// postRestartTmpVolumeName is the emptyDir volume backing /tmp so the
	// container can write there under readOnlyRootFilesystem: true.
	postRestartTmpVolumeName = "tmp"

	// postRestartTmpSizeLimit bounds the /tmp emptyDir so a runaway script
	// (e.g. a large openapi.json download) cannot exhaust node ephemeral
	// storage. Mirrors the deployment's /tmp volume (internal/resources/
	// deployment.go) but adds a limit since the Job's write pattern
	// (openapi export/upload) is less bounded than the gateway's own use.
	postRestartTmpSizeLimit = "256Mi"
)

// PostRestartJobName returns a deterministic Job name that embeds a short
// prefix of the config checksum, ensuring each unique config revision maps
// to exactly one Job. The result is capped at 63 characters so it remains
// valid as a Kubernetes label value (Job controllers mirror the name into
// pod labels).
func PostRestartJobName(gw *v1alpha1.KrakenDGateway, configChecksum string) string {
	short := configChecksum
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

// PostRestartJobChecksum combines the rendered krakend.json configChecksum
// with a checksum of the PostRestartJobSpec itself, so the Job's identity
// (see PostRestartJobName) changes whenever EITHER the gateway config OR the
// postRestartJob spec (script, securityContext, workingDir, ...) changes.
//
// Before this, the Job name/trigger was keyed on configChecksum alone: a
// spec-only edit (e.g. a script fix) was invisible to the reconciler until
// the next unrelated config change happened to roll a new checksum (nhig).
func PostRestartJobChecksum(spec *v1alpha1.PostRestartJobSpec, configChecksum string) (string, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshaling postRestartJob spec for checksum: %w", err)
	}
	combined := append([]byte(configChecksum), specJSON...)
	return hash.SHA256Hex(combined), nil
}

// BuildPostRestartJob mutates job in place with a complete Job definition
// that runs the user-provided bash script after the gateway has restarted.
// The checksum parameter is stamped onto the Job annotations so consumers
// can correlate the Job to a specific revision. Callers should pass the
// combined checksum from PostRestartJobChecksum (not the bare config
// checksum) so the Job's own annotation reflects the exact revision that
// produced it.
func BuildPostRestartJob(
	job *batchv1.Job,
	gw *v1alpha1.KrakenDGateway,
	configChecksum string,
) {
	spec := gw.Spec.PostRestartJob
	labels := StandardLabels(gw)
	labels["app.kubernetes.io/component"] = "post-restart-job"

	job.Labels = labels
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[PostRestartJobChecksumAnnotation] = configChecksum

	image := spec.Image
	if image == "" {
		image = DefaultPostRestartJobImage
	}

	saName := spec.ServiceAccountName
	if saName == "" {
		saName = gw.Name
	}

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

	podAnnotations := make(map[string]string, len(spec.PodAnnotations)+1)
	for k, v := range spec.PodAnnotations {
		podAnnotations[k] = v
	}
	// Set the reserved checksum annotation last so user-provided
	// annotations cannot overwrite it.
	podAnnotations[PostRestartJobChecksumAnnotation] = configChecksum

	cmd := spec.Command
	if len(cmd) == 0 {
		cmd = []string{"bash", "-c"}
	}

	workingDir := postRestartWorkingDir
	if spec.WorkingDir != "" {
		workingDir = spec.WorkingDir
	}

	container := corev1.Container{
		Name:            "post-restart",
		Image:           image,
		Command:         append(cmd, spec.Script),
		Env:             spec.Env,
		EnvFrom:         spec.EnvFrom,
		SecurityContext: mergeContainerSecurityContext(spec.SecurityContext),
		WorkingDir:      workingDir,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      postRestartTmpVolumeName,
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
						Name: postRestartTmpVolumeName,
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{
								SizeLimit: resourcePtr(postRestartTmpSizeLimit),
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
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range spec.PodLabels {
		merged[k] = v
	}
	return merged
}

// defaultPostRestartContainerSecurityContext returns the hardened container
// securityContext defaults applied when the user leaves a field unset.
// Mirrors the deployment's krakend container (internal/resources/
// deployment.go): readOnlyRootFilesystem + drop ALL capabilities + no
// privilege escalation.
func defaultPostRestartContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem:   ptr.To(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// mergeContainerSecurityContext merges a user-provided container
// SecurityContext on top of the hardened defaults, field by field. A field
// left unset (nil) by the user keeps the hardened default; a field the user
// explicitly sets overrides it. This replaces the previous verbatim
// replacement semantics, which meant a user setting e.g. runAsUser: 0 (as
// prod does, for `curl`/`rdme` needing root) silently discarded the
// hardened drop:ALL/ROFS/no-privilege-escalation defaults (8qln).
func mergeContainerSecurityContext(user *corev1.SecurityContext) *corev1.SecurityContext {
	merged := defaultPostRestartContainerSecurityContext()
	if user == nil {
		return merged
	}
	if user.Capabilities != nil {
		merged.Capabilities = user.Capabilities
	}
	if user.Privileged != nil {
		merged.Privileged = user.Privileged
	}
	if user.SELinuxOptions != nil {
		merged.SELinuxOptions = user.SELinuxOptions
	}
	if user.WindowsOptions != nil {
		merged.WindowsOptions = user.WindowsOptions
	}
	if user.RunAsUser != nil {
		merged.RunAsUser = user.RunAsUser
	}
	if user.RunAsGroup != nil {
		merged.RunAsGroup = user.RunAsGroup
	}
	if user.RunAsNonRoot != nil {
		merged.RunAsNonRoot = user.RunAsNonRoot
	}
	if user.ReadOnlyRootFilesystem != nil {
		merged.ReadOnlyRootFilesystem = user.ReadOnlyRootFilesystem
	}
	if user.AllowPrivilegeEscalation != nil {
		merged.AllowPrivilegeEscalation = user.AllowPrivilegeEscalation
	}
	if user.ProcMount != nil {
		merged.ProcMount = user.ProcMount
	}
	if user.SeccompProfile != nil {
		merged.SeccompProfile = user.SeccompProfile
	}
	if user.AppArmorProfile != nil {
		merged.AppArmorProfile = user.AppArmorProfile
	}
	return merged
}

// defaultPostRestartPodSecurityContext returns the hardened pod-level
// securityContext defaults applied when the user leaves a field unset.
func defaultPostRestartPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To(int64(1000)),
		RunAsGroup:   ptr.To(int64(1000)),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// mergePodSecurityContext merges a user-provided PodSecurityContext on top
// of the hardened defaults, field by field (see mergeContainerSecurityContext
// for rationale — 8qln). A prod override of runAsUser: 0 keeps
// runAsNonRoot's sibling defaults (runAsGroup, seccompProfile) intact unless
// the user also overrides them.
func mergePodSecurityContext(user *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	merged := defaultPostRestartPodSecurityContext()
	if user == nil {
		return merged
	}
	if user.SELinuxOptions != nil {
		merged.SELinuxOptions = user.SELinuxOptions
	}
	if user.WindowsOptions != nil {
		merged.WindowsOptions = user.WindowsOptions
	}
	if user.RunAsUser != nil {
		merged.RunAsUser = user.RunAsUser
	}
	if user.RunAsGroup != nil {
		merged.RunAsGroup = user.RunAsGroup
	}
	if user.RunAsNonRoot != nil {
		merged.RunAsNonRoot = user.RunAsNonRoot
	}
	if user.SupplementalGroups != nil {
		merged.SupplementalGroups = user.SupplementalGroups
	}
	if user.SupplementalGroupsPolicy != nil {
		merged.SupplementalGroupsPolicy = user.SupplementalGroupsPolicy
	}
	if user.FSGroup != nil {
		merged.FSGroup = user.FSGroup
	}
	if user.Sysctls != nil {
		merged.Sysctls = user.Sysctls
	}
	if user.FSGroupChangePolicy != nil {
		merged.FSGroupChangePolicy = user.FSGroupChangePolicy
	}
	if user.SeccompProfile != nil {
		merged.SeccompProfile = user.SeccompProfile
	}
	if user.AppArmorProfile != nil {
		merged.AppArmorProfile = user.AppArmorProfile
	}
	if user.SELinuxChangePolicy != nil {
		merged.SELinuxChangePolicy = user.SELinuxChangePolicy
	}
	return merged
}
