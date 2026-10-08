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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// deploymentObservation is what the Deployment step of the infrastructure
// stage saw. dep is the object CreateOrUpdate left behind: the API server's
// response after an update, or the cached object when nothing had to change.
// It is nil on a pass that did not reconcile the Deployment (held, or the
// step failed).
type deploymentObservation struct {
	dep *appsv1.Deployment
	// failed: the Deployment step failed (a rejected write, or a stale object
	// that the API server answered with a Conflict). dep is nil like on a hold,
	// but the cache then describes the Deployment from before the write, so
	// nothing about the rollout can be judged from it.
	failed bool
	// refused: the step failed because the Deployment named like the gateway
	// is one the gateway does not control (refuseUncontrolled). failed is set
	// too.
	refused bool
	// unreconciled: the ServiceAccount step failed without a refusal, so the
	// Deployment step did not run. Unlike failed, the cache still describes the
	// Deployment as it is, so it is read like on a hold.
	unreconciled bool
	// created: this pass created the Deployment.
	created bool
	// templateChanged: this pass's write changed the pod template, judged by
	// comparing the template read before the write with the server's response.
	templateChanged bool
}

// rolloutNote is the reason and message a pass's change detection chose for
// the rollout it started.
type rolloutNote struct {
	reason, message string
}

// configRolloutNote is the note of the rollout a newly applied config starts.
func configRolloutNote() *rolloutNote {
	return &rolloutNote{reason: v1alpha1.ReasonConfigDeployed, message: "Configuration updated, rolling deployment"}
}

// rolloutInFlight reports whether the pass that reconciled the Deployment
// started, or sees, a rollout: it created the Deployment, its write changed
// the pod template, the template is not the wanted one, or old pods remain
// beside updated ones. A replica change alone is not a rollout, and neither is
// a generation the Deployment controller has not observed yet: a scale (an
// HPA's included) bumps the generation without touching the pod template.
//
// The Deployment CreateOrUpdate returns normally carries the wanted template
// already. Two edge cases are accepted. When the cache already matches the
// desired spec but its status is stale, the result is at most one extra
// Progressing=True pass, which the Owns watch corrects. And on a later pass,
// inside the few milliseconds before the Deployment controller observes a
// template write, the Deployment can read as converged; the Owns watch
// reconciles again on the observation.
func rolloutInFlight(obs deploymentObservation, want infraInputs) bool {
	dep := obs.dep
	return dep != nil && (obs.created || obs.templateChanged || !templateRunsWant(dep, want) ||
		dep.Status.UpdatedReplicas < dep.Status.Replicas)
}

// templateRunsWant reports whether dep's pod template carries the applied
// config, image, plugins and license, and mounts the applied config's
// ConfigMap. The first four are compared through annotations because
// admission can rewrite the container image.
func templateRunsWant(dep *appsv1.Deployment, want infraInputs) bool {
	annotations := dep.Spec.Template.Annotations
	return annotations[resources.PostRestartJobChecksumAnnotation] == want.appliedChecksum &&
		annotations[resources.PluginChecksumAnnotation] == want.pluginChecksum &&
		annotations[resources.ImageAnnotation] == want.image &&
		annotations[resources.LicenseChecksumAnnotation] == want.licenseChecksum &&
		resources.MountedConfigMapName(&dep.Spec.Template.Spec) == want.configMapName
}

// raiseProgressing reports a rollout in progress. The reason is the one this
// pass's detection chose; otherwise the reason of a rollout already reported;
// otherwise DeploymentUpdated.
func raiseProgressing(gw *v1alpha1.KrakenDGateway, note *rolloutNote) {
	reason, message := "DeploymentUpdated", "Deployment is rolling out a changed pod template"
	if cur := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionProgressing); condTrue(cur) {
		reason, message = cur.Reason, cur.Message
	}
	if note != nil {
		reason, message = note.reason, note.message
	}
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionProgressing,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// failedRolloutApplies reports whether a ProgressDeadlineExceeded on dep
// describes the rollout it is running now: the Deployment controller has
// observed the latest spec. The deadline condition outlives the rollout it
// judged until the controller starts the next one.
func failedRolloutApplies(dep *appsv1.Deployment) bool {
	c := findDeploymentCondition(dep, appsv1.DeploymentProgressing)
	return c != nil && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" &&
		dep.Status.ObservedGeneration >= dep.Generation
}

// resetRolloutFailedAvailability clears the Available=False/RolloutFailed an
// earlier pass wrote once the failure no longer describes the Deployment. It
// becomes True when the Deployment reports itself available, and is removed
// otherwise: a False Available outranks a rollout in progress in the derived
// Ready, so a rollout that replaced the failed one would read as an error.
func resetRolloutFailedAvailability(gw *v1alpha1.KrakenDGateway, dep *appsv1.Deployment) {
	cur := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionAvailable)
	if cur == nil || cur.Status != metav1.ConditionFalse || cur.Reason != v1alpha1.ReasonRolloutFailed {
		return
	}
	if depAvailable := findDeploymentCondition(dep, appsv1.DeploymentAvailable); depAvailable != nil &&
		depAvailable.Status == corev1.ConditionTrue {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             "DeploymentAvailable",
			Message:            "All replicas are available",
		})
		return
	}
	meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionAvailable)
}
