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
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/model"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/common"
	"sigs.k8s.io/yaml"
)

const (
	endpointsCRD = "gateway.krakend.io_krakendendpoints.yaml"
	policiesCRD  = "gateway.krakend.io_krakendbackendpolicies.yaml"
	gatewaysCRD  = "gateway.krakend.io_krakendgateways.yaml"

	autoconfigsCRD = "gateway.krakend.io_krakendautoconfigs.yaml"
)

// validateCRD validates a create of objectYAML against crdFile.
func validateCRD(t *testing.T, crdFile, objectYAML string) field.ErrorList {
	t.Helper()
	return validateCRDUpdate(t, crdFile, objectYAML, "")
}

// validateCRDUpdate runs defaulting, the OpenAPI schema, list-type and CEL
// validation of objectYAML against crdFile, as the API server does. With
// oldYAML set it validates an update from that stored object, including the
// ratcheting of errors in fields the update leaves unchanged.
func validateCRDUpdate(t *testing.T, crdFile, objectYAML, oldYAML string) field.ErrorList {
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
	obj := defaultedObject(t, structural, objectYAML)
	celValidator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	if oldYAML == "" {
		errs := apiservervalidation.ValidateCustomResource(nil, obj, schemaValidator)
		errs = append(errs, listtype.ValidateListSetsAndMaps(nil, structural, obj)...)
		return withCEL(errs, func() field.ErrorList {
			celErrs, _ := celValidator.Validate(
				context.Background(), nil, structural, obj, nil, celconfig.RuntimeCELCostBudget)
			return celErrs
		})
	}

	old := defaultedObject(t, structural, oldYAML)
	corr := common.NewCorrelatedObject(obj, old, &model.Structural{Structural: structural})
	errs := apiservervalidation.ValidateCustomResourceUpdate(
		nil, obj, old, schemaValidator, apiservervalidation.WithRatcheting(corr))
	if len(listtype.ValidateListSetsAndMaps(nil, structural, old)) == 0 {
		errs = append(errs, listtype.ValidateListSetsAndMaps(nil, structural, obj)...)
	}
	return withCEL(errs, func() field.ErrorList {
		celErrs, _ := celValidator.Validate(context.Background(), nil, structural, obj, old,
			celconfig.RuntimeCELCostBudget, cel.WithRatcheting(corr))
		return celErrs
	})
}

// withCEL appends the CEL errors to errs, unless errs holds an error that makes
// the API server skip CEL (the server's hasBlockingErr).
func withCEL(errs field.ErrorList, celErrors func() field.ErrorList) field.ErrorList {
	for _, err := range errs {
		switch err.Type {
		case field.ErrorTypeNotSupported, field.ErrorTypeRequired, field.ErrorTypeTooLong,
			field.ErrorTypeTooMany, field.ErrorTypeTypeInvalid:
			return append(errs, field.Invalid(nil, nil, "some validation rules were not checked because "+
				"the object was invalid; correct the existing errors to complete validation"))
		}
	}
	return append(errs, celErrors()...)
}

func defaultedObject(t *testing.T, structural *schema.Structural, objectYAML string) map[string]any {
	t.Helper()
	data, err := yaml.YAMLToJSON([]byte(objectYAML))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := kjson.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	structuraldefaulting.Default(obj, structural)
	return obj
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
			expectErrors(t, validateCRD(t, crdFile, tc.object), tc.rejects)
		})
	}
}

type crdUpdateCase struct {
	name string
	// object is the update and old the stored object it replaces.
	object, old string
	// rejects is a substring of the error the update must produce; "" means admitted.
	rejects string
}

func runCRDUpdateCases(t *testing.T, crdFile string, cases []crdUpdateCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectErrors(t, validateCRDUpdate(t, crdFile, tc.object, tc.old), tc.rejects)
		})
	}
}

