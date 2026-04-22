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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// BuildDeployment mutates dep in place with a complete Deployment for the
// KrakenD gateway. The image parameter is the resolved container image
// (from renderer.ResolveImage). configChecksum and pluginChecksum are
// injected as pod annotations to trigger rolling restarts on config changes.
func BuildDeployment(
	dep *appsv1.Deployment,
	gw *v1alpha1.KrakenDGateway,
	configChecksum string,
	pluginChecksum string,
	image string,
) {
	labels := StandardLabels(gw)
	selectorLabels := SelectorLabels(gw)

	dep.Labels = labels

	dep.Spec.Replicas = gw.Spec.Replicas
	dep.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: selectorLabels,
	}

	// Rolling update strategy: zero-downtime
	dep.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(1)),
			MaxUnavailable: ptr.To(intstr.FromInt32(0)),
		},
	}

	// Pod annotations for config change detection
	annotations := map[string]string{
		"krakend.io/checksum-config": configChecksum,
	}
	if pluginChecksum != "" {
		annotations["krakend.io/checksum-plugins"] = pluginChecksum
	}

	port := int32(8080)
	if gw.Spec.Config.Port != 0 {
		port = gw.Spec.Config.Port
	}
	healthPath := "/__health"
	if gw.Spec.Config.Router != nil && gw.Spec.Config.Router.HealthPath != "" {
		healthPath = gw.Spec.Config.Router.HealthPath
	}

	// Volumes and volume mounts
	volumes, volumeMounts, initContainers := buildVolumes(gw)

	// OpenAPI export init container + shared volume (so the sidecar can serve it)
	oaInit, oaSidecar, oaVolume, oaMountForExport := buildOpenAPIPieces(gw, image)
	if oaVolume != nil {
		volumes = append(volumes, *oaVolume)
	}
	if oaInit != nil {
		// The export init container needs the rendered config and writable /tmp.
		oaInit.VolumeMounts = append(oaInit.VolumeMounts,
			corev1.VolumeMount{
				Name:      "config",
				MountPath: "/etc/krakend/krakend.json",
				SubPath:   "krakend.json",
				ReadOnly:  true,
			},
			corev1.VolumeMount{
				Name:      "tmp",
				MountPath: "/tmp",
			},
		)
		// EE gateways need the license file for krakend to start/export.
		if gw.Spec.Edition == v1alpha1.EditionEE && gw.Spec.License != nil {
			oaInit.VolumeMounts = append(oaInit.VolumeMounts, corev1.VolumeMount{
				Name:      "license",
				MountPath: "/etc/krakend/LICENSE",
				SubPath:   "LICENSE",
				ReadOnly:  true,
			})
		}
		if oaMountForExport != nil {
			oaInit.VolumeMounts = append(oaInit.VolumeMounts, *oaMountForExport)
		}
		initContainers = append(initContainers, *oaInit)
	}

	// Main container
	container := corev1.Container{
		Name:  "krakend",
		Image: image,
		Command: []string{
			"/usr/bin/krakend",
			"run",
			"-c",
			"/etc/krakend/krakend.json",
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: volumeMounts,
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       5,
			FailureThreshold:    3,
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: healthPath,
					Port: intstr.FromInt32(port),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       3,
			FailureThreshold:    10,
		},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	if gw.Spec.Resources != nil {
		container.Resources = *gw.Spec.Resources
	}

	gracePeriod := int64(60)

	containers := []corev1.Container{container}
	if oaSidecar != nil {
		containers = append(containers, *oaSidecar)
	}

	dep.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            gw.Name,
			TerminationGracePeriodSeconds: &gracePeriod,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true),
				RunAsUser:    ptr.To(int64(1000)),
				RunAsGroup:   ptr.To(int64(1000)),
				FSGroup:      ptr.To(int64(1000)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			InitContainers: initContainers,
			Containers:     containers,
			Volumes:        volumes,
		},
	}
}

