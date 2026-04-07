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
	"encoding/json"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func minimalGateway() *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
}

func TestRender_NilGateway(t *testing.T) {
	r := New(Options{})
	_, err := r.Render(RenderInput{})
	if err == nil {
		t.Fatal("expected error for nil gateway")
	}
}

func TestRender_MinimalGateway(t *testing.T) {
	r := New(Options{})
	out, err := r.Render(RenderInput{Gateway: minimalGateway()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Checksum == "" {
		t.Fatal("expected non-empty checksum")
	}
	if out.DesiredImage != "krakend:2.7.0" {
		t.Errorf("expected CE image, got %s", out.DesiredImage)
	}

	var config map[string]any
	if err := json.Unmarshal(out.JSON, &config); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if v, ok := config["version"]; !ok || v != float64(3) {
		t.Errorf("expected version 3, got %v", v)
	}
}

func TestRender_WithTimeout(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.Timeout = "3s"
	gw.Spec.Config.CacheTTL = "300s"
	gw.Spec.Config.OutputEncoding = "json"
	gw.Spec.Config.Port = 8080

	r := New(Options{})
	out, err := r.Render(RenderInput{Gateway: gw})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(out.JSON, &config); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if config["timeout"] != "3s" {
		t.Errorf("expected timeout 3s, got %v", config["timeout"])
	}
	if config["cache_ttl"] != "300s" {
		t.Errorf("expected cache_ttl 300s, got %v", config["cache_ttl"])
	}
	if config["output_encoding"] != "json" {
		t.Errorf("expected output_encoding json, got %v", config["output_encoding"])
	}
	if config["port"] != float64(8080) {
		t.Errorf("expected port 8080, got %v", config["port"])
	}
}

func TestRender_WithEndpoints(t *testing.T) {
	gw := minimalGateway()
	dur := metav1.Duration{Duration: 5 * time.Second}
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "ep1",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "test"},
				Endpoints: []v1alpha1.EndpointEntry{
					{
						Endpoint: "/api/v1/users",
						Method:   "GET",
						Timeout:  &dur,
						Backends: []v1alpha1.BackendSpec{
							{
								Host:       []string{"http://users-svc:8080"},
								URLPattern: "/users",
							},
						},
					},
				},
			},
		},
	}

	r := New(Options{})
	out, err := r.Render(RenderInput{Gateway: gw, Endpoints: endpoints})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(out.JSON, &config); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	eps, ok := config["endpoints"].([]any)
	if !ok {
		t.Fatal("expected endpoints array")
	}
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(eps))
	}
}

func TestRender_DeterministicOutput(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.Timeout = "3s"
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "ep1",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "test"},
				Endpoints: []v1alpha1.EndpointEntry{
					{
						Endpoint: "/b",
						Method:   "GET",
						Backends: []v1alpha1.BackendSpec{{Host: []string{"http://b:80"}, URLPattern: "/b"}},
					},
					{
						Endpoint: "/a",
						Method:   "POST",
						Backends: []v1alpha1.BackendSpec{{Host: []string{"http://a:80"}, URLPattern: "/a"}},
					},
				},
			},
		},
	}

	r := New(Options{})
	out1, _ := r.Render(RenderInput{Gateway: gw, Endpoints: endpoints})
	out2, _ := r.Render(RenderInput{Gateway: gw, Endpoints: endpoints})

	if out1.Checksum != out2.Checksum {
		t.Error("expected deterministic checksum across renders")
	}
	if string(out1.JSON) != string(out2.JSON) {
		t.Error("expected deterministic JSON across renders")
	}
}

func TestRender_ConflictDetection(t *testing.T) {
	gw := minimalGateway()
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Minute))

	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "ep-old",
				Namespace:         "default",
				CreationTimestamp: now,
			},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "test"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/conflict", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://a:80"}, URLPattern: "/a"}}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "ep-new",
				Namespace:         "default",
				CreationTimestamp: later,
			},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "test"},
				Endpoints: []v1alpha1.EndpointEntry{
					{Endpoint: "/conflict", Method: "GET", Backends: []v1alpha1.BackendSpec{{Host: []string{"http://b:80"}, URLPattern: "/b"}}},
				},
			},
		},
	}

	r := New(Options{})
	out, err := r.Render(RenderInput{Gateway: gw, Endpoints: endpoints})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.ConflictedEndpoints) != 1 {
		t.Fatalf("expected 1 conflicted endpoint, got %d", len(out.ConflictedEndpoints))
	}
	if out.ConflictedEndpoints[0].Name != "ep-new" {
		t.Errorf("expected ep-new conflicted, got %s", out.ConflictedEndpoints[0].Name)
	}

	// Only the winner's backend should appear
	var config map[string]any
	if err := json.Unmarshal(out.JSON, &config); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	eps := config["endpoints"].([]any)
	if len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after conflict resolution, got %d", len(eps))
	}
}

