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
	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
)

// BuildHPA mutates hpa in place with an HPA for the gateway deployment.
func BuildHPA(hpa *autoscalingv2.HorizontalPodAutoscaler, gw *v1alpha1.KrakenDGateway) {
	hpa.Labels = StandardLabels(gw)

	as := gw.Spec.Autoscaling
	hpa.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       gw.Name,
	}
	hpa.Spec.MaxReplicas = as.MaxReplicas
	hpa.Spec.MinReplicas = as.MinReplicas

	if as.TargetCPU != nil {
		hpa.Spec.Metrics = []autoscalingv2.MetricSpec{
			{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: as.TargetCPU,
					},
				},
			},
		}
	}
}
