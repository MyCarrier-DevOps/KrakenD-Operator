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
	"fmt"
	"strings"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	// GatewayContainerName names the KrakenD container in the gateway pod.
	GatewayContainerName = "krakend"

	// PluginChecksumAnnotation records the plugin set a pod template was
	// built with; it is absent when the gateway has no plugins.
	PluginChecksumAnnotation = "krakend.io/checksum-plugins"

	// ImageAnnotation records the image the operator set on the gateway
	// container. The container's own image can be rewritten after admission
	// (digest pinning, registry mirrors), so rollout checks compare this
	// annotation instead.
	ImageAnnotation = "krakend.io/image"

	// LicenseChecksumAnnotation records the license the pod template was
	// built for. The license is mounted with subPath, which never receives
	// Secret updates, so a changed license has to change the pod template to
	// reach running pods. It tracks the mounted license, so it is present for
	// every EE gateway with a readable license, CE fallback or not, and absent
	// when no license is mounted.
	LicenseChecksumAnnotation = "krakend.io/checksum-license"
)

// desiredReplicas returns the replica count BuildDeployment writes. With
// autoscaling configured the HorizontalPodAutoscaler owns spec.replicas: an
// existing Deployment keeps its live value, and a new one starts at the
// HPA's floor, MinReplicas (1 when unset, as for the HPA itself).
func desiredReplicas(dep *appsv1.Deployment, gw *v1alpha1.KrakenDGateway) *int32 {
	if gw.Spec.Autoscaling == nil {
		return gw.Spec.Replicas
	}
	if dep.Spec.Replicas != nil {
		return dep.Spec.Replicas
	}
	return ptr.To(ptr.Deref(gw.Spec.Autoscaling.MinReplicas, 1))
}

// configVolumeName is the name of the volume holding the gateway config.
const configVolumeName = "config"

// DeploymentInputs is what BuildDeployment needs besides the gateway spec.
// Named fields, not positional strings: four strings transposed would
// compile silently.
type DeploymentInputs struct {
	// ConfigMapName is the content-addressed ConfigMap holding the applied
	// config (see ConfigMapName).
	ConfigMapName string
	// ConfigChecksum is the applied config's checksum. The pod template
	// records it in PostRestartJobChecksumAnnotation for the post-restart
	// Job gate.
	ConfigChecksum string
	PluginChecksum string
	Image          string
	// LicenseChecksum is the checksum of the license bytes mounted into an
	// EE gateway, whether or not it falls back to CE; "" when none is mounted.
	LicenseChecksum string
	// CERender: the applied config is a CE render — a CE-edition gateway's,
	// or an EE gateway's CE fallback — run with the CE image.
	CERender bool
}

