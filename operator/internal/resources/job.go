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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	// DefaultPostRestartJobImage is the fallback image for the post-restart
	// Job container when the user does not override it.
	DefaultPostRestartJobImage = "bash:5"

	// PostRestartJobChecksumAnnotation records the config checksum that
	// triggered the Job. Used for idempotent Job naming.
	PostRestartJobChecksumAnnotation = "krakend.io/checksum-config"

	defaultPostRestartBackoffLimit            = int32(2)
	defaultPostRestartActiveDeadlineSeconds   = int64(600)
	defaultPostRestartTTLSecondsAfterFinished = int32(86400)
)

// PostRestartJobName returns a deterministic Job name that embeds a short
// prefix of the config checksum, ensuring each unique config revision maps
// to exactly one Job.
func PostRestartJobName(gw *v1alpha1.KrakenDGateway, configChecksum string) string {
	short := configChecksum
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("%s-postrestart-%s", gw.Name, short)
}

// BuildPostRestartJob mutates job in place with a complete Job definition
// that runs the user-provided bash script after the gateway has restarted.
// The configChecksum parameter is stamped onto the Job annotations so
// consumers can correlate the Job to a specific config revision.
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

	podAnnotations := map[string]string{
		PostRestartJobChecksumAnnotation: configChecksum,
	}
	for k, v := range spec.PodAnnotations {
		podAnnotations[k] = v
	}

	container := corev1.Container{
		Name:    "post-restart",
		Image:   image,
		Command: []string{"/usr/bin/env", "bash", "-c", spec.Script},
		Env:     spec.Env,
		EnvFrom: spec.EnvFrom,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
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
				Labels:      labels,
				Annotations: podAnnotations,
			},
			Spec: corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyOnFailure,
				ServiceAccountName: saName,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: ptr.To(true),
					RunAsUser:    ptr.To(int64(1000)),
					RunAsGroup:   ptr.To(int64(1000)),
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeRuntimeDefault,
					},
				},
				Containers: []corev1.Container{container},
			},
		},
	}
}
