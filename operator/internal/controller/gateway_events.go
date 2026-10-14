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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// setConditionWithEvent sets cond on the gateway and records an event when it
// is a transition (recordConditionTransition): a Warning when it turns False
// or changes reason while False, and a Normal event when it turns True again.
// A steady state emits nothing.
func (r *KrakenDGatewayReconciler) setConditionWithEvent(gw *v1alpha1.KrakenDGateway, cond metav1.Condition) {
	// meta.SetStatusCondition updates the stored condition in place, so the
	// previous value is copied first. DeepCopy of a nil condition is nil.
	prev := meta.FindStatusCondition(gw.Status.Conditions, cond.Type).DeepCopy()
	meta.SetStatusCondition(&gw.Status.Conditions, cond)
	recordConditionTransition(r.Recorder, gw, prev, cond)
}

// setProblemCondition sets cond, a condition whose True status reports a
// problem (LicenseSecretUnavailable, LicenseDegraded, CEFallbackApplied). It
// records a Warning, with the condition's reason and message, when the
// condition turns True or changes reason while True.
func (r *KrakenDGatewayReconciler) setProblemCondition(gw *v1alpha1.KrakenDGateway, cond metav1.Condition) {
	prev := meta.FindStatusCondition(gw.Status.Conditions, cond.Type).DeepCopy()
	meta.SetStatusCondition(&gw.Status.Conditions, cond)
	if cond.Status == metav1.ConditionTrue &&
		(prev == nil || prev.Status != metav1.ConditionTrue || prev.Reason != cond.Reason) {
		r.Recorder.Event(gw, corev1.EventTypeWarning, cond.Reason, cond.Message)
	}
}