// buildVolumes assembles volumes, volume mounts, and init containers for the
// KrakenD deployment. Always mounts the ConfigMap and emptyDir /tmp. Adds
// license Secret if EE and plugin volumes if plugins are configured.
func buildVolumes(gw *v1alpha1.KrakenDGateway) (
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
	initContainers []corev1.Container,
) {
	// ConfigMap volume: krakend.json
	volumes = append(volumes, corev1.Volume{
		Name: "config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: gw.Name},
			},
		},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      "config",
		MountPath: "/etc/krakend/krakend.json",
		SubPath:   "krakend.json",
		ReadOnly:  true,
	})

	// emptyDir for /tmp (readOnlyRootFilesystem requires writable tmp)
	volumes = append(volumes, corev1.Volume{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      "tmp",
		MountPath: "/tmp",
	})

	// License Secret (EE only)
	if gw.Spec.Edition == v1alpha1.EditionEE && gw.Spec.License != nil {
		var licenseSecretName, licenseKey string
		if gw.Spec.License.SecretRef != nil {
			licenseSecretName = gw.Spec.License.SecretRef.Name
			licenseKey = gw.Spec.License.SecretRef.Key
		} else if gw.Spec.License.ExternalSecret.Enabled {
			// ExternalSecret convention: target Secret is {gw.Name}-license with key LICENSE
			licenseSecretName = gw.Name + "-license"
			licenseKey = "LICENSE"
		}
		if licenseSecretName != "" {
			volumes = append(volumes, corev1.Volume{
				Name: "license",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: licenseSecretName,
						Items: []corev1.KeyToPath{
							{
								Key:  licenseKey,
								Path: "LICENSE",
							},
						},
					},
				},
			})
			mounts = append(mounts, corev1.VolumeMount{
				Name:      "license",
				MountPath: "/etc/krakend/LICENSE",
				SubPath:   "LICENSE",
				ReadOnly:  true,
			})
		}
	}

	// Plugin volumes
	pv, pm, ic := buildPluginVolumes(gw)
	volumes = append(volumes, pv...)
	mounts = append(mounts, pm...)
	initContainers = append(initContainers, ic...)

	return volumes, mounts, initContainers
}

// buildPluginVolumes returns volumes, mounts, and init containers for
// plugin sources (ConfigMap, PVC, OCI image).
func buildPluginVolumes(
	gw *v1alpha1.KrakenDGateway,
) ([]corev1.Volume, []corev1.VolumeMount, []corev1.Container) {
	if gw.Spec.Plugins == nil || len(gw.Spec.Plugins.Sources) == 0 {
		return nil, nil, nil
	}

	sources := gw.Spec.Plugins.Sources
	var hasConfigMap, hasPVC, hasOCI bool
	for _, src := range sources {
		if src.ConfigMapRef != nil {
			hasConfigMap = true
		}
		if src.PersistentVolumeClaimRef != nil {
			hasPVC = true
		}
		if src.ImageRef != nil {
			hasOCI = true
		}
	}

	needsMultiSource := (hasConfigMap && hasPVC) ||
		(hasConfigMap && hasOCI) ||
		(hasPVC && hasOCI) ||
		hasOCI

	if needsMultiSource {
		return buildMultiSourcePluginVolumes(gw)
	}
	return buildSingleSourcePluginVolumes(gw)
}

// buildSingleSourcePluginVolumes handles the case where all plugins come
// from a single source type (all ConfigMap or all PVC).
func buildSingleSourcePluginVolumes(
	gw *v1alpha1.KrakenDGateway,
) ([]corev1.Volume, []corev1.VolumeMount, []corev1.Container) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	for i, src := range gw.Spec.Plugins.Sources {
		name := fmt.Sprintf("plugin-%d", i)
		if src.ConfigMapRef != nil {
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: src.ConfigMapRef.Name,
						},
					},
				},
			})
			mounts = append(mounts, corev1.VolumeMount{
				Name:      name,
				MountPath: fmt.Sprintf("/opt/krakend/plugins/%s", src.ConfigMapRef.Key),
				SubPath:   src.ConfigMapRef.Key,
				ReadOnly:  true,
			})
		}
		if src.PersistentVolumeClaimRef != nil {
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: src.PersistentVolumeClaimRef,
				},
			})
			mounts = append(mounts, corev1.VolumeMount{
				Name:      name,
				MountPath: "/opt/krakend/plugins",
				ReadOnly:  true,
			})
		}
	}
	return volumes, mounts, nil
}

