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

package resources

import (
	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// ConfigRevisionLabel marks a gateway's content-addressed config ConfigMaps;
// its value is the short checksum in the ConfigMap's name.
const ConfigRevisionLabel = "krakend.io/config-revision"

// ConfigKey is the ConfigMap key the gateway config is stored under.
const ConfigKey = "krakend.json"

// ConfigMapName returns the name of the immutable ConfigMap that holds the
// rendered config with the given checksum: "<gateway>-config-<10 hex>".
func ConfigMapName(_ *v1alpha1.KrakenDGateway, _ string) string { return "" }

// BuildConfigMap mutates cm in place with the rendered krakend.json data.
func BuildConfigMap(cm *corev1.ConfigMap, gw *v1alpha1.KrakenDGateway, jsonData []byte, _ string) {
	cm.Labels = StandardLabels(gw)
	cm.Data = map[string]string{
		"krakend.json": string(jsonData),
	}
}
