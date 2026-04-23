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
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestPostRestartJobName_StableAndShort(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw"}}
	n1 := PostRestartJobName(gw, "abcdef0123456789deadbeef")
	n2 := PostRestartJobName(gw, "fedcba0123456789cafebabe")
	if n1 == n2 {
		t.Fatalf("expected distinct names for different checksums")
	}
	if !strings.HasPrefix(n1, "gw-postrestart-abcdef012345") {
		t.Fatalf("unexpected job name: %s", n1)
	}
}

func TestPostRestartJobName_LongGatewayName(t *testing.T) {
	longName := strings.Repeat("a", 80)
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: longName}}
	name := PostRestartJobName(gw, "abcdef0123456789deadbeef")
	if len(name) > 63 {
		t.Fatalf("job name exceeds 63 chars: len=%d, name=%s", len(name), name)
	}
	if len(name) != 63 {
		t.Fatalf("expected exactly 63 chars for truncated name, got %d", len(name))
	}
	// Must still embed the checksum for uniqueness.
	if !strings.HasSuffix(name, "-abcdef012345") {
		t.Fatalf("truncated name lost checksum suffix: %s", name)
	}
}

func TestBuildPostRestartJob_Defaults(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo hello",
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "checksum1")

	if job.Annotations[PostRestartJobChecksumAnnotation] != "checksum1" {
		t.Fatalf("annotation missing")
	}
	if *job.Spec.BackoffLimit != 2 {
		t.Fatalf("default backoffLimit not applied: %d", *job.Spec.BackoffLimit)
	}
	if *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("default activeDeadline not applied")
	}
	if *job.Spec.TTLSecondsAfterFinished != 86400 {
		t.Fatalf("default TTL not applied")
	}
	if job.Spec.Template.Spec.ServiceAccountName != "gw" {
		t.Fatalf("expected default SA to match gateway name, got %q",
			job.Spec.Template.Spec.ServiceAccountName)
	}
	if job.Spec.Template.Spec.Containers[0].Image != DefaultPostRestartJobImage {
		t.Fatalf("expected default image %q", DefaultPostRestartJobImage)
	}
	cmd := job.Spec.Template.Spec.Containers[0].Command
	if len(cmd) < 3 || cmd[len(cmd)-1] != "echo hello" {
		t.Fatalf("script not passed to bash: %v", cmd)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Fatalf("restart policy not OnFailure")
	}
	if job.Spec.Template.Annotations[PostRestartJobChecksumAnnotation] != "checksum1" {
		t.Fatalf("pod annotation missing")
	}
}

func TestBuildPostRestartJob_CustomFields(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:                 true,
				Script:                  "do-work",
				Image:                   "custom:1",
				ServiceAccountName:      "custom-sa",
				Env:                     []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
				PodAnnotations:          map[string]string{"team": "payments"},
				BackoffLimit:            ptr.To(int32(5)),
				ActiveDeadlineSeconds:   ptr.To(int64(120)),
				TTLSecondsAfterFinished: ptr.To(int32(60)),
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs")

	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "custom:1" {
		t.Fatalf("custom image not applied")
	}
	if job.Spec.Template.Spec.ServiceAccountName != "custom-sa" {
		t.Fatalf("custom SA not applied")
	}
	if len(c.Env) != 1 || c.Env[0].Name != "FOO" {
		t.Fatalf("env not applied")
	}
	if job.Spec.Template.Annotations["team"] != "payments" {
		t.Fatalf("custom pod annotation missing")
	}
	if *job.Spec.BackoffLimit != 5 || *job.Spec.ActiveDeadlineSeconds != 120 ||
		*job.Spec.TTLSecondsAfterFinished != 60 {
		t.Fatalf("custom scheduling fields not applied")
	}
}

func TestBuildService_WithOpenAPIPort(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			OpenAPI: &v1alpha1.OpenAPIExportSpec{Enabled: true, Port: 9090},
		},
	}
	svc := &corev1.Service{}
	BuildService(svc, gw)

	var names []string
	for _, p := range svc.Spec.Ports {
		names = append(names, p.Name)
	}
	if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[1].Name != "openapi" ||
		svc.Spec.Ports[1].Port != 9090 {
		t.Fatalf("openapi port not exposed: %v", names)
	}
}

