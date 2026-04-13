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
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func TestBuildDeployment_Minimal(t *testing.T) {
	gw := testGateway()
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "abc123", "", "krakend/krakend-ce:2.7.0")

	// Labels
	if dep.Labels["app.kubernetes.io/name"] != "krakend" {
		t.Error("expected standard labels on deployment")
	}

	// Selector
	if dep.Spec.Selector == nil {
		t.Fatal("expected selector")
	}
	if dep.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != "test-gw" {
		t.Error("expected selector labels")
	}

	// Strategy
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("expected RollingUpdate, got %s", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Strategy.RollingUpdate.MaxSurge.IntVal != 1 {
		t.Error("expected maxSurge 1")
	}
	if dep.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal != 0 {
		t.Error("expected maxUnavailable 0")
	}

	// Pod annotations
	ann := dep.Spec.Template.Annotations
	if ann["krakend.io/checksum-config"] != "abc123" {
		t.Errorf("expected config checksum annotation, got %q", ann["krakend.io/checksum-config"])
	}
	if _, ok := ann["krakend.io/checksum-plugins"]; ok {
		t.Error("should not have plugin checksum when empty")
	}

	// Container
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Name != "krakend" {
		t.Errorf("expected container name krakend, got %s", c.Name)
	}
	if c.Image != "krakend/krakend-ce:2.7.0" {
		t.Errorf("expected image krakend/krakend-ce:2.7.0, got %s", c.Image)
	}
	if c.Ports[0].ContainerPort != 8080 {
		t.Errorf("expected port 8080, got %d", c.Ports[0].ContainerPort)
	}

	// Command
	if len(c.Command) != 4 || c.Command[3] != "/etc/krakend/krakend.json" {
		t.Errorf("unexpected command: %v", c.Command)
	}

	// Health probes
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet.Path != "/__health" {
		t.Error("expected liveness probe on /__health")
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/__health" {
		t.Error("expected readiness probe on /__health")
	}
	if c.StartupProbe == nil || c.StartupProbe.HTTPGet.Path != "/__health" {
		t.Error("expected startup probe on /__health")
	}

	// Container security context
	if c.SecurityContext == nil {
		t.Fatal("expected security context")
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected readOnlyRootFilesystem")
	}
	if *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("expected no privilege escalation")
	}

	// Pod security context
	psc := dep.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("expected pod security context")
	}
	if !*psc.RunAsNonRoot {
		t.Error("expected runAsNonRoot")
	}
	if *psc.RunAsUser != 1000 {
		t.Errorf("expected runAsUser 1000, got %d", *psc.RunAsUser)
	}
	if *psc.RunAsGroup != 1000 {
		t.Errorf("expected runAsGroup 1000, got %d", *psc.RunAsGroup)
	}
	if *psc.FSGroup != 1000 {
		t.Errorf("expected fsGroup 1000, got %d", *psc.FSGroup)
	}

	// Termination grace period
	if *dep.Spec.Template.Spec.TerminationGracePeriodSeconds != 60 {
		t.Errorf("expected grace period 60, got %d", *dep.Spec.Template.Spec.TerminationGracePeriodSeconds)
	}

	// ServiceAccount
	if dep.Spec.Template.Spec.ServiceAccountName != "test-gw" {
		t.Errorf("expected SA test-gw, got %s", dep.Spec.Template.Spec.ServiceAccountName)
	}

	// Base volumes: config + tmp
	if len(dep.Spec.Template.Spec.Volumes) != 2 {
		t.Errorf("expected 2 base volumes, got %d", len(dep.Spec.Template.Spec.Volumes))
	}
	// No init containers
	if len(dep.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("expected no init containers, got %d", len(dep.Spec.Template.Spec.InitContainers))
	}
}

func TestBuildDeployment_PluginChecksum(t *testing.T) {
	gw := testGateway()
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "abc", "pluginhash", "img:latest")

	ann := dep.Spec.Template.Annotations
	if ann["krakend.io/checksum-plugins"] != "pluginhash" {
		t.Errorf("expected plugin checksum annotation, got %q", ann["krakend.io/checksum-plugins"])
	}
}