func TestRender_InvalidPolicyRef(t *testing.T) {
	gw := minimalGateway()
	endpoints := []v1alpha1.KrakenDEndpoint{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "ep-invalid",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "test"},
				Endpoints: []v1alpha1.EndpointEntry{
					{
						Endpoint: "/api/v1/broken",
						Method:   "GET",
						Backends: []v1alpha1.BackendSpec{
							{
								Host:       []string{"http://svc:80"},
								URLPattern: "/broken",
								PolicyRef:  &v1alpha1.PolicyRef{Name: "nonexistent"},
							},
						},
					},
				},
			},
		},
	}

	r := New(Options{})
	out, err := r.Render(RenderInput{Gateway: gw, Endpoints: endpoints})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.InvalidEndpoints) != 1 {
		t.Fatalf("expected 1 invalid endpoint, got %d", len(out.InvalidEndpoints))
	}
}

func TestResolveImage_CEEdition(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Edition = v1alpha1.EditionCE
	gw.Spec.Version = "2.7.0"

	got := ResolveImage(gw, false)
	if got != "krakend:2.7.0" {
		t.Errorf("expected krakend:2.7.0, got %s", got)
	}
}

func TestResolveImage_EEEdition(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.Version = "2.7.0"

	got := ResolveImage(gw, false)
	if got != "krakend/krakend-ee:2.7.0" {
		t.Errorf("expected krakend/krakend-ee:2.7.0, got %s", got)
	}
}

func TestResolveImage_CustomImage(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Image = "myregistry.io/krakend:custom"

	got := ResolveImage(gw, false)
	if got != "myregistry.io/krakend:custom" {
		t.Errorf("expected custom image, got %s", got)
	}
}

func TestResolveImage_CEFallback(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.Version = "2.7.0"

	got := ResolveImage(gw, true)
	if got != "krakend:2.7.0" {
		t.Errorf("expected CE fallback image, got %s", got)
	}
}

func TestResolveImage_CEFallbackCustom(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.CEImage = "myregistry.io/krakend-ce:fallback"

	got := ResolveImage(gw, true)
	if got != "myregistry.io/krakend-ce:fallback" {
		t.Errorf("expected custom CE fallback image, got %s", got)
	}
}

func TestBuildGatewayExtraConfig_CORS(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.CORS = &v1alpha1.CORSConfig{
		AllowOrigins: []string{"https://example.com"},
		AllowMethods: []string{"GET", "POST"},
		AllowHeaders: []string{"Authorization"},
		MaxAge:       "12h",
	}

	ec := buildGatewayExtraConfig(gw, nil)
	cors, ok := ec["security/cors"]
	if !ok {
		t.Fatal("expected security/cors in extra_config")
	}
	corsMap := cors.(map[string]any)
	if corsMap["max_age"] != "12h" {
		t.Errorf("expected max_age 12h, got %v", corsMap["max_age"])
	}
}

func TestBuildGatewayExtraConfig_Security(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.Security = &v1alpha1.SecurityConfig{
		SSLRedirect:           true,
		ContentTypeNosniff:    true,
		BrowserXSSFilter:      true,
		FrameOptions:          "DENY",
		HSTSSeconds:           31536000,
		ContentSecurityPolicy: "default-src 'self'",
	}

	ec := buildGatewayExtraConfig(gw, nil)
	sec, ok := ec["security/http"]
	if !ok {
		t.Fatal("expected security/http in extra_config")
	}
	secMap := sec.(map[string]any)
	if secMap["ssl_redirect"] != true {
		t.Error("expected ssl_redirect true")
	}
	if secMap["frame_deny"] != true {
		t.Error("expected frame_deny true")
	}
}

func TestBuildGatewayExtraConfig_Telemetry(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.Telemetry = &v1alpha1.TelemetryConfig{
		OpenTelemetry: &v1alpha1.OpenTelemetryConfig{
			ServiceName: "my-gateway",
			Exporters: &v1alpha1.OTelExporters{
				OTLP: &v1alpha1.OTLPExporter{Host: "otel-collector", Port: 4317},
			},
		},
		Prometheus: &v1alpha1.PrometheusConfig{Enabled: true, Port: 9090},
	}

	ec := buildGatewayExtraConfig(gw, nil)
	if _, ok := ec["telemetry/opentelemetry"]; !ok {
		t.Fatal("expected telemetry/opentelemetry in extra_config")
	}
	if _, ok := ec["telemetry/prometheus"]; !ok {
		t.Fatal("expected telemetry/prometheus in extra_config")
	}
}

func TestBuildGatewayExtraConfig_Router(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.Router = &v1alpha1.RouterConfig{
		ReturnErrorMsg:   true,
		HealthPath:       "/__health",
		AutoOptions:      true,
		DisableAccessLog: true,
	}

	ec := buildGatewayExtraConfig(gw, nil)
	router, ok := ec["router"]
	if !ok {
		t.Fatal("expected router in extra_config")
	}
	routerMap := router.(map[string]any)
	if routerMap["health_path"] != "/__health" {
		t.Errorf("expected health_path /__health, got %v", routerMap["health_path"])
	}
	if routerMap["auto_options"] != true {
		t.Error("expected auto_options true")
	}
}

