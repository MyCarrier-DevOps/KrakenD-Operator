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

package renderer

import (
	"sort"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
	corev1 "k8s.io/api/core/v1"
)

// buildPluginBlock builds the "plugin" root key for krakend.json when
// plugins are configured. Returns nil if no plugins are configured.
func buildPluginBlock(gw *v1alpha1.KrakenDGateway) map[string]any {
	if gw.Spec.Plugins == nil || len(gw.Spec.Plugins.Sources) == 0 {
		return nil
	}
	return map[string]any{
		"pattern": ".so",
		"folder":  "/opt/krakend/plugins/",
	}
}

// computePluginChecksum produces a deterministic checksum over all plugin
// sources. ConfigMap data is hashed via the hash utility; OCI image refs
// contribute their image tag strings. PVC sources are excluded because
// their content is not available at render time.
func computePluginChecksum(
	gw *v1alpha1.KrakenDGateway,
	pluginConfigMaps []corev1.ConfigMap,
) string {
	if gw.Spec.Plugins == nil || len(gw.Spec.Plugins.Sources) == 0 {
		return ""
	}

	var ociTags []string
	for _, src := range gw.Spec.Plugins.Sources {
		if src.ImageRef != nil {
			ociTags = append(ociTags, src.ImageRef.Image)
		}
	}
	sort.Strings(ociTags)

	return hash.PluginChecksum(pluginConfigMaps, ociTags)
}
