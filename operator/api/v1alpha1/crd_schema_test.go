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

package v1alpha1

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// crdFiles maps each generated CRD manifest to whether the kind has a phase.
var crdFiles = map[string]bool{
	"gateway.krakend.io_krakendgateways.yaml":        true,
	"gateway.krakend.io_krakendendpoints.yaml":       true,
	"gateway.krakend.io_krakendautoconfigs.yaml":     true,
	"gateway.krakend.io_krakendbackendpolicies.yaml": false,
}

// loadCRD reads a generated CRD manifest from config/crd/bases.
func loadCRD(t *testing.T, file string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", file))
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decoding %s: %v", file, err)
	}
	return &crd
}

func TestCRDs_StatusConditionsAreAMapKeyedByType(t *testing.T) {
	for file := range crdFiles {
		t.Run(file, func(t *testing.T) {
			status := loadCRD(t, file).Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
			conds := status.Properties["conditions"]
			if conds.XListType == nil || *conds.XListType != "map" {
				t.Errorf("status.conditions x-kubernetes-list-type = %v, want map", conds.XListType)
			}
			if !slices.Equal(conds.XListMapKeys, []string{"type"}) {
				t.Errorf("status.conditions x-kubernetes-list-map-keys = %v, want [type]", conds.XListMapKeys)
			}
			if _, ok := status.Properties["observedGeneration"]; !ok {
				t.Error("status.observedGeneration is missing")
			}
		})
	}
}

func TestCRDs_PrinterColumnsShowReadyAndReason(t *testing.T) {
	want := map[string]string{
		"Ready":  `.status.conditions[?(@.type=="Ready")].status`,
		"Reason": `.status.conditions[?(@.type=="Ready")].reason`,
	}
	for file, hasPhase := range crdFiles {
		t.Run(file, func(t *testing.T) {
			cols := loadCRD(t, file).Spec.Versions[0].AdditionalPrinterColumns
			byName := func(name string) *apiextensionsv1.CustomResourceColumnDefinition {
				i := slices.IndexFunc(cols, func(c apiextensionsv1.CustomResourceColumnDefinition) bool {
					return c.Name == name
				})
				if i < 0 {
					return nil
				}
				return &cols[i]
			}
			for name, path := range want {
				if c := byName(name); c == nil || c.JSONPath != path || c.Priority != 0 {
					t.Errorf("column %s = %+v, want JSONPath %s at priority 0", name, c, path)
				}
			}
			if phase := byName("Phase"); hasPhase && (phase == nil || phase.Priority != 1) {
				t.Errorf("column Phase = %+v, want priority 1 (kubectl get -o wide)", phase)
			}
		})
	}
}
