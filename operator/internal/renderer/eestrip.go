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

package renderer

import (
	"fmt"
	"sort"
)

// The extra_config namespaces KrakenD 2.13 documents as Enterprise-only, by
// the level they appear at. Source: the "Only applies to KrakenD Enterprise"
// box on each page under https://www.krakend.io/docs/enterprise/ (read
// 2026-09-28), cross-checked with "Enterprise only." in the v2.13 schema
// (https://www.krakend.io/schema/v2.13/krakend.json). The schema alone cannot
// separate them: the CE and EE 2.13 binaries embed the same schema, so CE
// lint accepts every namespace below and the CE runtime silently ignores it.
//
// Not listed, because CE honors part of them: telemetry/opentelemetry at
// endpoint and backend level (its proxy section is CE), and endpoint-level
// security/cors (undocumented). backend/http/client is listed although CE
// honors its send_body_on_redirect: its EE settings (client_tls,
// proxy_address, no_redirect) change where and how a backend is reached, and
// dropping them must be visible.
var (
	eeOnlyServiceNamespaces = namespaceSet(
		"ai/mcp", "auth/api-keys", "auth/basic", "documentation/openapi", "documentation/postman",
		"governance/processors", "governance/quota", "grpc", "modifier/request-body-extractor",
		"modifier/response-headers", "qos/ratelimit/service", "qos/ratelimit/service/redis",
		"qos/ratelimit/tiered", "redis", "server/static-filesystem", "server/virtualhost",
		"telemetry/moesif", "telemetry/newrelic", "telemetry/opentelemetry-security",
	)
	eeOnlyEndpointNamespaces = namespaceSet(
		"ai/mcp", "auth/api-keys", "auth/basic", "documentation/openapi", "documentation/postman",
		"governance/quota", "modifier/jmespath", "modifier/request-body-extractor",
		"modifier/request-body-extractor/early", "modifier/request-body-generator", "modifier/response-body",
		"modifier/response-body-generator", "plugin/middleware", "qos/ratelimit/router/redis",
		"qos/ratelimit/tiered", "security/policies", "validation/response-json-schema", "websocket",
	)
	eeOnlyBackendNamespaces = namespaceSet(
		"ai/llm", "auth/aws-sigv4", "auth/gcp", "auth/ntlm", "backend/conditional", "backend/grpc",
		"backend/http/client", "backend/pubsub/publisher/kafka", "backend/pubsub/subscriber/kafka",
		"backend/soap", "backend/static-filesystem", "governance/quota", "modifier/body-generator",
		"modifier/jmespath", "modifier/request-body-generator", "modifier/response-body",
		"modifier/response-body-generator", "plugin/middleware", "qos/circuit-breaker/http",
		"security/policies", "telemetry/logging", "validation/response-json-schema", "workflow",
	)
)

func namespaceSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// stripNamespaces deletes, in place, ec's keys that are in eeOnly, and returns
// one feature per deleted key, labelled "<where> <key>" and sorted.
func stripNamespaces(
	ec map[string]any, eeOnly map[string]struct{}, base StrippedEEFeature, where string,
) []StrippedEEFeature {
	var out []StrippedEEFeature
	for k := range ec {
		if _, ok := eeOnly[k]; !ok {
			continue
		}
		delete(ec, k)
		f := base
		f.Feature = where + " " + k
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Feature < out[j].Feature })
	return out
}

// stripEndpointEEFeatures removes, in place, the Enterprise-only namespaces of
// a rendered endpoint entry and of its backends.
func stripEndpointEEFeatures(ep map[string]any, fe flatEndpoint) []StrippedEEFeature {
	base := StrippedEEFeature{Source: fe.Source, Method: fe.Entry.Method, Endpoint: fe.Entry.Endpoint}
	var out []StrippedEEFeature
	if ec, ok := ep["extra_config"].(map[string]any); ok {
		out = append(out, stripNamespaces(ec, eeOnlyEndpointNamespaces, base, "extra_config")...)
		if len(ec) == 0 {
			delete(ep, "extra_config")
		}
	}
	backends, _ := ep["backend"].([]any)
	for i, b := range backends {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		ec, ok := bm["extra_config"].(map[string]any)
		if !ok {
			continue
		}
		where := fmt.Sprintf("backend[%d] extra_config", i)
		out = append(out, stripNamespaces(ec, eeOnlyBackendNamespaces, base, where)...)
		if len(ec) == 0 {
			delete(bm, "extra_config")
		}
	}
	return out
}
