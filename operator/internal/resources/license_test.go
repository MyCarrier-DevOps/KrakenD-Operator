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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestLicenseSecret(t *testing.T) {
	secretRef := func(name, key string) *corev1.SecretKeySelector {
		return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
	}
	cases := []struct {
		name              string
		license           *v1alpha1.LicenseConfig
		wantName, wantKey string
		wantOK            bool
	}{
		{name: "no license", license: nil},
		{name: "a license section naming no source", license: &v1alpha1.LicenseConfig{}},
		{
			name:     "a Secret reference",
			license:  &v1alpha1.LicenseConfig{SecretRef: secretRef("lic", "cert")},
			wantName: "lic", wantKey: "cert", wantOK: true,
		},
		{
			name:     "a Secret reference that names no key",
			license:  &v1alpha1.LicenseConfig{SecretRef: secretRef("lic", "")},
			wantName: "lic", wantKey: "LICENSE", wantOK: true,
		},
		{
			name:     "an enabled ExternalSecret",
			license:  &v1alpha1.LicenseConfig{ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{Enabled: true}},
			wantName: "gw-license", wantKey: "LICENSE", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"}}
			gw.Spec.License = tc.license

			name, key, ok := LicenseSecret(gw)

			if name != tc.wantName || key != tc.wantKey || ok != tc.wantOK {
				t.Errorf("LicenseSecret = (%q, %q, %v), want (%q, %q, %v)",
					name, key, ok, tc.wantName, tc.wantKey, tc.wantOK)
			}
		})
	}
}
