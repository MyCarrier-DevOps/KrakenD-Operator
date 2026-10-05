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
)

// deploymentObservation is what the Deployment step of the infrastructure
// stage saw. dep is the object CreateOrUpdate left behind: the API server's
// response after an update, or the cached object when nothing had to change.
// It is nil on a pass that did not reconcile the Deployment (held, or the
// step failed).
type deploymentObservation struct {
	dep *appsv1.Deployment
}

// rolloutNote is the reason and message a pass's change detection chose for
// the rollout it started.
type rolloutNote struct {
	reason, message string
}