func TestBuildDeployment_CustomPortAndHealthPath(t *testing.T) {
	gw := testGateway()
	gw.Spec.Config.Port = 9090
	gw.Spec.Config.Router = &v1alpha1.RouterConfig{HealthPath: "/ready"}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Ports[0].ContainerPort != 9090 {
		t.Errorf("expected port 9090, got %d", c.Ports[0].ContainerPort)
	}
	if c.LivenessProbe.HTTPGet.Path != "/ready" {
		t.Errorf("expected health path /ready, got %s", c.LivenessProbe.HTTPGet.Path)
	}
	if c.LivenessProbe.HTTPGet.Port.IntVal != 9090 {
		t.Errorf("expected probe port 9090, got %d", c.LivenessProbe.HTTPGet.Port.IntVal)
	}
}

func TestBuildDeployment_WithResources(t *testing.T) {
	gw := testGateway()
	gw.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	c := dep.Spec.Template.Spec.Containers[0]
	if c.Resources.Requests.Cpu().String() != "250m" {
		t.Errorf("expected CPU request 250m, got %s", c.Resources.Requests.Cpu().String())
	}
	if c.Resources.Limits.Memory().String() != "512Mi" {
		t.Errorf("expected memory limit 512Mi, got %s", c.Resources.Limits.Memory().String())
	}
}

func TestBuildDeployment_WithReplicas(t *testing.T) {
	gw := testGateway()
	gw.Spec.Replicas = ptr.To(int32(3))
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	if *dep.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", *dep.Spec.Replicas)
	}
}

func TestBuildDeployment_EEWithLicense(t *testing.T) {
	gw := testGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.License = &v1alpha1.LicenseConfig{
		SecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "krakend-license"},
			Key:                  "license.key",
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "krakend/krakend-ee:2.7.0")

	// Should have config + tmp + license = 3 volumes
	vols := dep.Spec.Template.Spec.Volumes
	if len(vols) != 3 {
		t.Fatalf("expected 3 volumes (config+tmp+license), got %d", len(vols))
	}

	// License volume
	licVol := vols[2]
	if licVol.Name != "license" {
		t.Errorf("expected volume name 'license', got %s", licVol.Name)
	}
	if licVol.Secret.SecretName != "krakend-license" {
		t.Errorf("expected secret name krakend-license, got %s", licVol.Secret.SecretName)
	}
	if licVol.Secret.Items[0].Key != "license.key" {
		t.Errorf("expected key 'license.key', got %s", licVol.Secret.Items[0].Key)
	}

	// License mount
	mounts := dep.Spec.Template.Spec.Containers[0].VolumeMounts
	found := false
	for _, m := range mounts {
		if m.Name == "license" {
			found = true
			if m.MountPath != "/etc/krakend/LICENSE" {
				t.Errorf("expected mount at /etc/krakend/LICENSE, got %s", m.MountPath)
			}
			if m.SubPath != "LICENSE" {
				t.Errorf("expected subPath LICENSE, got %s", m.SubPath)
			}
		}
	}
	if !found {
		t.Error("expected license volume mount")
	}
}

func TestBuildDeployment_SingleSourceConfigMapPlugin(t *testing.T) {
	gw := testGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "my-plugin-cm", Key: "myplugin.so"}},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	// config + tmp + plugin-0 = 3
	vols := dep.Spec.Template.Spec.Volumes
	if len(vols) != 3 {
		t.Fatalf("expected 3 volumes, got %d", len(vols))
	}
	if vols[2].Name != "plugin-0" {
		t.Errorf("expected volume plugin-0, got %s", vols[2].Name)
	}
	if vols[2].ConfigMap.Name != "my-plugin-cm" {
		t.Errorf("expected ConfigMap my-plugin-cm, got %s", vols[2].ConfigMap.Name)
	}

	mounts := dep.Spec.Template.Spec.Containers[0].VolumeMounts
	found := false
	for _, m := range mounts {
		if m.Name == "plugin-0" {
			found = true
			if m.MountPath != "/opt/krakend/plugins/myplugin.so" {
				t.Errorf("expected mount at /opt/krakend/plugins/myplugin.so, got %s", m.MountPath)
			}
			if m.SubPath != "myplugin.so" {
				t.Errorf("expected subPath myplugin.so, got %s", m.SubPath)
			}
		}
	}
	if !found {
		t.Error("expected plugin configmap mount")
	}

	// No init containers for single-source ConfigMap
	if len(dep.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("expected no init containers, got %d", len(dep.Spec.Template.Spec.InitContainers))
	}
}

