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

package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSHA256Hex(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			name:  "empty input",
			input: []byte{},
			want:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:  "known input",
			input: []byte("hello"),
			want:  "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		},
		{
			name:  "deterministic for same input",
			input: []byte(`{"version":"3","endpoints":[]}`),
			want: func() string {
				h := sha256.Sum256([]byte(`{"version":"3","endpoints":[]}`))
				return hex.EncodeToString(h[:])
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SHA256Hex(tt.input)
			if got != tt.want {
				t.Errorf("SHA256Hex() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPluginChecksum_Deterministic(t *testing.T) {
	cm1 := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-plugins"},
		BinaryData: map[string][]byte{
			"plugin-b.so": []byte("binary-b"),
			"plugin-a.so": []byte("binary-a"),
		},
	}
	cm2 := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "beta-plugins"},
		BinaryData: map[string][]byte{
			"plugin-c.so": []byte("binary-c"),
		},
	}

	tags := []string{"registry.example.com/plugins:v2", "registry.example.com/plugins:v1"}

	// Order 1: cm2, cm1 with reversed tags
	result1 := PluginChecksum(
		[]corev1.ConfigMap{cm2, cm1},
		[]string{"registry.example.com/plugins:v2", "registry.example.com/plugins:v1"},
	)

	// Order 2: cm1, cm2 with original tags
	result2 := PluginChecksum(
		[]corev1.ConfigMap{cm1, cm2},
		tags,
	)

	if result1 != result2 {
		t.Errorf("PluginChecksum not deterministic: %q != %q", result1, result2)
	}
}

func TestPluginChecksum_EmptyInputs(t *testing.T) {
	result := PluginChecksum(nil, nil)
	if result == "" {
		t.Error("PluginChecksum(nil, nil) should return a valid hash, got empty string")
	}

	// Empty inputs should produce the hash of zero bytes written
	h := sha256.New()
	want := hex.EncodeToString(h.Sum(nil))
	if result != want {
		t.Errorf("PluginChecksum(nil, nil) = %q, want %q", result, want)
	}
}

func TestPluginChecksum_DifferentInputs(t *testing.T) {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "plugins"},
		BinaryData: map[string][]byte{
			"plugin.so": []byte("binary-data"),
		},
	}

	result1 := PluginChecksum([]corev1.ConfigMap{cm}, nil)
	result2 := PluginChecksum(nil, []string{"some:tag"})

	if result1 == result2 {
		t.Error("Different inputs should produce different checksums")
	}
}

func TestPluginChecksum_OnlyOCITags(t *testing.T) {
	tags := []string{"registry.example.com/plugin:v1.0.0"}
	result := PluginChecksum(nil, tags)
	if result == "" {
		t.Error("PluginChecksum with only OCI tags should return a valid hash")
	}
}

func TestPluginChecksum_OnlyConfigMaps(t *testing.T) {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "plugins"},
		BinaryData: map[string][]byte{
			"plugin.so": []byte("binary-data"),
		},
	}
	result := PluginChecksum([]corev1.ConfigMap{cm}, nil)
	if result == "" {
		t.Error("PluginChecksum with only ConfigMaps should return a valid hash")
	}
}