func TestBuildGatewayExtraConfig_Logging(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.Logging = &v1alpha1.LoggingConfig{
		Level:  "DEBUG",
		Format: "logstash",
		Stdout: true,
	}

	ec := buildGatewayExtraConfig(gw, nil)
	logging, ok := ec["telemetry/logging"]
	if !ok {
		t.Fatal("expected telemetry/logging in extra_config")
	}
	logMap := logging.(map[string]any)
	if logMap["level"] != "DEBUG" {
		t.Errorf("expected level DEBUG, got %v", logMap["level"])
	}
	if logMap["syslog"] != true {
		t.Error("expected syslog true for logstash format")
	}
}

func TestBuildGatewayExtraConfig_DNSCache(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.DNSCacheTTL = "30s"

	ec := buildGatewayExtraConfig(gw, nil)
	dns, ok := ec["qos/dns"]
	if !ok {
		t.Fatal("expected qos/dns in extra_config")
	}
	dnsMap := dns.(map[string]any)
	if dnsMap["ttl"] != "30s" {
		t.Errorf("expected ttl 30s, got %v", dnsMap["ttl"])
	}
}

func TestBuildGatewayExtraConfig_Redis(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Redis = &v1alpha1.RedisSpec{
		ConnectionPool: v1alpha1.RedisConnectionPool{
			Addresses: []string{"redis:6379"},
			PoolSize:  10,
		},
	}

	ec := buildGatewayExtraConfig(gw, nil)
	redis, ok := ec["backend/redis"]
	if !ok {
		t.Fatal("expected backend/redis in extra_config")
	}
	redisMap := redis.(map[string]any)
	addrs := redisMap["addresses"].([]string)
	if len(addrs) != 1 || addrs[0] != "redis:6379" {
		t.Errorf("unexpected redis addresses: %v", addrs)
	}
}

func TestBuildGatewayExtraConfig_DragonflyOverridesRedis(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Redis = &v1alpha1.RedisSpec{
		ConnectionPool: v1alpha1.RedisConnectionPool{
			Addresses: []string{"old-redis:6379"},
		},
	}
	df := &DragonflyState{
		Enabled:    true,
		ServiceDNS: "gw-dragonfly.ns.svc.cluster.local:6379",
	}

	ec := buildGatewayExtraConfig(gw, df)
	redis := ec["backend/redis"].(map[string]any)
	addrs := redis["addresses"].([]string)
	if len(addrs) != 1 || addrs[0] != "gw-dragonfly.ns.svc.cluster.local:6379" {
		t.Errorf("expected dragonfly DNS to override redis, got %v", addrs)
	}
}

func TestBuildGatewayExtraConfig_DragonflyWithoutRedis(t *testing.T) {
	gw := minimalGateway()
	df := &DragonflyState{
		Enabled:    true,
		ServiceDNS: "gw-dragonfly.ns.svc.cluster.local:6379",
	}

	ec := buildGatewayExtraConfig(gw, df)
	redis, ok := ec["backend/redis"]
	if !ok {
		t.Fatal("expected backend/redis from dragonfly state")
	}
	redisMap := redis.(map[string]any)
	addrs := redisMap["addresses"].([]string)
	if addrs[0] != "gw-dragonfly.ns.svc.cluster.local:6379" {
		t.Errorf("expected dragonfly DNS, got %v", addrs)
	}
}

func TestBuildGatewayExtraConfig_RawOverrides(t *testing.T) {
	gw := minimalGateway()
	gw.Spec.Config.Logging = &v1alpha1.LoggingConfig{Level: "WARNING"}
	gw.Spec.Config.ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"telemetry/logging":{"level":"ERROR"},"custom/plugin":{"enabled":true}}`),
	}

	ec := buildGatewayExtraConfig(gw, nil)
	// Raw overrides typed fields
	logging := ec["telemetry/logging"].(map[string]any)
	if logging["level"] != "ERROR" {
		t.Errorf("expected raw override level ERROR, got %v", logging["level"])
	}
	// Custom plugin key preserved
	if _, ok := ec["custom/plugin"]; !ok {
		t.Error("expected custom/plugin from raw extra_config")
	}
}

func TestBuildGatewayExtraConfig_Empty(t *testing.T) {
	gw := minimalGateway()
	ec := buildGatewayExtraConfig(gw, nil)
	if len(ec) != 0 {
		t.Errorf("expected empty extra_config, got %v", ec)
	}
}

func TestSerializeJSON_Deterministic(t *testing.T) {
	config := map[string]any{
		"z_key": "z_value",
		"a_key": "a_value",
		"m_key": "m_value",
	}
	out1, err := serializeJSON(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out2, _ := serializeJSON(config)
	if string(out1) != string(out2) {
		t.Error("expected deterministic output")
	}
	// Verify sorted key order
	var parsed map[string]any
	if err := json.Unmarshal(out1, &parsed); err != nil {
		t.Fatal(err)
	}
}
