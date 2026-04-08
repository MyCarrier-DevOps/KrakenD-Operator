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

// ExternalSecretGVR returns the GroupVersionResource for the ExternalSecret CRD.
func ExternalSecretGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "external-secrets.io",
		Version:  "v1",
		Resource: "externalsecrets",
	}
}

// ExternalSecretName returns the conventional name for a gateway's license ExternalSecret.
func ExternalSecretName(gw *v1alpha1.KrakenDGateway) string {
	return fmt.Sprintf("%s-license", gw.Name)
}

// BuildExternalSecret mutates the unstructured ExternalSecret in place from the gateway spec.
func BuildExternalSecret(es *unstructured.Unstructured, gw *v1alpha1.KrakenDGateway) {
	es.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "external-secrets.io",
		Version: "v1",
		Kind:    "ExternalSecret",
	})
	es.SetLabels(StandardLabels(gw))

	lic := gw.Spec.License
	targetName := ExternalSecretName(gw)

	esSpec := map[string]interface{}{
		"refreshInterval": "1h",
		"secretStoreRef": map[string]interface{}{
			"name": lic.ExternalSecret.SecretStoreRef.Name,
			"kind": lic.ExternalSecret.SecretStoreRef.Kind,
		},
		"target": map[string]interface{}{
			"name":           targetName,
			"creationPolicy": "Owner",
			"template": map[string]interface{}{
				"type": "Opaque",
				"data": map[string]interface{}{
					"LICENSE": "{{ .license }}",
				},
			},
		},
		"data": []interface{}{
			map[string]interface{}{
				"secretKey": "license",
				"remoteRef": map[string]interface{}{
					"key":      lic.ExternalSecret.RemoteRef.Key,
					"property": lic.ExternalSecret.RemoteRef.Property,
				},
			},
		},
	}

	es.Object["spec"] = esSpec
}