// expectErrors requires errs to be empty when rejects is "" and otherwise to
// contain rejects.
func expectErrors(t *testing.T, errs field.ErrorList, rejects string) {
	t.Helper()
	if rejects == "" {
		if len(errs) != 0 {
			t.Errorf("errors = %v, want none", errs)
		}
		return
	}
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), rejects) {
		t.Errorf("errors = %v, want one containing %q", errs, rejects)
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
		{"overflowing timeout", endpointHead + `[{endpoint: "/a", method: GET, timeout: "2562048h", ` + okBackend + `}]}}`, "spec.endpoints[0].timeout"},
		{"overflowing cacheTTL", endpointHead + `[{endpoint: "/a", method: GET, cacheTTL: "99999999999s", ` + okBackend + `}]}}`, "spec.endpoints[0].cacheTTL"},
		{"negative duration", endpointHead + `[{endpoint: "/a", method: GET, cacheTTL: "-1s", ` + okBackend + `}]}}`, "spec.endpoints[0].cacheTTL"},
		{"output encoding", endpointHead + `[{endpoint: "/a", method: GET, outputEncoding: jsn, ` + okBackend + `}]}}`, "Unsupported value: \"jsn\""},
		{"backend encoding", endpointHead + `[{endpoint: "/a", method: GET, backends: [{host: ["http://svc"], urlPattern: "/", encoding: jsn}]}]}}`, "Unsupported value: \"jsn\""},
		{"service discovery", endpointHead + `[{endpoint: "/a", method: GET, backends: [{host: ["http://svc"], urlPattern: "/", sd: consul}]}]}}`, "Unsupported value: \"consul\""},
		{"backend method", endpointHead + `[{endpoint: "/a", method: GET, backends: [{host: ["http://svc"], urlPattern: "/", method: get}]}]}}`, "Unsupported value: \"get\""},
		{"empty gateway name", `{apiVersion: gateway.krakend.io/v1alpha1, kind: KrakenDEndpoint, metadata: {name: e}, spec: {gatewayRef: {name: ""}, endpoints: [{endpoint: "/a", method: GET, ` + okBackend + `}]}}`, "at least 1 chars long"},
		{"empty policy name", endpointHead + `[{endpoint: "/a", method: GET, backends: [{host: ["http://svc"], urlPattern: "/", policyRef: {name: ""}}]}]}}`, "at least 1 chars long"},
	})
}

func TestEndpointCRD_Ratchets(t *testing.T) {
	const badPath = `{endpoint: "a/b", method: GET, ` + okBackend + `}`
	const goodPath = `{endpoint: "/c", method: GET, ` + okBackend + `}`
	const changedGood = `{endpoint: "/c", method: GET, backends: [{host: ["http://svc"], urlPattern: "/changed"}]}`
	const badBackends = `backends: [{host: ["http://svc"], urlPattern: "/", method: get}, {host: ["http://svc"], urlPattern: "/ok"}]`
	runCRDUpdateCases(t, endpointsCRD, []crdUpdateCase{
		{"stored bad path, a different entry edited",
			endpointHead + `[` + badPath + `, ` + changedGood + `]}}`,
			endpointHead + `[` + badPath + `, ` + goodPath + `]}}`, ""},
		{"stored bad backend, the entry's timeout edited",
			endpointHead + `[{endpoint: "/a", method: GET, timeout: 5s, ` + badBackends + `}]}}`,
			endpointHead + `[{endpoint: "/a", method: GET, ` + badBackends + `}]}}`, ""},
		{"stored bad backend, a sibling backend edited",
			endpointHead + `[{endpoint: "/a", method: GET, backends: [{host: ["http://svc"], urlPattern: "/", method: get}, {host: ["http://svc"], urlPattern: "/changed"}]}]}}`,
			endpointHead + `[{endpoint: "/a", method: GET, ` + badBackends + `}]}}`, "Unsupported value: \"get\""},
	})
}

const policyHead = `{apiVersion: gateway.krakend.io/v1alpha1, kind: KrakenDBackendPolicy, metadata: {name: p, namespace: ns}, spec: `