// buildMultiSourcePluginVolumes handles mixed or OCI plugin sources.
// OCI images use init containers to copy plugins into a shared emptyDir.
func buildMultiSourcePluginVolumes(
	gw *v1alpha1.KrakenDGateway,
) ([]corev1.Volume, []corev1.VolumeMount, []corev1.Container) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	var initContainers []corev1.Container

	// Shared emptyDir for OCI plugin images
	volumes = append(volumes, corev1.Volume{
		Name:         "plugins",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      "plugins",
		MountPath: "/opt/krakend/plugins",
	})

	for i, src := range gw.Spec.Plugins.Sources {
		if src.ImageRef != nil {
			initContainers = append(initContainers, corev1.Container{
				Name:  fmt.Sprintf("plugin-init-%d", i),
				Image: src.ImageRef.Image,
				Command: []string{
					"cp", "-r", "/plugins/.", "/opt/krakend/plugins/",
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "plugins", MountPath: "/opt/krakend/plugins"},
				},
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			})
			if src.ImageRef.PullPolicy != "" {
				initContainers[len(initContainers)-1].ImagePullPolicy = src.ImageRef.PullPolicy
			}
		}
		if src.ConfigMapRef != nil {
			name := fmt.Sprintf("plugin-cm-%d", i)
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: src.ConfigMapRef.Name,
						},
					},
				},
			})
			// use init container to copy from ConfigMap to shared emptyDir
			initContainers = append(initContainers, corev1.Container{
				Name:    fmt.Sprintf("plugin-cm-init-%d", i),
				Image:   "busybox:1.37",
				Command: []string{"cp", fmt.Sprintf("/cm/%s", src.ConfigMapRef.Key), "/opt/krakend/plugins/"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: name, MountPath: "/cm", ReadOnly: true},
					{Name: "plugins", MountPath: "/opt/krakend/plugins"},
				},
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			})
		}
		if src.PersistentVolumeClaimRef != nil {
			name := fmt.Sprintf("plugin-pvc-%d", i)
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: src.PersistentVolumeClaimRef,
				},
			})
			// use init container to copy from PVC to shared emptyDir
			initContainers = append(initContainers, corev1.Container{
				Name:    fmt.Sprintf("plugin-pvc-init-%d", i),
				Image:   "busybox:1.37",
				Command: []string{"cp", "-r", "/pvc/.", "/opt/krakend/plugins/"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: name, MountPath: "/pvc", ReadOnly: true},
					{Name: "plugins", MountPath: "/opt/krakend/plugins"},
				},
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			})
		}
	}

	return volumes, mounts, initContainers
}

// buildOpenAPIPieces constructs the init container that exports the OpenAPI
// spec using the KrakenD binary, the sidecar that serves it, the shared
// emptyDir volume, and the mount applied to the init container. Returns
// all nil values when the OpenAPI export feature is disabled.
func buildOpenAPIPieces(
	gw *v1alpha1.KrakenDGateway,
	krakendImage string,
) (initContainer, sidecar *corev1.Container, volume *corev1.Volume, initMount *corev1.VolumeMount) {
	if gw.Spec.OpenAPI == nil || !gw.Spec.OpenAPI.Enabled {
		return nil, nil, nil, nil
	}

	oa := gw.Spec.OpenAPI

	volume = &corev1.Volume{
		Name:         "openapi",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}

	exportMount := corev1.VolumeMount{
		Name:      "openapi",
		MountPath: "/openapi",
	}
	initMount = &exportMount

	exportArgs := []string{
		"openapi", "export",
		"-c", "/etc/krakend/krakend.json",
		"-o", "/openapi/openapi.json",
	}
	if oa.Legacy {
		exportArgs = append(exportArgs, "--legacy")
	}
	if oa.SkipJSONSchema {
		exportArgs = append(exportArgs, "--skip-jsonschema")
	}
	if oa.Audience != "" {
		exportArgs = append(exportArgs, "--audience", oa.Audience)
	}

	initContainer = &corev1.Container{
		Name:    "openapi-export",
		Image:   krakendImage,
		Command: []string{"/usr/bin/krakend"},
		Args:    exportArgs,
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}
	if oa.Resources != nil {
		initContainer.Resources = *oa.Resources
	}

	sidecarImage := oa.SidecarImage
	if sidecarImage == "" {
		sidecarImage = "busybox:1.37"
	}
	oaPort := OpenAPIPort(gw)

	sidecar = &corev1.Container{
		Name:  "openapi-serve",
		Image: sidecarImage,
		Command: []string{
			"httpd", "-f", "-v",
			"-p", fmt.Sprintf("%d", oaPort),
			"-h", "/openapi",
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "openapi",
				ContainerPort: oaPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "openapi",
				MountPath: "/openapi",
				ReadOnly:  true,
			},
		},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}
	if oa.Resources != nil {
		sidecar.Resources = *oa.Resources
	}

	return initContainer, sidecar, volume, initMount
}
