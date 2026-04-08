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

// Package resources provides pure-function builders that construct
// Kubernetes object specs from CRD state, following the
// controllerutil.CreateOrUpdate mutate-function pattern.
package resources

import (
	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// StandardLabels returns the standard set of labels applied to all
// managed KrakenD gateway resources.
func StandardLabels(gw *v1alpha1.KrakenDGateway) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "krakend",
		"app.kubernetes.io/instance":   gw.Name,
		"app.kubernetes.io/version":    gw.Spec.Version,
		"app.kubernetes.io/component":  "gateway",
		"app.kubernetes.io/part-of":    "krakend-operator",
		"app.kubernetes.io/managed-by": "krakend-operator",
	}
}

// SelectorLabels returns the minimal label set used in label selectors.
func SelectorLabels(gw *v1alpha1.KrakenDGateway) map[string]string {
	return map[string]string{
		"app.kubernetes.io/instance":   gw.Name,
		"app.kubernetes.io/managed-by": "krakend-operator",
	}
}

// DragonflyLabels returns labels for the Dragonfly CR, distinct from
// gateway labels to avoid incorrect name/component on the Dragonfly resource.
func DragonflyLabels(gw *v1alpha1.KrakenDGateway) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "dragonfly",
		"app.kubernetes.io/instance":   gw.Name + "-dragonfly",
		"app.kubernetes.io/part-of":    "krakend-operator",
		"app.kubernetes.io/managed-by": "krakend-operator",
	}
}
