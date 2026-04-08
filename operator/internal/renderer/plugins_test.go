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
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildPluginBlock_NilPlugins(t *testing.T) {
	gw := minimalGateway()
	if block := buildPluginBlock(gw); block != nil {
		t.Errorf("expected nil, got %v", block)
	}
}

func TestBuildPluginBlock_EmptySources(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{Sources: []v1alpha1.PluginSource{}}
	if block := buildPluginBlock(gw); block != nil {
		t.Errorf("expected nil for empty sources, got %v", block)
	}
}

func TestBuildPluginBlock_WithSources(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ImageRef: &v1alpha1.OCIImageRef{Image: "myregistry.io/plugin:v1"}},
		},
	}
	block := buildPluginBlock(gw)
	if block == nil {
		t.Fatal("expected non-nil plugin block")
	}
	if block["pattern"] != ".so" {
		t.Errorf("expected pattern .so, got %v", block["pattern"])
	}
	if block["folder"] != "/opt/krakend/plugins/" {
		t.Errorf("expected folder /opt/krakend/plugins/, got %v", block["folder"])
	}
}

func TestComputePluginChecksum_NoPlugins(t *testing.T) {
	gw := minimalGateway()
	if cs := computePluginChecksum(gw, nil); cs != "" {
		t.Errorf("expected empty checksum, got %s", cs)
	}
}

func TestComputePluginChecksum_OCIOnly(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ImageRef: &v1alpha1.OCIImageRef{Image: "registry.io/plugin-b:v2"}},
			{ImageRef: &v1alpha1.OCIImageRef{Image: "registry.io/plugin-a:v1"}},
		},
	}
	cs := computePluginChecksum(gw, nil)
	if cs == "" {
		t.Fatal("expected non-empty checksum")
	}
}

func TestComputePluginChecksum_Deterministic(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ImageRef: &v1alpha1.OCIImageRef{Image: "registry.io/plugin-b:v2"}},
			{ImageRef: &v1alpha1.OCIImageRef{Image: "registry.io/plugin-a:v1"}},
		},
	}
	cms := []corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cm1"},
			Data:       map[string]string{"plugin.so": "binary-data"},
		},
	}
	cs1 := computePluginChecksum(gw, cms)
	cs2 := computePluginChecksum(gw, cms)
	if cs1 != cs2 {
		t.Error("expected deterministic checksum")
	}
}

func TestComputePluginChecksum_DifferentInputsDifferentChecksum(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Plugins = &v1alpha1.PluginsSpec{
		Sources: []v1alpha1.PluginSource{
			{ImageRef: &v1alpha1.OCIImageRef{Image: "registry.io/plugin:v1"}},
		},
	}
	cs1 := computePluginChecksum(gw, nil)

	gw.Spec.Plugins.Sources[0].ImageRef.Image = "registry.io/plugin:v2"
	cs2 := computePluginChecksum(gw, nil)

	if cs1 == cs2 {
		t.Error("expected different checksums for different images")
	}
}
