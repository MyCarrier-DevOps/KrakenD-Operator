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
	"bytes"
	"encoding/json"
	"fmt"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
	"k8s.io/apimachinery/pkg/types"
)

type krakendRenderer struct{}

// New creates a new krakendRenderer.
func New(_ Options) *krakendRenderer {
	return &krakendRenderer{}
}

// Render transforms CRD state into a deterministic krakend.json.
func (r *krakendRenderer) Render(input RenderInput) (*RenderOutput, error) {
	gw := input.Gateway
	if gw == nil {
		return nil, fmt.Errorf("gateway must not be nil")
	}

	// Flatten and deduplicate endpoints
	flat, conflicted, invalid := flattenEndpoints(input.Endpoints, input.Policies)

	// Build the root config object
	config := buildRootConfig(gw)

	// Build endpoints array
	var endpointsJSON []any
	for _, fe := range flat {
		ep := buildEndpointJSON(fe.Entry, input.Policies)
		endpointsJSON = append(endpointsJSON, ep)
	}
	config["endpoints"] = endpointsJSON

	// Build gateway-level extra_config
	gatewayEC := buildGatewayExtraConfig(gw, input.Dragonfly)
	if len(gatewayEC) > 0 {
		config["extra_config"] = gatewayEC
	}

	// Build plugin block
	if pluginBlock := buildPluginBlock(gw); pluginBlock != nil {
		config["plugin"] = pluginBlock
	}

	// Serialize to JSON
	jsonData, err := serializeJSON(config)
	if err != nil {
		return nil, fmt.Errorf("serializing config: %w", err)
	}

	checksum := hash.SHA256Hex(jsonData)
	desiredImage := ResolveImage(gw, input.CEFallback)
	pluginChecksum := computePluginChecksum(gw, input.PluginConfigMaps)

	// Convert conflict/invalid sets to slices
	conflictedSlice := make([]types.NamespacedName, 0, len(conflicted))
	for nn := range conflicted {
		conflictedSlice = append(conflictedSlice, nn)
	}
	invalidSlice := make([]types.NamespacedName, 0, len(invalid))
	for nn := range invalid {
		invalidSlice = append(invalidSlice, nn)
	}

	return &RenderOutput{
		JSON:                jsonData,
		Checksum:            checksum,
		DesiredImage:        desiredImage,
		PluginChecksum:      pluginChecksum,
		ConflictedEndpoints: conflictedSlice,
		InvalidEndpoints:    invalidSlice,
	}, nil
}

// buildRootConfig assembles the top-level krakend.json keys from the gateway spec.
func buildRootConfig(gw *v1alpha1.KrakenDGateway) map[string]any {
	config := map[string]any{
		"version": 3,
	}

	spec := gw.Spec.Config

	if spec.Timeout != "" {
		config["timeout"] = spec.Timeout
	}
	if spec.CacheTTL != "" {
		config["cache_ttl"] = spec.CacheTTL
	}
	if spec.OutputEncoding != "" {
		config["output_encoding"] = spec.OutputEncoding
	}
	if spec.Port != 0 {
		config["port"] = spec.Port
	}

	return config
}

// buildGatewayExtraConfig builds the gateway-level extra_config from spec fields.
func buildGatewayExtraConfig(gw *v1alpha1.KrakenDGateway, df *DragonflyState) map[string]any {
	ec := make(map[string]any)

	spec := gw.Spec.Config

	appendTelemetryConfig(ec, spec.Telemetry)
	appendCORSConfig(ec, spec.CORS)
	appendSecurityConfig(ec, spec.Security)
	appendRouterConfig(ec, spec.Router)
	appendLoggingConfig(ec, spec.Logging)

	if spec.DNSCacheTTL != "" {
		ec["qos/dns"] = map[string]any{"ttl": spec.DNSCacheTTL}
	}

	appendRedisConfig(ec, gw.Spec.Redis, df)

	// Gateway-level raw extra_config (merge last, user overrides take precedence)
	if spec.ExtraConfig != nil && spec.ExtraConfig.Raw != nil {
		var raw map[string]any
		if err := json.Unmarshal(spec.ExtraConfig.Raw, &raw); err == nil {
			for k, v := range raw {
				ec[k] = v
			}
		}
	}

	return ec
}

func appendTelemetryConfig(ec map[string]any, tel *v1alpha1.TelemetryConfig) {
	if tel == nil {
		return
	}
	if tel.OpenTelemetry != nil {
		otel := tel.OpenTelemetry
		otelConfig := make(map[string]any)
		if otel.ServiceName != "" {
			otelConfig["service_name"] = otel.ServiceName
		}
		if otel.Exporters != nil && otel.Exporters.OTLP != nil {
			otelConfig["exporters"] = map[string]any{
				"otlp": []map[string]any{{
					"host": otel.Exporters.OTLP.Host,
					"port": otel.Exporters.OTLP.Port,
				}},
			}
		}
		if len(otelConfig) > 0 {
			ec["telemetry/opentelemetry"] = otelConfig
		}
	}
	if tel.Prometheus != nil && tel.Prometheus.Enabled {
		promConfig := map[string]any{}
		if tel.Prometheus.Port != 0 {
			promConfig["listen_address"] = fmt.Sprintf(":%d", tel.Prometheus.Port)
		}
		ec["telemetry/prometheus"] = promConfig
	}
}

