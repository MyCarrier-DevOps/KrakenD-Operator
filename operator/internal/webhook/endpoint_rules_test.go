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

package webhook

import (
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

func TestValidateEntries(t *testing.T) {
	tests := []struct {
		name   string
		gw     *v1alpha1.KrakenDGateway
		ep     *v1alpha1.KrakenDEndpoint
		reject string // "" means valid
	}{
		{"reserved health", testGateway(), testEndpoint("e", "/__health"), "reserved by KrakenD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := validateEntries(tt.ep, []int{0}, tt.gw)
			if tt.reject == "" {
				if len(errs) != 0 {
					t.Errorf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tt.reject) {
				t.Errorf("errors = %v, want %q", errs, tt.reject)
			}
		})
	}
}
