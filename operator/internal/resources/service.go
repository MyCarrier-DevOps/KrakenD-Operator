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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// BuildService mutates svc in place to expose the KrakenD gateway.
func BuildService(svc *corev1.Service, gw *v1alpha1.KrakenDGateway) {
	svc.Labels = StandardLabels(gw)
	svc.Spec.Selector = SelectorLabels(gw)
	svc.Spec.Type = corev1.ServiceTypeClusterIP

	port := int32(8080)
	if gw.Spec.Config.Port != 0 {
		port = gw.Spec.Config.Port
	}

	svc.Spec.Ports = []corev1.ServicePort{
		{
			Name:       "http",
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		},
	}
}