// BuildDeployment mutates dep in place with a complete Deployment for the
// KrakenD gateway. in.Image is the container image the infrastructure
// stage deploys, that of the applied config's edition. in.ConfigChecksum and
// in.PluginChecksum are injected as pod annotations to trigger rolling
// restarts on config changes.
func BuildDeployment(dep *appsv1.Deployment, gw *v1alpha1.KrakenDGateway, in DeploymentInputs) {
	labels := StandardLabels(gw)
	selectorLabels := SelectorLabels(gw)

	dep.Labels = labels

	dep.Spec.Replicas = desiredReplicas(dep, gw)
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
		PostRestartJobChecksumAnnotation: in.ConfigChecksum,
		ImageAnnotation:                  in.Image,
	}
	if in.PluginChecksum != "" {
		annotations[PluginChecksumAnnotation] = in.PluginChecksum
	}
	if in.LicenseChecksum != "" {
		annotations[LicenseChecksumAnnotation] = in.LicenseChecksum
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
	volumes, volumeMounts, initContainers := buildVolumes(gw, in.ConfigMapName)

	// OpenAPI export init container + shared volume (so the sidecar can serve it)
	// OpenAPI export and serving are Enterprise features: the CE binary has
	// no `openapi` command, and a CE render has no documentation/openapi to
	// export. A pod running a CE render runs without them.
	var (
		oaInit, oaSidecar *corev1.Container
		oaVolume          *corev1.Volume
		oaMountForExport  *corev1.VolumeMount
	)
	if !in.CERender {
		oaInit, oaSidecar, oaVolume, oaMountForExport = buildOpenAPIPieces(gw, in.Image)
	}
	if oaVolume != nil {
		volumes = append(volumes, *oaVolume)
	}
	if oaInit != nil {
		// The export init container needs the rendered config and writable /tmp.
		oaInit.VolumeMounts = append(oaInit.VolumeMounts,
			corev1.VolumeMount{
				Name:      configVolumeName,
				MountPath: "/etc/krakend/krakend.json",
				SubPath:   ConfigKey,
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
		Name:  GatewayContainerName,
		Image: in.Image,
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
func buildVolumes(gw *v1alpha1.KrakenDGateway, configMapName string) (
	volumes []corev1.Volume,
	mounts []corev1.VolumeMount,
	initContainers []corev1.Container,
) {
	// ConfigMap volume: krakend.json
	volumes = append(volumes, corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			},
		},
	})
	mounts = append(mounts, corev1.VolumeMount{
		Name:      configVolumeName,
		MountPath: "/etc/krakend/krakend.json",
		SubPath:   ConfigKey,
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

// probeTimings names the four knobs of a shallow probe. Named fields, not
// positional int32 params: transposing period and timeout would compile
// silently.
type probeTimings struct {
	initialDelay, period, timeout, failures int32
}

// sidecarTCPProbe builds the operator's shallow TCP probe against the sidecar
// port. Both sidecar defaults share the handler wiring and differ only in
// timings, which stay visible at the call sites.
func sidecarTCPProbe(port int32, t probeTimings) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
		},
		InitialDelaySeconds: t.initialDelay,
		PeriodSeconds:       t.period,
		TimeoutSeconds:      t.timeout,
		FailureThreshold:    t.failures,
	}
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

	var command []string
	var args []string

	if oa.Audience != "" {
		// User specified an audience filter — pass it directly.
		exportArgs = append(exportArgs, "--audience", oa.Audience)
		command = []string{"/usr/bin/krakend"}
		args = exportArgs
	} else {
		// No audience filter — strip audience tags from a temp copy of the
		// config so krakend openapi export includes ALL endpoints. Without
		// this the CLI silently excludes every endpoint that declares an
		// audience array in documentation/openapi.
		var flags []string
		if oa.Legacy {
			flags = append(flags, "--legacy")
		}
		if oa.SkipJSONSchema {
			flags = append(flags, "--skip-jsonschema")
		}
		krakendCmd := "/usr/bin/krakend openapi export -c /openapi/krakend-all.json -o /openapi/openapi.json"
		if len(flags) > 0 {
			krakendCmd += " " + strings.Join(flags, " ")
		}
		script := fmt.Sprintf(
			`sed -E 's/,?"audience"\s*:\s*\[[^]]*\]//g; s/"audience"\s*:\s*\[[^]]*\]\s*,?//g' /etc/krakend/krakend.json > /openapi/krakend-all.json && %s && rm -f /openapi/krakend-all.json`,
			krakendCmd,
		)
		command = []string{"sh", "-c", script}
	}

	initContainer = &corev1.Container{
		Name:    "openapi-export",
		Image:   krakendImage,
		Command: command,
		Args:    args,
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

	sidecarImage := EffectiveOpenAPISidecarImage(oa)
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

	// DeepCopy, not the caller's pointer: the typed client decodes the API
	// server's response back into the object it submitted, and encoding/json
	// reuses non-nil pointer fields, so an aliased probe would have the server's
	// defaults written into gw.Spec in memory on every reconcile.
	if oa.ReadinessProbe != nil {
		sidecar.ReadinessProbe = oa.ReadinessProbe.DeepCopy()
	} else {
		sidecar.ReadinessProbe = sidecarTCPProbe(oaPort, probeTimings{
			initialDelay: 2, period: 10, timeout: 1, failures: 3,
		})
	}

	// Why a liveness probe here at all, given the handler is identical to the
	// readiness handler above and every threshold is slacker: it adds no
	// DETECTION -- anything failing this probe already failed readiness ~100s
	// earlier -- it adds the RECOVERY ACTION readiness structurally cannot.
	// That matters because openapi-serve is a plain entry in Containers, not a
	// native sidecar, so pod readiness ANDs across containers: a wedged
	// openapi-serve keeps the MAIN krakend container out of Service endpoints
	// indefinitely, with no path back. Liveness restarts it. It also satisfies
	// the require-liveness-probes ClusterPolicy, which is Enforce on some
	// clusters with no carve-out for this workload.
	//
	// Note this is a TCP connect, so it does NOT detect a process wedged in
	// userspace: the kernel completes the handshake from the listen backlog
	// whether or not httpd ever calls accept(). It catches a lost listener.
	//
	// The timings are ~5x slacker than the readiness DEFAULT. The kubelet acts on
	// the Nth CONSECUTIVE failure, so time-to-action is
	// initialDelay + (failureThreshold-1)*period + timeout: ~117s to restart here
	// vs ~23s to mark unready. That ordering is only
	// guaranteed while readiness is also left at its default -- a user-supplied
	// spec.openapi.readinessProbe slacker than ~135s is pre-empted by this.
	if oa.LivenessProbe != nil {
		sidecar.LivenessProbe = oa.LivenessProbe.DeepCopy()
	} else {
		sidecar.LivenessProbe = sidecarTCPProbe(oaPort, probeTimings{
			initialDelay: 15, period: 20, timeout: 2, failures: 6,
		})
	}

	return initContainer, sidecar, volume, initMount
}

// MountedConfigMapName returns the ConfigMap spec mounts as the gateway
// config, or "" when it mounts none.
func MountedConfigMapName(spec *corev1.PodSpec) string {
	for _, v := range spec.Volumes {
		if v.Name == configVolumeName && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}
