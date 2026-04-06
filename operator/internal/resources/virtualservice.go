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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// VirtualServiceGVR returns the GroupVersionResource for the Istio VirtualService CRD.
func VirtualServiceGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "networking.istio.io",
		Version:  "v1",
		Resource: "virtualservices",
	}
}

// BuildVirtualService mutates the unstructured VirtualService in place from the gateway spec.
func BuildVirtualService(vs *unstructured.Unstructured, gw *v1alpha1.KrakenDGateway) {
	vs.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1",
		Kind:    "VirtualService",
	})
	vs.SetLabels(StandardLabels(gw))

	istio := gw.Spec.Istio
	vsSpec := map[string]interface{}{}

	if len(istio.Hosts) > 0 {
		hosts := make([]interface{}, 0, len(istio.Hosts))
		for _, h := range istio.Hosts {
			hosts = append(hosts, h)
		}
		vsSpec["hosts"] = hosts
	}

	if len(istio.Gateways) > 0 {
		gateways := make([]interface{}, 0, len(istio.Gateways))
		for _, g := range istio.Gateways {
			gateways = append(gateways, g)
		}
		vsSpec["gateways"] = gateways
	}

	port := int64(8080)
	if gw.Spec.Config.Port > 0 {
		port = int64(gw.Spec.Config.Port)
	}

	vsSpec["http"] = []interface{}{
		map[string]interface{}{
			"match": []interface{}{
				map[string]interface{}{
					"uri": map[string]interface{}{
						"prefix": "/",
					},
				},
			},
			"route": []interface{}{
				map[string]interface{}{
					"destination": map[string]interface{}{
						"host": fmt.Sprintf("%s.%s.svc.cluster.local", gw.Name, gw.Namespace),
						"port": map[string]interface{}{
							"number": port,
						},
					},
				},
			},
			"timeout": "30s",
		},
	}

	vs.Object["spec"] = vsSpec
}