func TestPolicyCRD_Minimums(t *testing.T) {
	runCRDCases(t, policiesCRD, []crdCase{
		{"valid", policyHead + `{circuitBreaker: {maxErrors: 3, interval: 60, timeout: 10}, rateLimit: {maxRate: 5}}}`, ""},
		{"max errors zero", policyHead + `{circuitBreaker: {maxErrors: 0, interval: 60, timeout: 10}}}`, "spec.circuitBreaker.maxErrors"},
		{"interval zero", policyHead + `{circuitBreaker: {maxErrors: 3, interval: 0, timeout: 10}}}`, "spec.circuitBreaker.interval"},
		{"timeout zero", policyHead + `{circuitBreaker: {maxErrors: 3, interval: 60, timeout: 0}}}`, "spec.circuitBreaker.timeout"},
		{"max rate zero", policyHead + `{rateLimit: {maxRate: 0}}}`, "spec.rateLimit.maxRate"},
	})
}

const gatewayHead = `{apiVersion: gateway.krakend.io/v1alpha1, kind: KrakenDGateway, metadata: {name: g}, spec: {version: "2.13", `

func TestGatewayCRD_Rules(t *testing.T) {
	runCRDCases(t, gatewaysCRD, []crdCase{
		{"valid", gatewayHead + `edition: CE, config: {timeout: 3s, cacheTTL: 0s, dnsCacheTTL: 30s, port: 8080, outputEncoding: json, router: {healthPath: /healthz}, cors: {maxAge: 12h}}}}`, ""},
		{"timeout", gatewayHead + `edition: CE, config: {timeout: "3 seconds"}}}`, "spec.config.timeout"},
		{"cacheTTL", gatewayHead + `edition: CE, config: {cacheTTL: "1 minute"}}}`, "spec.config.cacheTTL"},
		{"dnsCacheTTL", gatewayHead + `edition: CE, config: {dnsCacheTTL: "30"}}}`, "spec.config.dnsCacheTTL"},
		{"compound duration", gatewayHead + `edition: CE, config: {cors: {maxAge: 12h0m}}}}`, "spec.config.cors.maxAge"},
		{"port above range", gatewayHead + `edition: CE, config: {port: 70000}}}`, "spec.config.port"},
		{"port zero", gatewayHead + `edition: CE, config: {port: 0}}}`, "spec.config.port"},
		{"output encoding", gatewayHead + `edition: CE, config: {outputEncoding: yaml}}}`, "Unsupported value: \"yaml\""},
		{"health path", gatewayHead + `edition: CE, config: {router: {healthPath: healthz}}}}`, "spec.config.router.healthPath"},
		{"EE needs a license", gatewayHead + `edition: EE, config: {}}}`, "edition EE requires"},
		{"EE with secretRef", gatewayHead + `edition: EE, config: {}, license: {secretRef: {name: l, key: k}}}}`, ""},
		{"CE with a license", gatewayHead + `edition: CE, config: {}, license: {secretRef: {name: l, key: k}}}}`, "CE edition does not require"},
		{"CE with fallback flag only", gatewayHead + `edition: CE, config: {}, license: {fallbackToCE: true}}}`, ""},
		{"both license sources", gatewayHead + `edition: EE, config: {}, license: {secretRef: {name: l, key: k}, externalSecret: {enabled: true, secretStoreRef: {name: s}, remoteRef: {key: k}}}}}`, "mutually exclusive"},
		{"EE with an empty secretRef name", gatewayHead + `edition: EE, config: {}, license: {secretRef: {name: "", key: k}}}}`, "a license.secretRef with a non-empty name"},
		{"openapi default port clash", gatewayHead + `edition: CE, config: {port: 8090}, openapi: {enabled: true}}}`, "openapi port must differ"},
		{"openapi explicit port clash", gatewayHead + `edition: CE, config: {}, openapi: {enabled: true, port: 8080}}}`, "openapi port must differ"},
		{"openapi disabled", gatewayHead + `edition: CE, config: {port: 8090}, openapi: {enabled: false}}}`, ""},
		{"post-restart job without script", gatewayHead + `edition: CE, config: {}, postRestartJob: {enabled: true}}}`, "script is required"},
		{"too many plugin sources", gatewayHead + `edition: CE, config: {}, plugins: {sources: [` + strings.Repeat(`{configMapRef: {name: c, key: k}}, `, 33) + `]}}}`, "Too many: 33: must have at most 32 items"},
		{"two PVC plugin sources", gatewayHead + `edition: CE, config: {}, plugins: {sources: [{persistentVolumeClaimRef: {claimName: a}}, {persistentVolumeClaimRef: {claimName: b}}]}}}`, "only one PVC plugin source"},
		{"one PVC plugin source", gatewayHead + `edition: CE, config: {}, plugins: {sources: [{persistentVolumeClaimRef: {claimName: a}}, {configMapRef: {name: c, key: k}}]}}}`, ""},
		{"redis password", gatewayHead + `edition: CE, config: {}, redis: {connectionPool: {addresses: ["redis:6379"], password: {name: s, key: p}}}}}`, "password is not supported yet"},
		{"redis tls", gatewayHead + `edition: CE, config: {}, redis: {connectionPool: {addresses: ["redis:6379"], tls: {enabled: true}}}}}`, "tls is not supported yet"},
		{"redis dial timeout", gatewayHead + `edition: CE, config: {}, redis: {connectionPool: {addresses: ["redis:6379"], dialTimeout: "5 seconds"}}}}`, "spec.redis.connectionPool.dialTimeout"},
		{"redis pool", gatewayHead + `edition: CE, config: {}, redis: {connectionPool: {addresses: ["redis:6379"], poolSize: 10, dialTimeout: 5s}}}}`, ""},
		{"Dragonfly password on EE", gatewayHead + `edition: EE, config: {}, license: {secretRef: {name: l, key: k}}, dragonfly: {enabled: true, authentication: {passwordFromSecret: {name: s, key: p}}}}}`, "passwordFromSecret is not supported yet with edition EE"},
		{"Dragonfly password on CE", gatewayHead + `edition: CE, config: {}, dragonfly: {enabled: true, authentication: {passwordFromSecret: {name: s, key: p}}}}}`, ""},
		{"openapi explicit non-default port clash", gatewayHead + `edition: CE, config: {port: 9090}, openapi: {enabled: true, port: 9090}}}`, "openapi port must differ"},
		{"openapi with distinct ports", gatewayHead + `edition: CE, config: {}, openapi: {enabled: true}}}`, ""},
		{"overflowing timeout", gatewayHead + `edition: CE, config: {timeout: "99999999999h"}}}`, "spec.config.timeout"},
		{"overflowing cacheTTL", gatewayHead + `edition: CE, config: {cacheTTL: "99999999999h"}}}`, "spec.config.cacheTTL"},
		{"overflowing dnsCacheTTL", gatewayHead + `edition: CE, config: {dnsCacheTTL: "99999999999h"}}}`, "spec.config.dnsCacheTTL"},
		{"overflowing redis dial timeout", gatewayHead + `edition: CE, config: {}, redis: {connectionPool: {addresses: ["redis:6379"], dialTimeout: "99999999999h"}}}}`, "spec.redis.connectionPool.dialTimeout"},
		{"overflowing tmpSizeLimit", gatewayHead + `edition: CE, config: {}, postRestartJob: {enabled: true, script: x, tmpSizeLimit: "1e99999999999999999999"}}}`, "spec.postRestartJob.tmpSizeLimit"},
		{"fractional exponent tmpSizeLimit", gatewayHead + `edition: CE, config: {}, postRestartJob: {enabled: true, script: x, tmpSizeLimit: "1.5e3.5"}}}`, "spec.postRestartJob.tmpSizeLimit"},
		{"long tmpSizeLimit", gatewayHead + `edition: CE, config: {}, postRestartJob: {enabled: true, script: x, tmpSizeLimit: "0.00000000000000000000000000000000000000000000000000000000000000000000001"}}}`, "at most 64 characters"},
	})
}

