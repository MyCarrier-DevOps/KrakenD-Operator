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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// DragonflyGVR returns the GroupVersionResource for the Dragonfly CRD.
func DragonflyGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "dragonflydb.io",
		Version:  "v1alpha1",
		Resource: "dragonflies",
	}
}

// DragonflyName returns the conventional name for a gateway's Dragonfly instance.
func DragonflyName(gw *v1alpha1.KrakenDGateway) string {
	return fmt.Sprintf("%s-dragonfly", gw.Name)
}

// DragonflyServiceDNS returns the in-cluster DNS for the Dragonfly service.
func DragonflyServiceDNS(gw *v1alpha1.KrakenDGateway) string {
	return fmt.Sprintf("%s-dragonfly.%s.svc.cluster.local:6379", gw.Name, gw.Namespace)
}

// BuildDragonfly mutates the unstructured Dragonfly CR in place from the gateway spec.
func BuildDragonfly(df *unstructured.Unstructured, gw *v1alpha1.KrakenDGateway) {
	df.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "dragonflydb.io",
		Version: "v1alpha1",
		Kind:    "Dragonfly",
	})
	df.SetLabels(DragonflyLabels(gw))

	spec := gw.Spec.Dragonfly
	dfSpec := map[string]interface{}{}

	if spec.Replicas != nil {
		dfSpec["replicas"] = int64(*spec.Replicas)
	}
	if spec.Image != "" {
		dfSpec["image"] = spec.Image
	}
	if spec.Resources != nil {
		dfSpec["resources"] = buildResourceRequirements(spec.Resources)
	}
	if spec.Snapshot != nil {
		snapshot := map[string]interface{}{}
		if spec.Snapshot.Cron != "" {
			snapshot["cron"] = spec.Snapshot.Cron
		}
		if spec.Snapshot.PersistentVolumeClaimSpec != nil {
			pvcSpec := map[string]interface{}{}
			modes := make([]interface{}, 0, len(spec.Snapshot.PersistentVolumeClaimSpec.AccessModes))
			for _, m := range spec.Snapshot.PersistentVolumeClaimSpec.AccessModes {
				modes = append(modes, string(m))
			}
			pvcSpec["accessModes"] = modes
			if spec.Snapshot.PersistentVolumeClaimSpec.Resources.Requests != nil {
				if storage, ok := spec.Snapshot.PersistentVolumeClaimSpec.Resources.Requests["storage"]; ok {
					pvcSpec["resources"] = map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": storage.String(),
						},
					}
				}
			}
			snapshot["persistentVolumeClaimSpec"] = pvcSpec
		}
		dfSpec["snapshot"] = snapshot
	}
	if spec.Authentication != nil && spec.Authentication.PasswordFromSecret != nil {
		dfSpec["authentication"] = map[string]interface{}{
			"passwordFromSecret": map[string]interface{}{
				"name": spec.Authentication.PasswordFromSecret.Name,
				"key":  spec.Authentication.PasswordFromSecret.Key,
			},
		}
	}
	if len(spec.Args) > 0 {
		args := make([]interface{}, 0, len(spec.Args))
		for _, a := range spec.Args {
			args = append(args, a)
		}
		dfSpec["args"] = args
	}

	df.Object["spec"] = dfSpec
}

func buildResourceRequirements(r *corev1.ResourceRequirements) map[string]interface{} {
	resources := map[string]interface{}{}
	if r.Requests != nil {
		requests := map[string]interface{}{}
		for k, v := range r.Requests {
			requests[string(k)] = v.String()
		}
		resources["requests"] = requests
	}
	if r.Limits != nil {
		limits := map[string]interface{}{}
		for k, v := range r.Limits {
			limits[string(k)] = v.String()
		}
		resources["limits"] = limits
	}
	return resources
}
