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

package controller

import (
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestRenderEdition(t *testing.T) {
	cases := []struct {
		name       string
		edition    v1alpha1.Edition
		ceFallback bool
		want       v1alpha1.Edition
	}{
		{"CE gateway", v1alpha1.EditionCE, false, v1alpha1.EditionCE},
		{"EE gateway", v1alpha1.EditionEE, false, v1alpha1.EditionEE},
		{"EE gateway in CE fallback", v1alpha1.EditionEE, true, v1alpha1.EditionCE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := testGateway()
			gw.Spec.Edition = tc.edition
			if got := renderEdition(gw, tc.ceFallback); got != tc.want {
				t.Errorf("renderEdition = %s, want %s", got, tc.want)
			}
		})
	}
}
