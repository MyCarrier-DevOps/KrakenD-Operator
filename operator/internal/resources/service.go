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

	port := GatewayPort(gw)

	ports := []corev1.ServicePort{
		{
			Name:       "http",
			Port:       port,
			TargetPort: intstr.FromInt32(port),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	if gw.Spec.OpenAPI != nil && gw.Spec.OpenAPI.Enabled {
		oaPort := OpenAPIPort(gw)
		ports = append(ports, corev1.ServicePort{
			Name:       "openapi",
			Port:       oaPort,
			TargetPort: intstr.FromInt32(oaPort),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	svc.Spec.Ports = ports
}

// GatewayPort returns the configured gateway listen port, defaulting to 8080.
func GatewayPort(gw *v1alpha1.KrakenDGateway) int32 {
	if gw.Spec.Config.Port != 0 {
		return gw.Spec.Config.Port
	}
	return 8080
}

// OpenAPIPort returns the configured OpenAPI sidecar port, defaulting to 8090.
func OpenAPIPort(gw *v1alpha1.KrakenDGateway) int32 {
	if gw.Spec.OpenAPI != nil && gw.Spec.OpenAPI.Port > 0 {
		return gw.Spec.OpenAPI.Port
	}
	return 8090
}
