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
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
		},
	}

	if gw.Spec.Resources != nil {
		container.Resources = *gw.Spec.Resources
	}

	gracePeriod := int64(60)

	dep.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            gw.Name,
			TerminationGracePeriodSeconds: &gracePeriod,
			InitContainers:                initContainers,
			Containers:                    []corev1.Container{container},
			Volumes:                       volumes,
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
		MountPath: "/etc/krakend",
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
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
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
				Image:   "busybox:latest",
				Command: []string{"cp", fmt.Sprintf("/cm/%s", src.ConfigMapRef.Key), "/opt/krakend/plugins/"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: name, MountPath: "/cm", ReadOnly: true},
					{Name: "plugins", MountPath: "/opt/krakend/plugins"},
				},
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   ptr.To(true),
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
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
				Image:   "busybox:latest",
				Command: []string{"cp", "-r", "/pvc/.", "/opt/krakend/plugins/"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: name, MountPath: "/pvc", ReadOnly: true},
					{Name: "plugins", MountPath: "/opt/krakend/plugins"},
				},
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   ptr.To(true),
					RunAsNonRoot:             ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(false),
				},
			})
		}
	}

	return volumes, mounts, initContainers
}
