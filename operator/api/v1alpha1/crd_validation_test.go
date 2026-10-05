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
	"context"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

const (
	endpointsCRD = "gateway.krakend.io_krakendendpoints.yaml"
)

// validateCRD runs defaulting, the OpenAPI schema, list-type and CEL
// validation of objectYAML against crdFile, as the API server does on create.
func validateCRD(t *testing.T, crdFile, objectYAML string) field.ErrorList {
	t.Helper()
	crd := loadCRD(t, crdFile)
	var props apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &props, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&props)
	if err != nil {
		t.Fatal(err)
	}
	schemaValidator, _, err := apiservervalidation.NewSchemaValidator(&props)
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.YAMLToJSON([]byte(objectYAML))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := kjson.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	structuraldefaulting.Default(obj, structural)
	errs := apiservervalidation.ValidateCustomResource(nil, obj, schemaValidator)
	errs = append(errs, listtype.ValidateListSetsAndMaps(nil, structural, obj)...)
	celErrs, _ := cel.NewValidator(structural, true, celconfig.PerCallLimit).
		Validate(context.Background(), nil, structural, obj, nil, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

type crdCase struct {
	name   string
	object string
	// rejects is a substring of the error the case must produce; "" means valid.
	rejects string
}

func runCRDCases(t *testing.T, crdFile string, cases []crdCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateCRD(t, crdFile, tc.object)
			if tc.rejects == "" {
				if len(errs) != 0 {
					t.Errorf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.rejects) {
				t.Errorf("errors = %v, want one containing %q", errs, tc.rejects)
			}
		})
	}
}

// The API server rejects a CRD whose CEL does not compile or exceeds the cost
// budget; this catches that before the integration suite does.
func TestGeneratedCRDsAreValid(t *testing.T) {
	for file := range crdFiles {
		crd := loadCRD(t, file)
		apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(crd)
		var internal apiextensions.CustomResourceDefinition
		if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(
			crd, &internal, nil); err != nil {
			t.Fatal(err)
		}
		for _, err := range validation.ValidateCustomResourceDefinition(context.Background(), &internal) {
			t.Errorf("%s: %v", file, err)
		}
	}
}

const endpointHead = `{apiVersion: gateway.krakend.io/v1alpha1, kind: KrakenDEndpoint, metadata: {name: e, namespace: ns}, spec: {gatewayRef: {name: gw}, endpoints: `
const okBackend = `backends: [{host: ["http://svc"], urlPattern: "/"}]`

func TestEndpointCRD_Rules(t *testing.T) {
	runCRDCases(t, endpointsCRD, []crdCase{
		{"valid", endpointHead + `[{endpoint: "/a/{id}", method: GET, timeout: "1m30s", cacheTTL: "0", ` + okBackend + `}]}}`, ""},
		{"root wildcard", endpointHead + `[{endpoint: "/*", method: GET, ` + okBackend + `}]}}`, ""},
		{"prefix wildcard", endpointHead + `[{endpoint: "/a/*", method: GET, ` + okBackend + `}]}}`, ""},
		{"no leading slash", endpointHead + `[{endpoint: "a/b", method: GET, ` + okBackend + `}]}}`, "should match"},
		{"query in path", endpointHead + `[{endpoint: "/a?b", method: GET, ` + okBackend + `}]}}`, "should match"},
		{"wildcard mid-path", endpointHead + `[{endpoint: "/a/*/b", method: GET, ` + okBackend + `}]}}`, "should match"},
		{"no endpoints", endpointHead + `[]}}`, "should have at least 1 items"},
		{"no backends", endpointHead + `[{endpoint: "/a", method: GET, backends: []}]}}`, "should have at least 1 items"},
		{"duplicate entry", endpointHead + `[{endpoint: "/a", method: GET, ` + okBackend + `}, {endpoint: "/a", method: GET, ` + okBackend + `}]}}`, "Duplicate value"},
		{"same path, other method", endpointHead + `[{endpoint: "/a", method: GET, ` + okBackend + `}, {endpoint: "/a", method: POST, ` + okBackend + `}]}}`, ""},
		{"not a duration", endpointHead + `[{endpoint: "/a", method: GET, timeout: "3 seconds", ` + okBackend + `}]}}`, "spec.endpoints[0].timeout"},
		{"negative duration", endpointHead + `[{endpoint: "/a", method: GET, cacheTTL: "-1s", ` + okBackend + `}]}}`, "spec.endpoints[0].cacheTTL"},
	})
}