func TestBuildDeployment_SingleSourcePVC(t *testing.T) {
	gw := testGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{PersistentVolumeClaimRef: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "plugin-pvc"}},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	vols := dep.Spec.Template.Spec.Volumes
	if len(vols) != 3 {
		t.Fatalf("expected 3 volumes, got %d", len(vols))
	}
	if vols[2].PersistentVolumeClaim == nil {
		t.Fatal("expected PVC volume")
	}
	if vols[2].PersistentVolumeClaim.ClaimName != "plugin-pvc" {
		t.Errorf("expected claim plugin-pvc, got %s", vols[2].PersistentVolumeClaim.ClaimName)
	}
}

func TestBuildDeployment_MultiSourceOCI(t *testing.T) {
	gw := testGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ImageRef: &v1alpha1.OCIImageRef{Image: "registry/plugin:v1", PullPolicy: corev1.PullAlways}},
			{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "extra-cm", Key: "extra.so"}},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "phash", "img:v1")

	// Should use multi-source: emptyDir "plugins" + plugin-cm-1
	vols := dep.Spec.Template.Spec.Volumes
	foundPlugins := false
	foundCM := false
	for _, v := range vols {
		if v.Name == "plugins" {
			foundPlugins = true
			if v.EmptyDir == nil {
				t.Error("expected emptyDir for plugins volume")
			}
		}
		if v.Name == "plugin-cm-1" {
			foundCM = true
		}
	}
	if !foundPlugins {
		t.Error("expected shared plugins emptyDir volume")
	}
	if !foundCM {
		t.Error("expected plugin-cm-1 volume")
	}

	// Init containers: 1 for OCI + 1 for ConfigMap copy
	ics := dep.Spec.Template.Spec.InitContainers
	if len(ics) != 2 {
		t.Fatalf("expected 2 init containers, got %d", len(ics))
	}
	// First init: OCI plugin
	if ics[0].Name != "plugin-init-0" {
		t.Errorf("expected init container plugin-init-0, got %s", ics[0].Name)
	}
	if ics[0].Image != "registry/plugin:v1" {
		t.Errorf("expected OCI image, got %s", ics[0].Image)
	}
	if ics[0].ImagePullPolicy != corev1.PullAlways {
		t.Errorf("expected PullAlways, got %s", ics[0].ImagePullPolicy)
	}
	// Security context on init containers
	if ics[0].SecurityContext == nil || !*ics[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Error("expected readOnlyRootFilesystem on init container")
	}

	// Second init: ConfigMap copy
	if ics[1].Name != "plugin-cm-init-1" {
		t.Errorf("expected init container plugin-cm-init-1, got %s", ics[1].Name)
	}
	if ics[1].Image != "busybox:latest" {
		t.Errorf("expected busybox for CM copy, got %s", ics[1].Image)
	}
}

func TestBuildDeployment_MultiSourcePVC(t *testing.T) {
	gw := testGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ImageRef: &v1alpha1.OCIImageRef{Image: "plugin:v1"}},
			{PersistentVolumeClaimRef: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc1"}},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	ics := dep.Spec.Template.Spec.InitContainers
	if len(ics) != 2 {
		t.Fatalf("expected 2 init containers, got %d", len(ics))
	}
	if ics[1].Name != "plugin-pvc-init-1" {
		t.Errorf("expected pvc init container, got %s", ics[1].Name)
	}
}

func TestBuildDeployment_NoPlugins(t *testing.T) {
	gw := testGateway()
	gw.Spec.Plugins = nil
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	if len(dep.Spec.Template.Spec.Volumes) != 2 {
		t.Errorf("expected 2 base volumes with nil plugins, got %d", len(dep.Spec.Template.Spec.Volumes))
	}
}

func TestBuildDeployment_EmptyPluginSources(t *testing.T) {
	gw := testGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{}}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	if len(dep.Spec.Template.Spec.Volumes) != 2 {
		t.Errorf("expected 2 base volumes with empty sources, got %d", len(dep.Spec.Template.Spec.Volumes))
	}
}

func TestBuildDeployment_CENoLicenseVolume(t *testing.T) {
	gw := testGateway()
	gw.Spec.Edition = v1alpha1.EditionCE
	gw.Spec.License = &v1alpha1.LicenseConfig{
		SecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "should-not-mount"},
			Key:                  "key",
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cs", "", "img:v1")

	// CE should not mount license even if SecretRef is set
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "license" {
			t.Error("CE deployment should not have license volume")
		}
	}
}