func appendCORSConfig(ec map[string]any, cors *v1alpha1.CORSConfig) {
	if cors == nil {
		return
	}
	c := make(map[string]any)
	if len(cors.AllowOrigins) > 0 {
		c["allow_origins"] = cors.AllowOrigins
	}
	if len(cors.AllowMethods) > 0 {
		c["allow_methods"] = cors.AllowMethods
	}
	if len(cors.AllowHeaders) > 0 {
		c["allow_headers"] = cors.AllowHeaders
	}
	if cors.MaxAge != "" {
		c["max_age"] = cors.MaxAge
	}
	if len(c) > 0 {
		ec["security/cors"] = c
	}
}

func appendSecurityConfig(ec map[string]any, sec *v1alpha1.SecurityConfig) {
	if sec == nil {
		return
	}
	s := make(map[string]any)
	if sec.SSLRedirect {
		s["ssl_redirect"] = true
	}
	if len(sec.SSLProxyHeaders) > 0 {
		s["ssl_proxy_headers"] = sec.SSLProxyHeaders
	}
	if sec.FrameOptions != "" {
		s["frame_deny"] = sec.FrameOptions == "DENY"
		s["custom_frame_options_value"] = sec.FrameOptions
	}
	if sec.ContentTypeNosniff {
		s["content_type_nosniff"] = true
	}
	if sec.BrowserXSSFilter {
		s["browser_xss_filter"] = true
	}
	if sec.HSTSSeconds > 0 {
		s["sts_seconds"] = sec.HSTSSeconds
	}
	if sec.ContentSecurityPolicy != "" {
		s["content_security_policy"] = sec.ContentSecurityPolicy
	}
	if len(s) > 0 {
		ec["security/http"] = s
	}
}

func appendRouterConfig(ec map[string]any, router *v1alpha1.RouterConfig) {
	if router == nil {
		return
	}
	r := make(map[string]any)
	if router.ReturnErrorMsg {
		r["return_error_msg"] = true
	}
	if router.HealthPath != "" {
		r["health_path"] = router.HealthPath
	}
	if router.AutoOptions {
		r["auto_options"] = true
	}
	if router.DisableAccessLog {
		r["disable_access_log"] = true
	}
	if len(r) > 0 {
		ec["router"] = r
	}
}

func appendLoggingConfig(ec map[string]any, logging *v1alpha1.LoggingConfig) {
	if logging == nil {
		return
	}
	l := make(map[string]any)
	if logging.Level != "" {
		l["level"] = logging.Level
	}
	if logging.Format != "" {
		l["syslog"] = logging.Format == "logstash"
	}
	if logging.Stdout {
		l["stdout"] = true
	}
	if len(l) > 0 {
		ec["telemetry/logging"] = l
	}
}

func appendRedisConfig(ec map[string]any, redis *v1alpha1.RedisSpec, df *DragonflyState) {
	if redis != nil {
		pool := redis.ConnectionPool
		r := map[string]any{"addresses": pool.Addresses}
		if pool.PoolSize > 0 {
			r["pool_size"] = pool.PoolSize
		}
		if pool.MinIdleConns > 0 {
			r["min_idle_conns"] = pool.MinIdleConns
		}
		if pool.DialTimeout != "" {
			r["dial_timeout"] = pool.DialTimeout
		}
		if pool.ReadTimeout != "" {
			r["read_timeout"] = pool.ReadTimeout
		}
		if pool.WriteTimeout != "" {
			r["write_timeout"] = pool.WriteTimeout
		}
		ec["backend/redis"] = r
	}

	if df != nil && df.Enabled && df.ServiceDNS != "" {
		if existing, ok := ec["backend/redis"]; ok {
			if redisMap, ok := existing.(map[string]any); ok {
				redisMap["addresses"] = []string{df.ServiceDNS}
			}
		} else {
			ec["backend/redis"] = map[string]any{
				"addresses": []string{df.ServiceDNS},
			}
		}
	}
}

// ResolveImage determines the container image to use based on edition and fallback state.
func ResolveImage(gw *v1alpha1.KrakenDGateway, ceFallback bool) string {
	if ceFallback {
		if gw.Spec.CEImage != "" {
			return gw.Spec.CEImage
		}
		return fmt.Sprintf("krakend:%s", gw.Spec.Version)
	}
	if gw.Spec.Image != "" {
		return gw.Spec.Image
	}
	switch gw.Spec.Edition {
	case v1alpha1.EditionEE:
		return fmt.Sprintf("krakend/krakend-ee:%s", gw.Spec.Version)
	case v1alpha1.EditionCE:
		return fmt.Sprintf("krakend:%s", gw.Spec.Version)
	}
	return fmt.Sprintf("krakend:%s", gw.Spec.Version)
}

// serializeJSON produces deterministic pretty-printed JSON from a config map.
func serializeJSON(config map[string]any) ([]byte, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshaling config to JSON: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return nil, fmt.Errorf("indenting JSON: %w", err)
	}
	return buf.Bytes(), nil
}
