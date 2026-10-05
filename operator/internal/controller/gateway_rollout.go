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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// deploymentObservation is what the Deployment step of the infrastructure
// stage saw. dep is the object CreateOrUpdate left behind: the API server's
// response after an update, or the cached object when nothing had to change.
// It is nil on a pass that did not reconcile the Deployment (held, or the
// step failed).
type deploymentObservation struct {
	dep *appsv1.Deployment
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

// rolloutInFlight reports whether the pass that reconciled the Deployment
// started a rollout.
func rolloutInFlight(obs deploymentObservation) bool {
	return obs.created || obs.templateChanged
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