func TestGatewayCRD_Ratchets(t *testing.T) {
	const pool = `redis: {connectionPool: {addresses: ["redis:6379"], `
	const ee = gatewayHead + `edition: EE, config: {}, license: {secretRef: {name: l, key: k}}, `
	const withPassword = `dragonfly: {enabled: true, authentication: {passwordFromSecret: {name: s, key: p}}}}}`
	const storedPassword = gatewayHead + `edition: CE, config: {}, ` + pool + `password: {name: s, key: p}}}}}`
	runCRDUpdateCases(t, gatewaysCRD, []crdUpdateCase{
		{"stored timeout that is not a duration, another field edited",
			gatewayHead + `edition: CE, replicas: 3, config: {timeout: "5 seconds"}}}`,
			gatewayHead + `edition: CE, config: {timeout: "5 seconds"}}}`, ""},
		{"stored cacheTTL that is not a duration, another field edited",
			gatewayHead + `edition: CE, replicas: 3, config: {cacheTTL: "1d"}}}`,
			gatewayHead + `edition: CE, config: {cacheTTL: "1d"}}}`, ""},
		{"stored empty dnsCacheTTL, another field edited",
			gatewayHead + `edition: CE, replicas: 3, config: {dnsCacheTTL: ""}}}`,
			gatewayHead + `edition: CE, config: {dnsCacheTTL: ""}}}`, ""},
		{"stored dial timeout that is not a duration, another field edited",
			gatewayHead + `edition: CE, config: {}, replicas: 3, ` + pool + `dialTimeout: "5 seconds"}}}}`,
			gatewayHead + `edition: CE, config: {}, ` + pool + `dialTimeout: "5 seconds"}}}}`, ""},
		{"CE gateway switches to EE without a license",
			gatewayHead + `edition: EE, config: {}}}`, gatewayHead + `edition: CE, config: {}}}`, "edition EE requires"},
		{"stored Dragonfly password on EE, another field edited", ee + `replicas: 3, ` + withPassword, ee + withPassword, ""},
		{"stored Dragonfly password on EE, password changed",
			ee + `dragonfly: {enabled: true, authentication: {passwordFromSecret: {name: s, key: other}}}}}`,
			ee + withPassword, "passwordFromSecret is not supported yet"},
		{"Dragonfly password added on EE", ee + withPassword, ee + `dragonfly: {enabled: true}}}`,
			"passwordFromSecret is not supported yet"},
		{"CE gateway with a Dragonfly password switches to EE", ee + withPassword,
			gatewayHead + `edition: CE, config: {}, ` + withPassword, "passwordFromSecret is not supported yet"},
		{"stored redis password, another field edited",
			gatewayHead + `edition: CE, config: {}, replicas: 3, ` + pool + `password: {name: s, key: p}}}}}`,
			storedPassword, ""},
		{"stored redis password, password changed",
			gatewayHead + `edition: CE, config: {}, ` + pool + `password: {name: s, key: other}}}}}`,
			storedPassword, "password is not supported yet"},
		{"stored redis tls, another field edited",
			gatewayHead + `edition: CE, config: {}, replicas: 3, ` + pool + `tls: {enabled: true}}}}}`,
			gatewayHead + `edition: CE, config: {}, ` + pool + `tls: {enabled: true}}}}}`, ""},
		{"stored bad dial timeout, another field edited",
			gatewayHead + `edition: CE, config: {}, replicas: 3, ` + pool + `dialTimeout: "1h30m"}}}}`,
			gatewayHead + `edition: CE, config: {}, ` + pool + `dialTimeout: "1h30m"}}}}`, ""},
		{"stored unparsable dial timeout, another field edited",
			gatewayHead + `edition: CE, config: {}, replicas: 3, ` + pool + `dialTimeout: "99999999999h"}}}}`,
			gatewayHead + `edition: CE, config: {}, ` + pool + `dialTimeout: "99999999999h"}}}}`,
			"spec.redis.connectionPool.dialTimeout"},
		{"stored bad dial timeout, dial timeout changed",
			gatewayHead + `edition: CE, config: {}, ` + pool + `dialTimeout: "2h30m"}}}}`,
			gatewayHead + `edition: CE, config: {}, ` + pool + `dialTimeout: "1h30m"}}}}`,
			"spec.redis.connectionPool.dialTimeout"},
	})
}