func TestBuildService_OpenAPIDisabled(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw"},
		Spec:       v1alpha1.KrakenDGatewaySpec{},
	}
	svc := &corev1.Service{}
	BuildService(svc, gw)
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected single http port, got %d", len(svc.Spec.Ports))
	}
}

func TestBuildDeployment_OpenAPIContainersAndVolume(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Edition: v1alpha1.EditionCE,
			Version: "2.13",
			OpenAPI: &v1alpha1.OpenAPIExportSpec{
				Enabled:        true,
				Audience:       "public",
				SkipJSONSchema: true,
			},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cksum", "", "krakend:2.13")

	if len(dep.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("expected krakend + openapi sidecar, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	var sawExport bool
	for _, ic := range dep.Spec.Template.Spec.InitContainers {
		if ic.Name == "openapi-export" {
			sawExport = true
			joined := strings.Join(ic.Args, " ")
			if !strings.Contains(joined, "--audience public") {
				t.Fatalf("audience flag missing: %v", ic.Args)
			}
			if !strings.Contains(joined, "--skip-jsonschema") {
				t.Fatalf("skip-jsonschema flag missing: %v", ic.Args)
			}
		}
	}
	if !sawExport {
		t.Fatalf("openapi-export init container missing")
	}
	var sawVolume bool
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "openapi" {
			sawVolume = true
		}
	}
	if !sawVolume {
		t.Fatalf("openapi volume missing")
	}
}

func TestBuildDeployment_OpenAPINoAudienceStripsConfig(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Edition: v1alpha1.EditionCE,
			Version: "2.13",
			OpenAPI: &v1alpha1.OpenAPIExportSpec{
				Enabled:        true,
				SkipJSONSchema: true,
			},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cksum", "", "krakend:2.13")

	var exportInit *corev1.Container
	for i := range dep.Spec.Template.Spec.InitContainers {
		if dep.Spec.Template.Spec.InitContainers[i].Name == "openapi-export" {
			exportInit = &dep.Spec.Template.Spec.InitContainers[i]
		}
	}
	if exportInit == nil {
		t.Fatal("openapi-export init container missing")
	}

	// When no audience is configured the container must use a shell
	// script that strips audience arrays before calling the export.
	if len(exportInit.Command) < 2 || exportInit.Command[0] != "sh" {
		t.Fatalf("expected sh -c wrapper, got command=%v", exportInit.Command)
	}
	script := exportInit.Command[2]
	if !strings.Contains(script, "sed") {
		t.Fatalf("expected sed in script, got: %s", script)
	}
	if !strings.Contains(script, "krakend-all.json") {
		t.Fatalf("expected stripped config path in script, got: %s", script)
	}
	if !strings.Contains(script, "--skip-jsonschema") {
		t.Fatalf("skip-jsonschema flag missing from script: %s", script)
	}
	if strings.Contains(script, "--audience") {
		t.Fatalf("audience flag should NOT be present when unset: %s", script)
	}
	if len(exportInit.Args) != 0 {
		t.Fatalf("expected no args when using sh -c, got %v", exportInit.Args)
	}
}

func TestBuildDeployment_OpenAPIEEMountsLicenseAndTmp(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Edition: v1alpha1.EditionEE,
			Version: "2.13",
			License: &v1alpha1.LicenseConfig{
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "lic-secret"},
					Key:                  "LICENSE",
				},
			},
			OpenAPI: &v1alpha1.OpenAPIExportSpec{Enabled: true},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cksum", "", "krakend-ee:2.13")

	var exportInit *corev1.Container
	for i := range dep.Spec.Template.Spec.InitContainers {
		if dep.Spec.Template.Spec.InitContainers[i].Name == "openapi-export" {
			exportInit = &dep.Spec.Template.Spec.InitContainers[i]
		}
	}
	if exportInit == nil {
		t.Fatal("openapi-export init container missing")
	}

	mountNames := map[string]bool{}
	for _, m := range exportInit.VolumeMounts {
		mountNames[m.Name] = true
	}
	if !mountNames["license"] {
		t.Error("expected license volume mount on openapi-export init container")
	}
	if !mountNames["tmp"] {
		t.Error("expected tmp volume mount on openapi-export init container")
	}
	if !mountNames["config"] {
		t.Error("expected config volume mount on openapi-export init container")
	}
}