// The API server skips CEL when the schema reports a NotSupported, Required,
// TooLong, TooMany or TypeInvalid error, so a case cannot rely on a rule that
// the server would never evaluate.
func TestValidateCRD_SkipsCELAfterABlockingSchemaError(t *testing.T) {
	object := gatewayHead + `edition: EE, config: {outputEncoding: yaml}}}`
	for name, errs := range map[string]field.ErrorList{
		"create": validateCRD(t, gatewaysCRD, object),
		"update": validateCRDUpdate(t, gatewaysCRD, object, gatewayHead+`edition: CE, config: {}}}`),
	} {
		msg := errs.ToAggregate().Error()
		if strings.Contains(msg, "edition EE requires") {
			t.Errorf("%s: CEL ran despite a blocking schema error: %v", name, errs)
		}
		if !strings.Contains(msg, "some validation rules were not checked") {
			t.Errorf("%s: errors = %v, want the skipped-rules message", name, errs)
		}
	}
}

const autoconfigHead = `{apiVersion: gateway.krakend.io/v1alpha1, kind: KrakenDAutoConfig, metadata: {name: a}, spec: {gatewayRef: {name: gw}, `

func TestAutoConfigCRD_Rules(t *testing.T) {
	runCRDCases(t, autoconfigsCRD, []crdCase{
		{"valid", autoconfigHead + `openapi: {url: "http://svc/openapi.json"}, trigger: Periodic, periodic: {interval: 5m0s}, additionalEndpoints: [{endpoint: /health}]}}`, ""},
		{"both sources", autoconfigHead + `openapi: {url: "http://x", configMapRef: {name: c}}, urlTransform: {hostMapping: [{from: a, to: b}]}, trigger: OnChange}}`, "exactly one of url or configMapRef"},
		{"no source", autoconfigHead + `openapi: {}, trigger: OnChange}}`, "exactly one of url or configMapRef"},
		{"configMap without hostMapping", autoconfigHead + `openapi: {configMapRef: {name: c}}, trigger: OnChange}}`, "hostMapping is required"},
		{"periodic below 30s", autoconfigHead + `openapi: {url: "http://x"}, trigger: Periodic, periodic: {interval: 10s}}}`, "at least 30s"},
		{"periodic interval not a duration", autoconfigHead + `openapi: {url: "http://x"}, trigger: Periodic, periodic: {interval: "5 minutes"}}}`, "spec.periodic.interval in body should match"},
		{"periodic without interval", autoconfigHead + `openapi: {url: "http://x"}, trigger: Periodic}}`, "at least 30s"},
		{"periodic interval overflows", autoconfigHead + `openapi: {url: "http://x"}, trigger: Periodic, periodic: {interval: "99999999999h"}}}`, "spec.periodic.interval"},
		{"both auth secrets", autoconfigHead + `openapi: {url: "http://x", auth: {bearerTokenSecret: {name: s, key: k}, basicAuthSecret: {name: b}}}, trigger: OnChange}}`, "mutually exclusive"},
		{"name too long", `{apiVersion: gateway.krakend.io/v1alpha1, kind: KrakenDAutoConfig, metadata: {name: ` + strings.Repeat("a", 64) + `}, spec: {gatewayRef: {name: gw}, openapi: {url: "http://x"}, trigger: OnChange}}`, "at most 63 characters"},
		{"override method", autoconfigHead + `openapi: {url: "http://x"}, trigger: OnChange, overrides: [{operationId: x, method: HEAD}]}}`, "Unsupported value: \"HEAD\""},
		{"override backend index", autoconfigHead + `openapi: {url: "http://x"}, trigger: OnChange, overrides: [{operationId: x, backends: [{index: -1}]}]}}`, "spec.overrides[0].backends[0].index"},
	})
}
