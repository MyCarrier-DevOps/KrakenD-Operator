# KrakenD API Gateway Configuration Cheat Sheet

> **Target:** KrakenD v2.x (config version 3) — Community Edition (CE) and Enterprise (EE)
> **Purpose:** Reference for building a Kubernetes Operator that manages KrakenD gateways via Flexible Configuration
> **Key:** Features marked **[EE]** are Enterprise-only. All others are available in both CE and EE.

---

## 1. Architecture Overview

KrakenD is a **stateless**, **declarative** API Gateway built on the [Lura Project](https://luraproject.org/) (Linux Foundation). Key properties:

- **No database or coordination** — all behavior is defined in configuration files
- **Immutable infrastructure** — config is baked into Docker images or mounted via ConfigMap
- **Stateless clustering** — each instance is independent; no node synchronization
- **Linear scalability** — add replicas without coordination overhead
- **Written in Go** — single binary, runs as UID 1000 in containers

### Request Flow

```
Client → [Router Layer] → [Proxy Layer] → [Backend Layer] → Upstream Services
              │                 │                │
         rate-limit         merge/transform    circuit-breaker
         JWT validation     sequential proxy   backend rate-limit
         CORS               flatmap/filter     caching
```

---

## 2. Configuration File Structure

The root configuration file must be **JSON** (conventionally `krakend.json`). YAML, TOML, and other formats are only supported as _settings files_ within the Flexible Configuration template system (see Section 6) — they cannot be used as the root config file.

```json
{
  "$schema": "https://www.krakend.io/schema/v2.13/krakend.json",
  "version": 3,
  "port": 8080,
  "host": ["http://default-backend:8080"],
  "timeout": "3s",
  "cache_ttl": "0s",
  "output_encoding": "json",
  "listen_ip": "0.0.0.0",
  "dns_cache_ttl": "30s",
  "endpoints": [],
  "extra_config": {}
}
```

### Root-Level Fields

| Field | Type | Default | Description |
|---|---|---|---|
| `version` | int | **required** | Must be `3` for KrakenD v2.x |
| `port` | int | `8080` | TCP listen port (1024-65535 recommended) |
| `host` | []string | — | Optional. Default backend hosts inherited by backends without their own `host` |
| `timeout` | string | `"2s"` | Default timeout for all endpoints. Units: `ns`, `us`, `ms`, `s`, `m`, `h` |
| `cache_ttl` | string | `"0s"` | Default `Cache-Control: public, max-age=N` header for clients/CDN (not server-side caching) |
| `output_encoding` | string | `"json"` | Default response encoding. Values: `json`, `fast-json` **[EE]**, `json-collection`, `yaml`, `xml`, `negotiate`, `string`, `no-op` |
| `listen_ip` | string | `"0.0.0.0"` | Bind address. Supports IPv4 and IPv6 |
| `dns_cache_ttl` | string | `"30s"` | How long DNS SRV service discovery results are cached. Values under `1s` are ignored |
| `endpoints` | []object | **required** | Array of endpoint definitions (your API contract) |
| `extra_config` | object | `{}` | Service-level component configurations (namespaced) |

Reserved paths (cannot be declared as endpoints): `/__health/`, `/__debug/`, `/__echo/`, `/__catchall`, `/__stats/`

---

## 3. Endpoint Configuration

Each entry in `endpoints[]` defines a route the gateway exposes:

```json
{
  "endpoint": "/v1/users/{id}",
  "method": "GET",
  "output_encoding": "json",
  "timeout": "800ms",
  "cache_ttl": "60s",
  "concurrent_calls": 1,
  "input_headers": ["Authorization", "Content-Type"],
  "input_query_strings": ["page", "limit"],
  "backend": [],
  "extra_config": {}
}
```

### Endpoint Fields

| Field | Type | Default | Description |
|---|---|---|---|
| `endpoint` | string | **required** | URL path (case-sensitive, starts with `/`). Supports `{placeholders}`. No colons `:` allowed. **[EE]** supports trailing `/*` wildcards. |
| `method` | string | `"GET"` | HTTP method. Values: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`. Create separate endpoint entries for different methods on the same path. |
| `backend` | []object | **required** | Backend definitions (data origins). Multiple backends are called in parallel and merged. |
| `output_encoding` | string | (inherits root) | Override response encoding. Values: `json`, `fast-json` **[EE]**, `json-collection`, `yaml`, `xml`, `negotiate`, `string`, `no-op` |
| `timeout` | string | (inherits root) | Override timeout for this endpoint |
| `cache_ttl` | string | (inherits root) | Override Cache-Control header. Setting to `0` uses the service default, not zero. |
| `concurrent_calls` | int | `1` | Number of parallel identical requests to backends (returns fastest response) |
| `input_headers` | []string | `[]` | Allowed client headers forwarded to backends. `["*"]` forwards all (unsafe). Case-insensitive. `[]` blocks all headers. |
| `input_query_strings` | []string | `[]` | Allowed query params forwarded to backends. `["*"]` forwards all. `[]` blocks all. |
| `extra_config` | object | `{}` | Endpoint-scoped component configurations |

---

## 4. Backend Configuration

Each entry in `backend[]` inside an endpoint defines an upstream service call:

```json
{
  "host": ["http://user-service:8080"],
  "url_pattern": "/api/users/{id}",
  "method": "GET",
  "encoding": "json",
  "group": "user",
  "target": "data",
  "allow": ["id", "name", "email"],
  "deny": ["password"],
  "mapping": {"id": "user_id"},
  "is_collection": false,
  "sd": "static",
  "disable_host_sanitize": false,
  "extra_config": {}
}
```

### Backend Fields

| Field | Type | Default | Description |
|---|---|---|---|
| `host` | []string | (inherits root) | Backend hosts for load balancing (e.g., `["http://svc:8080"]`). In K8s, use the Service name. |
| `url_pattern` | string | **required** | Path on the backend. Supports `{placeholders}` from the endpoint. |
| `method` | string | (inherits endpoint) | HTTP method in **UPPERCASE**. Can differ from the endpoint method. |
| `encoding` | string | `"json"` | Response parsing. Values: `json`, `safejson`, `fast-json` **[EE]**, `yaml` **[EE]**, `xml`, `rss`, `string`, `no-op` |
| `group` | string | — | Wraps this backend's response under a key in the merged response |
| `target` | string | — | Extracts a nested object from the response (dot notation for nesting) |
| `allow` | []string | — | Allowlist: only return these fields (case-sensitive, dot notation for nested) |
| `deny` | []string | — | Denylist: remove these fields from response (case-sensitive, dot notation for nested) |
| `mapping` | object | — | Rename response fields: `{"original_name": "new_name"}` |
| `is_collection` | bool | `false` | Set `true` when backend returns a JSON array `[]` instead of object `{}`. Note: the KrakenD JSON schema says `true` but the engine behaves as if the default is `false` — this is a known upstream docs discrepancy. |
| `sd` | string | `"static"` | Service discovery: `static`, `dns` (DNS SRV), or `dns-shared` |
| `sd_scheme` | string | `"http"` | Protocol scheme for DNS SRV discovery (e.g., `"https"`) |
| `disable_host_sanitize` | bool | `false` | Set `true` for non-HTTP protocols (`amqp://`, `nats://`) or `sd=dns` |
| `extra_config` | object | `{}` | Backend-scoped component configurations |

### Response Manipulation Priority

When `flatmap_filter` is enabled: `group` and `target` work, but `allow`, `deny`, and `mapping` are **ignored**.

---

## 5. `extra_config` Namespaces Reference

The `extra_config` object exists at three scopes. Each component defines which scope(s) it supports.

| Scope | Placement | Applies To |
|---|---|---|
| **service** | Root-level `extra_config` | All requests globally |
| **endpoint** | Inside an endpoint's `extra_config` | That endpoint only |
| **backend** | Inside a backend's `extra_config` | That backend only |

### 5.1 Security — CORS (`security/cors`) — Scope: service

```json
{
  "extra_config": {
    "security/cors": {
      "allow_origins": ["https://example.com"],
      "allow_methods": ["GET", "POST", "PUT", "DELETE"],
      "allow_headers": ["Authorization", "Content-Type"],
      "expose_headers": ["Content-Length", "Content-Type"],
      "allow_credentials": false,
      "max_age": "12h",
      "debug": false
    }
  }
}
```

| Field | Default | Description |
|---|---|---|
| `allow_origins` | `["*"]` | Allowed origins. `"*"` = any |
| `allow_methods` | `["GET","HEAD","POST"]` | Allowed HTTP methods |
| `allow_headers` | `[]` | Allowed request headers (Origin always appended) |
| `expose_headers` | `["Content-Length","Content-Type"]` | Headers safe to expose |
| `allow_credentials` | `false` | Allow cookies/auth credentials |
| `max_age` | `"0h"` | Preflight cache duration |
| `debug` | `false` | Enable debug logging (dev only) |

### 5.2 Security — HTTP Headers (`security/http`) — Scope: service

```json
{
  "extra_config": {
    "security/http": {
      "allowed_hosts": [],
      "ssl_redirect": true,
      "sts_seconds": 31536000,
      "sts_include_subdomains": true,
      "frame_deny": true,
      "content_type_nosniff": true,
      "browser_xss_filter": true,
      "content_security_policy": "default-src 'self';",
      "referrer_policy": "same-origin",
      "is_development": false
    }
  }
}
```

> **Note:** `ssl_redirect` defaults to `true`. Set to `false` only for non-TLS environments or when TLS is terminated upstream.

| Field | Default | Description |
|---|---|---|
| `allowed_hosts` | `[]` | Restrict to specific Host headers |
| `ssl_redirect` | `true` | Redirect HTTP to HTTPS |
| `ssl_host` | `""` | Host for SSL redirect |
| `ssl_proxy_headers` | `{}` | Headers indicating HTTPS (e.g., `{"X-Forwarded-Proto":"https"}`) |
| `sts_seconds` | `0` | HSTS max-age. `0` disables |
| `sts_include_subdomains` | `false` | HSTS includeSubdomains |
| `frame_deny` | `false` | X-Frame-Options: DENY |
| `custom_frame_options_value` | `""` | Custom X-Frame-Options value |
| `content_type_nosniff` | `false` | X-Content-Type-Options: nosniff |
| `browser_xss_filter` | `false` | X-XSS-Protection: 1; mode=block |
| `content_security_policy` | `""` | CSP header value |
| `referrer_policy` | `"same-origin"` | Referrer-Policy header |
| `is_development` | `false` | Disables host/SSL/STS checks (dev only) |

### 5.3 TLS (`tls`) — Scope: root level (NOT inside `extra_config`)

> **Important:** `tls` is a **root-level key**, not an `extra_config` namespace.

**v2.7+ format** (current — keys moved to `keys[]` array for multi-cert support):

```json
{
  "version": 3,
  "tls": {
    "keys": [
      {
        "public_key": "/path/to/cert.pem",
        "private_key": "/path/to/key.pem"
      }
    ],
    "min_version": "TLS13",
    "max_version": "TLS13",
    "enable_mtls": false,
    "ca_certs": [],
    "disable_system_ca_pool": false,
    "disabled": false
  }
}
```

**Legacy format** (pre-v2.7, deprecated but still functional):

```json
{
  "version": 3,
  "tls": {
    "public_key": "/path/to/cert.pem",
    "private_key": "/path/to/key.pem"
  }
}
```

| Field | Default | Description |
|---|---|---|
| `keys` | — | Array of `{public_key, private_key}` objects. Supports multiple certs (v2.7+) |
| `min_version` | `"TLS13"` | Minimum TLS version: `SSL3.0`, `TLS10`, `TLS11`, `TLS12`, `TLS13` |
| `max_version` | `"TLS13"` | Maximum TLS version |
| `enable_mtls` | `false` | Require client certificates on all endpoints |
| `ca_certs` | `[]` | Additional CA PEM files for mTLS |
| `disable_system_ca_pool` | `false` | Ignore system CAs, only use `ca_certs` |
| `disabled` | `false` | Disable TLS (development only) |

### 5.4 Authentication — JWT Validation (`auth/validator`) — Scope: endpoint (+ service level for `shared_cache_duration`)

```json
{
  "endpoint": "/protected",
  "extra_config": {
    "auth/validator": {
      "alg": "RS256",
      "jwk_url": "https://idp.example.com/.well-known/jwks.json",
      "cache": true,
      "audience": ["https://api.example.com"],
      "issuer": "https://idp.example.com",
      "roles_key": "realm_access.roles",
      "roles_key_is_nested": true,
      "roles": ["admin", "user"],
      "scopes_key": "scope",
      "scopes_matcher": "any",
      "scopes": ["read:data", "write:data"],
      "propagate_claims": [["sub", "x-user-id"], ["email", "x-user-email"]],
      "disable_jwk_security": false,
      "operation_debug": false
    }
  }
}
```

| Field | Default | Description |
|---|---|---|
| `alg` | `"RS256"` | Signing algorithm. Values: `EdDSA`, `HS256/384/512`, `RS256/384/512`, `ES256/384/512`, `PS256/384/512` |
| `jwk_url` | — | URL to JWK endpoint for public keys |
| `jwk_local_path` | — | Local JWK file path (takes priority over `jwk_url`) |
| `cache` | `false` | Cache JWK keys (see `cache_duration`) |
| `cache_duration` | `900` | Seconds to cache JWK keys when `cache: true` |
| `audience` | — | Required audiences (ALL must match) |
| `issuer` | — | Required issuer |
| `roles_key` | — | Claim key containing roles (dot notation for nesting) |
| `roles_key_is_nested` | `false` | Set `true` when using dot notation in `roles_key` |
| `roles` | — | At least one role must be present |
| `scopes_key` | — | Claim key containing scopes |
| `scopes_matcher` | `"any"` | `any` = at least one scope, `all` = all scopes required |
| `scopes` | — | Required scopes |
| `propagate_claims` | — | Forward claims as backend headers: `[["claim","header"]]`. **Important:** The endpoint's `input_headers` must also include the propagated header names, or backends won't receive them. |
| `auth_header_name` | `"Authorization"` | Custom header to read the token from |
| `cookie_key` | — | Read token from a cookie instead of Authorization header |
| `leeway` | `"1s"` | Clock skew tolerance for token expiry validation |
| `key_identify_strategy` | `"kid"` | Key lookup strategy. Values: `kid`, `x5t`, `x5t#S256`, `kid_x5t` |
| `jwk_fingerprints` | — | Certificate fingerprints for pinning (MITM protection) |
| `disable_jwk_security` | `false` | Allow plain HTTP for JWK URL (dev only) |
| `operation_debug` | `false` | Log all validation operations at ERROR level |

### 5.5 Rate Limiting — Endpoint (`qos/ratelimit/router`) — Scope: endpoint

```json
{
  "endpoint": "/api/resource",
  "extra_config": {
    "qos/ratelimit/router": {
      "max_rate": 100,
      "client_max_rate": 10,
      "strategy": "ip",
      "key": "",
      "every": "1s",
      "capacity": 100,
      "client_capacity": 10
    }
  }
}
```

| Field | Default | Description |
|---|---|---|
| `max_rate` | — | Max requests/`every` from **all** clients combined (token bucket refill rate) |
| `client_max_rate` | — | Max requests/`every` **per client** |
| `strategy` | `"ip"` | Client identification: `ip`, `header`, or `param` |
| `key` | — | Header name when `strategy=header`, or param name when `strategy=param` |
| `every` | `"1s"` | Time window for rate limits |
| `capacity` | `max_rate` (in requests/second), or `1` for sub-second fractions | Max burst tokens for all clients. Recommended: set explicitly equal to `max_rate` |
| `client_capacity` | `client_max_rate` (in requests/second), or `1` for sub-second fractions | Max burst tokens per client. Recommended: set explicitly equal to `client_max_rate` |

At least one of `max_rate` or `client_max_rate` is required. Counters are **in-memory per instance**.

### 5.6 Rate Limiting — Backend (`qos/ratelimit/proxy`) — Scope: backend

```json
{
  "backend": [{
    "extra_config": {
      "qos/ratelimit/proxy": {
        "max_rate": 50,
        "every": "1s",
        "capacity": 50
      }
    }
  }]
}
```

Controls KrakenD-to-backend traffic. Same token bucket algorithm. `max_rate` + `capacity` are required. This protects your backends from being overwhelmed by KrakenD itself.

### 5.7 Circuit Breaker (`qos/circuit-breaker`) — Scope: backend

```json
{
  "backend": [{
    "extra_config": {
      "qos/circuit-breaker": {
        "interval": 60,
        "timeout": 10,
        "max_errors": 3,
        "name": "cb-users-backend",
        "log_status_change": true
      }
    }
  }]
}
```

| Field | Required | Description |
|---|---|---|
| `interval` | **yes** | Time window (seconds) to count consecutive errors |
| `timeout` | **yes** | Seconds to wait before testing backend health again (half-open state) |
| `max_errors` | **yes** | Consecutive errors within `interval` to trip the circuit. Errors include: responses with status codes other than **200 or 201** (this means 202, 204, etc. also count as errors), network failures, timeouts, proxy rate limit rejections, security policy violations, Lua/CEL errors. For `no-op` encoding, HTTP status codes are NOT evaluated. |
| `name` | no | Friendly name for log output |
| `log_status_change` | no | Log state transitions (closed/open/half-open) |

States: **Closed** (healthy) → **Open** (failing, all requests rejected) → **Half-Open** (testing).

### 5.8 Backend HTTP Caching (`qos/http-cache`) — Scope: backend

```json
{
  "backend": [{
    "extra_config": {
      "qos/http-cache": {
        "shared": false
      }
    }
  }]
}
```

- Caches backend GET/HEAD responses **in-memory** per instance
- Honors the backend's `Cache-Control` header (RFC 7234)
- Cache key = final backend URL + Vary headers
- `shared: true` shares cache across backends with the same URL
- **No TTL control** — cache lifetime is entirely driven by the backend's `Cache-Control` response header. If the backend returns no `Cache-Control`, nothing is cached.
- **Warning:** Increases memory consumption proportional to response sizes and TTLs

### 5.9 Logging (`telemetry/logging`) — Scope: service

```json
{
  "extra_config": {
    "telemetry/logging": {
      "level": "INFO",
      "prefix": "[KRAKEND]",
      "stdout": true,
      "syslog": false,
      "format": "logstash"
    }
  }
}
```

| Field | Default | Description |
|---|---|---|
| `level` | **(required)** | Log level: `DEBUG`, `INFO`, `WARNING`, `ERROR`, `CRITICAL` |
| `prefix` | `""` | Log line prefix |
| `stdout` | `false` | Output to stdout |
| `syslog` | `false` | Output to syslog |
| `format` | `"default"` | Log format: `default`, `logstash` (JSON output), or `custom` |

### 5.10 Router Options (`router`) — Scope: service

```json
{
  "extra_config": {
    "router": {
      "return_error_msg": true,
      "disable_access_log": false,
      "disable_redirect_fixed_path": true,
      "disable_redirect_trailing_slash": true,
      "auto_options": true,
      "health_path": "/health",
      "app_engine": false
    }
  }
}
```

| Field | Default | Description |
|---|---|---|
| `return_error_msg` | `false` | Return error interpretations (without backend body) to clients |
| `disable_access_log` | `false` | Suppress access request logs |
| `disable_redirect_fixed_path` | `false` | Disable automatic case-correction redirects |
| `disable_redirect_trailing_slash` | `false` | Disable trailing-slash redirect (requires `disable_redirect_fixed_path` too) |
| `auto_options` | `false` | Auto-generate OPTIONS handlers for all registered paths |
| `health_path` | `"/__health"` | Change the health endpoint path |

### 5.11 Sequential Proxy (`proxy`) — Scope: endpoint

```json
{
  "endpoint": "/chained",
  "extra_config": {
    "proxy": {
      "sequential": true
    }
  },
  "backend": [
    {"url_pattern": "/first"},
    {"url_pattern": "/second/{resp0_id}"}
  ]
}
```

Backends execute **in order** instead of parallel. Reference previous response fields with `{resp0_fieldName}`, `{resp1_fieldName}`, etc.

### 5.12 Flatmap Filter (`proxy`) — Scope: backend or endpoint

```json
{
  "extra_config": {
    "proxy": {
      "flatmap_filter": [
        {"type": "move", "args": ["zone.state", "shipping_state"]},
        {"type": "del", "args": ["zone"]}
      ]
    }
  }
}
```

Operations: `move` (rename/relocate), `del` (delete), `append`. When flatmap is active, `allow`/`deny`/`mapping` are ignored but `group`/`target` still work.

> **Note:** Flatmap at the endpoint level only works when there is more than one backend configured. Single-backend endpoints silently ignore endpoint-level flatmap.

### 5.13 Telemetry — OpenTelemetry (`telemetry/opentelemetry`) — Scope: service

The **recommended** telemetry integration. Supports distributed tracing and metrics export to OTLP-compatible backends (Jaeger, Prometheus, Grafana, Datadog, AWS X-Ray, Azure Monitor, etc.). Configure exporters within the `telemetry/opentelemetry` block, including a `prometheus` exporter for metrics scraping.

### 5.14 Telemetry — OpenCensus (`telemetry/opencensus`) — Scope: service — **DEPRECATED**

> **Warning:** OpenCensus is officially deprecated, frozen, and receives no further updates (including security fixes). Use `telemetry/opentelemetry` instead.

Legacy integration that can expose Prometheus metrics, Jaeger/Zipkin traces, etc.

### 5.15 Traffic Shadowing (`proxy`) — Scope: backend

```json
{
  "backend": [
    {
      "host": ["http://my-api.com"],
      "url_pattern": "/v1/user/{id}"
    },
    {
      "host": ["http://my-api.com"],
      "url_pattern": "/v2/user/{id}",
      "extra_config": {
        "proxy": {
          "shadow": true
        }
      }
    }
  ]
}
```

Shadow backends receive copies of traffic but their responses are **ignored** and never merged. Useful for canary testing, performance benchmarking, and validating new backend versions in production without impacting users.

---

## 6. Flexible Configuration System

The Flexible Configuration replaces a monolithic `krakend.json` with a **Go-template-based** build system, enabling multi-file configs, environment injection, and code reuse.

### 6.1 Directory Structure

```
.
├── krakend.tmpl              # Main template (entry point)
└── config/
    ├── partials/             # Raw text fragments (inserted as-is, NOT evaluated)
    │   ├── rate_limit.json
    │   └── cors.json
    ├── templates/            # Go templates (evaluated with full template engine)
    │   ├── endpoints.tmpl
    │   └── middleware.tmpl
    └── settings/             # JSON data files (variables/values)
        ├── prod/
        │   └── urls.json
        ├── dev/
        │   └── urls.json
        └── endpoints.json
```

### 6.2 Environment Variables

| Variable | Required | Description |
|---|---|---|
| `FC_ENABLE=1` | **yes** | Activates Flexible Configuration (any non-empty value; `0` does NOT disable) |
| `FC_SETTINGS=path` | no | Path to `settings/` directory (JSON data files) |
| `FC_PARTIALS=path` | no | Path to `partials/` directory (raw text fragments) |
| `FC_TEMPLATES=path` | no | Path to `templates/` directory (Go templates) |
| `FC_OUT=filename` | no | Write rendered config to this file (for debugging) |

### 6.3 Template Functions

The template engine supports Go `text/template` syntax, **Sprig functions**, and KrakenD-specific functions:

| Function | Description | Example |
|---|---|---|
| `{{ .filename.key }}` | Access a value from a settings JSON file | `{{ .urls.users_api }}` |
| `{{ marshal .filename.key }}` | Insert a JSON structure from settings as serialized JSON | `{{ marshal .urls }}` |
| `{{ include "file.txt" }}` | Include a partial file as-is (no evaluation) | `{{ include "rate_limit.json" }}` |
| `{{ template "file.tmpl" . }}` | Execute a sub-template with the given context | `{{ template "endpoints.tmpl" . }}` |
| `{{ env "VAR_NAME" }}` | Read an environment variable (KrakenD-specific, not in standard Sprig) | `{{ env "DB_HOST" }}` |
| `{{ range }}` | Iterate over arrays/maps | `{{ range .endpoints.group }}` |
| `{{ if }}` / `{{ else }}` | Conditionals | `{{ if .settings.enable_cors }}` |
| `{{ add }}`, `{{ mul }}`, etc. | Sprig math functions | `{{ add 2 1 }}` → `3` |

### 6.4 Example: Main Template (`krakend.tmpl`)

```go-template
{
    "version": 3,
    "port": {{ .service.port }},
    "host": {{ marshal .service.default_hosts }},
    "timeout": "{{ .service.timeout }}",
    "extra_config": {
        {{ include "cors.json" }},
        {{ include "logging.json" }}
    },
    "endpoints": [
        {{ range $idx, $ep := .endpoints.api_v1 }}
        {{ if $idx }},{{ end }}
        {
            "endpoint": "{{ $ep.endpoint }}",
            "method": "{{ $ep.method }}",
            "backend": [
                {
                    "url_pattern": "{{ $ep.backend }}",
                    "host": {{ marshal $.service.default_hosts }}
                }
            ]
        }
        {{ end }}
    ]
}
```

### 6.5 Example: Settings File (`settings/endpoints.json`)

```json
{
    "api_v1": [
        {
            "endpoint": "/users/{id}",
            "method": "GET",
            "backend": "/api/v1/users/{id}"
        },
        {
            "endpoint": "/orders",
            "method": "GET",
            "backend": "/api/v1/orders"
        }
    ]
}
```

### 6.6 Example: Partial File (`partials/cors.json`)

```json
"security/cors": {
    "allow_origins": ["*"],
    "allow_methods": ["GET", "POST", "PUT", "DELETE"],
    "allow_headers": ["Authorization", "Content-Type"],
    "max_age": "12h"
}
```

### 6.7 Validation Commands

```bash
# Check syntax with flexible config
FC_ENABLE=1 \
FC_SETTINGS="$PWD/config/settings/prod" \
FC_PARTIALS="$PWD/config/partials" \
FC_TEMPLATES="$PWD/config/templates" \
FC_OUT=compiled.json \
krakend check -tlc "$PWD/krakend.tmpl"

# Run with flexible config
FC_ENABLE=1 \
FC_SETTINGS="config/settings/prod" \
FC_PARTIALS="config/partials" \
FC_TEMPLATES="config/templates" \
krakend run -c krakend.tmpl
```

---

## 7. Service Discovery

### Static (Default)

Hosts are explicitly listed in `host[]` arrays. In Kubernetes, use the K8s Service DNS name:

```json
{"host": ["http://my-service.namespace.svc.cluster.local:8080"]}
```

### DNS SRV

For Kubernetes, Consul, or any DNS SRV-aware system:

```json
{
  "sd": "dns",
  "sd_scheme": "https",
  "host": ["_https._tcp.my-app.default.svc.cluster.local"],
  "disable_host_sanitize": true
}
```

DNS SRV entries are cached for **30 seconds** by default (configurable via root-level `dns_cache_ttl`, e.g., `"dns_cache_ttl": "10s"`).

---

## 8. Response Encoding

### Endpoint `output_encoding` (what clients receive)

| Value | Description |
|---|---|
| `json` | Standard JSON object (default) |
| `fast-json` | Faster JSON serialization (less validation) **[EE]** |
| `json-collection` | Returns JSON arrays. Backend must set `is_collection: true` |
| `yaml` | YAML output |
| `xml` | XML output |
| `negotiate` | Content negotiation based on `Accept` header |
| `string` | Plain text |
| `no-op` | Direct proxy — no manipulation, single backend only |

### Backend `encoding` (how to parse upstream responses)

| Value | Description |
|---|---|
| `json` | Standard JSON (default) |
| `safejson` | JSON with extra validation |
| `fast-json` | Faster parsing **[EE]** |
| `yaml` | YAML response parsing **[EE]** |
| `xml` | XML response |
| `rss` | RSS feed |
| `string` | Plain text (access with `resp0_content` in sequential proxy) |
| `no-op` | Pass-through, no parsing |

---

## 9. Kubernetes Deployment

### Recommended Dockerfile

```dockerfile
FROM krakend/krakend-ee:2.x    # or krakend/krakend:2.x for CE
COPY krakend.json /etc/krakend/krakend.json
# Or for flexible config:
# COPY config/ /etc/krakend/config/
# COPY krakend.tmpl /etc/krakend/krakend.tmpl
USER 1000
EXPOSE 8080
```

### Deployment YAML

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: krakend
spec:
  replicas: 2
  selector:
    matchLabels:
      app: krakend
  template:
    metadata:
      labels:
        app: krakend
    spec:
      containers:
      - name: krakend
        image: your-krakend-image:1.0.0
        ports:
        - containerPort: 8080
        command: ["/usr/bin/krakend"]
        args: ["run", "-c", "/etc/krakend/krakend.json", "-p", "8080"]  # omit -d in production
        securityContext:
          allowPrivilegeEscalation: false
          runAsNonRoot: true
          runAsUser: 1000
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
        env:
        - name: KRAKEND_PORT
          value: "8080"
        # For flexible config:
        # - name: FC_ENABLE
        #   value: "1"
        # - name: FC_SETTINGS
        #   value: "/etc/krakend/config/settings"
        # - name: FC_PARTIALS
        #   value: "/etc/krakend/config/partials"
        # - name: FC_TEMPLATES
        #   value: "/etc/krakend/config/templates"
```

### Service YAML

```yaml
apiVersion: v1
kind: Service
metadata:
  name: krakend-service
spec:
  type: ClusterIP
  selector:
    app: krakend
  ports:
  - name: http
    port: 8080
    targetPort: 8080
    protocol: TCP
```

### Key Deployment Notes

- **Always run as UID 1000** (required for OpenShift and recommended for all K8s)
- **Immutable artifacts preferred** — bake config into the image via CI/CD
- **ConfigMap alternative** — mount config files, but requires pod restart on change
- **Stateless** — no persistent storage needed; scale horizontally with replicas
- **No coordination** — each instance independently processes its config
- **Health endpoint** — `/__health` (configurable via `router.health_path`)

---

## 10. Environment Variables

### KrakenD Reserved Variables

Override configuration values with `KRAKEND_`-prefixed environment variables (set during `run` or `check`).

> **Critical constraint:** `KRAKEND_`-prefixed variables can only **override** values that already exist in the configuration file. If a key is absent from the config, the env var has no effect. For example, `KRAKEND_PORT=9090` only works if `"port"` is already declared in the config.

### Custom Variables in Flexible Config

Use the `{{ env "VAR_NAME" }}` function (KrakenD-specific, not in standard Sprig) in templates:

```go-template
{
    "host": ["{{ env "BACKEND_HOST" }}"],
    "name": "Build {{ env "COMMIT_SHA" | trunc 8 }}"
}
```

---

## 11. Extending KrakenD

### Plugin Types (CE + EE)

All three plugin types below are available in **both CE and EE**:

| Type | Namespace | Scope | Description |
|---|---|---|---|
| HTTP Server Plugin | `plugin/http-server` | service | Intercepts incoming HTTP requests before KrakenD processing |
| HTTP Client Plugin | `plugin/http-client` | backend | Intercepts outgoing requests to backends |
| Req/Resp Modifier | `plugin/req-resp-modifier` | endpoint/backend | Modifies request/response payloads in the proxy pipe. Executed sequentially in declaration order. |

**[EE]** `plugin/middleware` — Enterprise-only plugin type at the HTTP server level with additional capabilities.

### Lua Scripting (CE + EE)

Embed Lua scripts in endpoint, backend, or service-level `extra_config` for lightweight custom logic without compiling plugins. Namespaces: `modifier/lua-endpoint` (endpoint or service level — service level applies globally to all requests), `modifier/lua-proxy` (endpoint level), `modifier/lua-backend` (backend level).

### CEL — Common Expression Language (CE + EE)

Used for conditional request/response evaluation and, in Enterprise, for security policy definitions with built-in functions and advanced macros.

---

## 12. Enterprise-Only Features [EE]

Features exclusive to KrakenD Enterprise that the operator should support when managing EE deployments:

### Security & Authentication

| Feature | Namespace / Description |
|---|---|
| **Security Policies** | CEL-based policy language with built-in functions, advanced macros, and playbooks for fine-grained access control |
| **API Key Authentication** | Validate requests using API keys |
| **Basic Authentication** | HTTP Basic Auth (plugin) |
| **NTLM Authentication** | NTLM auth for backend services |
| **Multiple Identity Providers** | Integrate with multiple IdPs simultaneously |
| **IP Filtering** | Allow/deny by IP address at endpoint level (plugin) |

### Traffic Management

| Feature | Description |
|---|---|
| **Tiered Rate Limiting** | Different rate limits per user tier/group |
| **Cluster Rate Limiting** | Coordinated rate limits across instances (requires Redis) |
| **Stateful Rate Limiting** | Redis-backed persistent rate limit counters |
| **Bot Detector** | Identify and block bot traffic (CE has a bot detector at `security/bot-detector`; listed here because EE bundles extended traffic management) |

### Endpoints & Routing

| Feature | Description |
|---|---|
| **Wildcard Endpoints** | Trailing `/*` in endpoint paths for glob matching |
| **Dynamic Routing** | Runtime route resolution |
| **CatchAll Routes** | Handle unmatched requests |
| **URL Rewrite** | Modify request URLs |
| **WebSockets** | Bidirectional real-time communication |
| **Static Content** | Serve files directly from filesystem |

### Validation & Documentation

| Feature | Description |
|---|---|
| **JSON Schema Response Validation** | Validate response bodies against schema |
| **OpenAPI/Swagger** | Auto-generate API documentation |

> **Note:** JSON Schema **Request** Validation (`validation/json-schema`) is available in both CE and EE — it is NOT Enterprise-only.

### Non-REST Connectivity

| Feature | Description |
|---|---|
| **gRPC Backend** | Connect to gRPC services (`backend/grpc`) |
| **SOAP** | SOAP backend integration (`backend/soap`) |

> **Note:** GraphQL (`backends/graphql`) and AMQP/RabbitMQ (`backends/amqp-consumer`, `backends/amqp-producer`) are available in both CE and EE — they are NOT Enterprise-only.

### Extended Flexible Configuration [EE]

Enterprise replaces the `FC_*` environment variables with a richer JSON-based configuration in the same file:

```json
{
    "$schema": "https://www.krakend.io/schema/v2.9/flexible_config.json",
    "settings": {
        "paths": ["settings/common", "settings/production"],
        "allow_overwrite": true,
        "allowed_suffixes": [".yaml", ".json"]
    },
    "partials": {
        "paths": ["partials"]
    },
    "templates": {
        "paths": ["templates"]
    },
    "ref_key": "$ref",
    "out": "result.json",
    "debug": false
}
```

Key EE Flexible Config advantages over CE:
- **`$ref` operator** — Include external JSON files inline (like JSON Schema `$ref`)
- **Nested directories** — Recursive scanning of settings/partials/templates
- **Multiple format support** — Settings in JSON, YAML, TOML, INI, ENV, and properties files
- **No environment variables needed** — Configuration-driven instead of `FC_*` env vars
- **`allow_overwrite`** — Control whether overlapping settings paths can override each other
- All CE `FC_*`-based templates remain 100% compatible with the EE engine

---

## 13. Enterprise License Management [EE]

### License File Format

The LICENSE file is a standard **X.509 certificate** (PEM format). It has no file extension — just named `LICENSE`. It can be inspected with standard `openssl` tools.

### Default Location

`/etc/krakend/LICENSE` — resolved as `./LICENSE` relative to KrakenD's working directory (default: `/etc/krakend`).

### Customizing the License Path

```bash
# Environment variable — path to license file
export KRAKEND_LICENSE_PATH=/path/to/prod_license.crt

# Environment variable — base64-encoded license content (useful for Secrets Manager injection)
export KRAKEND_LICENSE_BASE64="$(base64 -w0 LICENSE)"

# Command-line flag (takes precedence over env var)
krakend run -c krakend.json --license /my/dir/DEVELOPMENT_LICENSE
```

> **Convention:** Production licenses are named `LICENSE`, development/trial licenses are named `LICENSE_DEV`.

### License CLI Commands

```bash
# Check if license is valid
krakend license
# OK: License is still valid
# KO: x509: certificate has expired or is not yet valid...

# Check expiration date
krakend license valid-until
# OK: License is valid until 2024-02-01 00:00:00 +0000 UTC

# Check remaining validity
krakend license valid-for
# OK: License is valid for the next 71 days, 6 hours, and 1 minutes

# Check a license at a custom path
krakend license --license /path/to/devel_license
```

### What Happens When the License Expires

- **Running EE processes will shut down** when the license expires
- KrakenD EE **will not start** with an expired, incorrect, or missing LICENSE file
- Downgrade to CE is straightforward — switch to the `krakend/krakend` image (CE) without Enterprise features

### Automated Expiration Checks

**Preferred approach — native CLI** (simplest, cross-platform):

```bash
# Check remaining validity (no args = print human-readable remaining time)
krakend license valid-for
# OK: License is valid for the next 71 days, 6 hours, and 1 minutes

# For CI/CD scripting, parse the output or use the openssl fallback below
```

**Alternative — openssl-based script** (when KrakenD binary is unavailable in CI):

```bash
#!/bin/sh
WARN_IN_DAYS=30
LICENSE_FILE_PATH="./LICENSE"
set -e
NOW=$(date +%s)
EXPIRATION=$(openssl x509 -in $LICENSE_FILE_PATH -text -noout -dates \
  | grep notAfter | sed -e 's#notAfter=##')
EXPIRATION_TIMESTAMP=$(date -d "$EXPIRATION" +%s)
EXPIRATION_IN_DAYS=$(echo "($EXPIRATION_TIMESTAMP - $NOW)/(3600*24)" | bc)
HAS_EXPIRED=$(echo "$NOW > $EXPIRATION_TIMESTAMP" | bc)
ABOUT_TO_EXPIRE=$(echo "$EXPIRATION_IN_DAYS < $WARN_IN_DAYS" | bc)
echo "License expiration: $EXPIRATION (in $EXPIRATION_IN_DAYS days)"
if [ "1" = "$HAS_EXPIRED" ]; then
  echo "LICENSE expired — KrakenD Enterprise cannot start!"; exit 1
fi
if [ "1" = "$ABOUT_TO_EXPIRE" ]; then
  echo "LICENSE will expire soon"; exit 1
fi
```

> **Note:** `date -d` is Linux-only. For macOS CI, use the native CLI approach above.

### Storing LICENSE in Kubernetes

Options for the operator:
- **Kubernetes Secret** — Mount as a volume at `/etc/krakend/LICENSE`
- **`KRAKEND_LICENSE_BASE64` env var** — Inject base64-encoded license content via AWS Secrets Manager, HashiCorp Vault, or External Secrets Operator
- **CSI Secret Store Driver** — Mount from Vault/AWS SM via CSI volume
- **Baked into Docker image** — Less flexible but simplest for immutable deployments

### License Renewal

Replace `/etc/krakend/LICENSE` with the new file content and restart KrakenD. The operator should automate this via Secret rotation.

---

## 14. Configuration Reload & Zero-Downtime Strategies

### KrakenD's Restart Requirement

KrakenD loads its entire configuration at startup and **never re-reads it at runtime**. This is by design for performance — routes and decision trees are pre-computed during startup. Any configuration change requires a process restart.

### The `:watch` Docker Image (Development Only)

Both CE and EE provide a `:watch` Docker tag that wraps KrakenD in a [reflex](https://github.com/cespare/reflex) file watcher. When config files change, it automatically restarts KrakenD.

**NOT recommended for production** because:
- Kills the server **without graceful shutdown** — active connections are interrupted immediately
- An invalid config save leaves the server unable to restart
- Significant throughput decrease due to the watcher wrapper overhead
- Decreased binary stability (unexpected panics/exits)

Pre-built `:watch` Docker tags are available for EE (`krakend/krakend-ee:watch`). CE users must build the `:watch` image from the [krakend-watch](https://github.com/krakend/krakend-watch) repository — the CE `:watch` tag is no longer published to Docker Hub.

### Production Zero-Downtime: Blue/Green Rolling Deployment

The recommended production approach for config changes:

1. Generate the new configuration (rendered `krakend.json` from flexible config)
2. Validate with `krakend check -tlc krakend.json` (the `-l` flag enables JSON schema linting to catch wrong keys/types)
3. Update the ConfigMap/Secret with the new config
4. Trigger a **rolling deployment** — Kubernetes spins up new pods with the new config while old pods continue serving traffic
5. Old pods drain connections and terminate after new pods pass health checks

This ensures **zero downtime** and is the standard pattern used by Nginx, Varnish, Apache, and all other stateless reverse proxies.

---

## 15. Complete Configuration Example

```json
{
  "$schema": "https://www.krakend.io/schema/v2.13/krakend.json",
  "version": 3,
  "port": 8080,
  "timeout": "3s",
  "cache_ttl": "300s",
  "host": ["http://default-backend:8080"],
  "extra_config": {
    "telemetry/logging": {
      "level": "INFO",
      "prefix": "[GW]",
      "stdout": true,
      "format": "logstash"
    },
    "security/cors": {
      "allow_origins": ["https://app.example.com"],
      "allow_methods": ["GET", "POST", "PUT", "DELETE"],
      "allow_headers": ["Authorization", "Content-Type"],
      "max_age": "12h"
    },
    "security/http": {
      "frame_deny": true,
      "content_type_nosniff": true,
      "browser_xss_filter": true,
      "sts_seconds": 31536000
    },
    "router": {
      "return_error_msg": true,
      "health_path": "/health",
      "auto_options": true
    }
  },
  "endpoints": [
    {
      "endpoint": "/api/v1/users/{id}",
      "method": "GET",
      "input_headers": ["Authorization"],
      "extra_config": {
        "auth/validator": {
          "alg": "RS256",
          "jwk_url": "https://idp.example.com/.well-known/jwks.json",
          "cache": true,
          "audience": ["https://api.example.com"]
        },
        "qos/ratelimit/router": {
          "max_rate": 1000,
          "client_max_rate": 50,
          "strategy": "ip"
        }
      },
      "backend": [
        {
          "host": ["http://user-service:8080"],
          "url_pattern": "/users/{id}",
          "allow": ["id", "name", "email", "role"],
          "mapping": {"id": "user_id"},
          "extra_config": {
            "qos/circuit-breaker": {
              "interval": 60,
              "timeout": 15,
              "max_errors": 5,
              "log_status_change": true
            },
            "qos/ratelimit/proxy": {
              "max_rate": 200,
              "capacity": 200
            }
          }
        }
      ]
    },
    {
      "endpoint": "/api/v1/dashboard",
      "method": "GET",
      "timeout": "5s",
      "concurrent_calls": 2,
      "input_headers": ["Authorization"],
      "backend": [
        {
          "host": ["http://user-service:8080"],
          "url_pattern": "/users/me",
          "group": "user",
          "allow": ["name", "email"]
        },
        {
          "host": ["http://analytics-service:8080"],
          "url_pattern": "/stats/overview",
          "group": "analytics",
          "target": "data",
          "extra_config": {
            "qos/http-cache": {}
          }
        }
      ]
    }
  ]
}
```

---

## 16. Operator Design Implications

Key considerations when building a Kubernetes Operator for KrakenD:

### Configuration Management

1. **Config is the API** — KrakenD has no runtime API to add/remove endpoints. All changes require regenerating/updating the configuration and restarting the gateway.

2. **Flexible Config as the merge mechanism** — The operator should maintain settings files (JSON) in the `FC_SETTINGS` directory. Each CRD endpoint translates to an entry in a settings JSON file. The main template (`krakend.tmpl`) iterates over these with `{{ range }}`.

3. **Schema validation** — Use `krakend check -tlc krakend.json` to validate generated configs (the `-l`/`--lint` flag validates against the official JSON schema, catching unknown keys and wrong types). Run this in an init container or as a pre-deployment step.

4. **ConfigMap or init container patterns** — The operator can:
   - Generate a ConfigMap with the rendered `krakend.json` and trigger a rolling restart
   - Use an init container that assembles flexible config files from CRD state
   - Generate an immutable image in CI/CD from CRD-derived configs

### Zero-Downtime Dynamic Deployment

5. **Rolling deployment for config changes** — KrakenD loads config at startup only. Use Kubernetes Deployment rolling update strategy. The operator should:
   - Render the new `krakend.json` from CRD state
   - Validate it (`krakend check`)
   - Update the ConfigMap/Secret
   - Annotate the Deployment pod template (e.g., `checksum/config: <sha256>`) to trigger a rolling update
   - Kubernetes handles the rest: new pods start with new config, old pods drain and terminate

6. **Never use `:watch` in production** — The `:watch` image is development-only. It kills the server non-gracefully, causes 10-30% throughput loss, and can leave the gateway unable to restart if a broken config is written. The operator must use proper Kubernetes rolling deployments instead.

7. **Blue/green for major changes** — For large-scale config changes (TLS cert rotation, major endpoint overhaul), the operator can create a second Deployment, validate health, then switch the Service selector.

### Enterprise License Lifecycle

8. **License monitoring** — The operator should periodically check license validity using the KrakenD CLI (`krakend license valid-for`) or by parsing the X.509 certificate's `notAfter` date via `openssl`. Start alerting at a configurable threshold (e.g., 30 days before expiry).

9. **License rotation** — When a new license is available (via a Kubernetes Secret update, Vault, or AWS Secrets Manager), the operator should:
   - Update the Secret containing the LICENSE file
   - Trigger a rolling restart so pods pick up the new license
   - Verify the new pods start successfully before draining old pods

10. **Fallback to CE on license expiry** — When the EE license has expired and no renewal is available, the operator should:
    - Switch the Deployment container image from `krakend/krakend-ee` to `krakend/krakend` (CE)
    - Remove or adjust structural Enterprise-only features that CE cannot handle (e.g., wildcard endpoint paths `/*` which CE's router rejects). Enterprise-only `extra_config` namespaces (e.g., `security/policies`, `auth/api-keys`) are silently ignored by CE and do not need to be stripped.
    - Trigger a rolling deployment with the CE-compatible config
    - Set a status condition on the CRD (e.g., `LicenseDegraded=True`) to alert operators
    - **Note:** There is no CLI command to generate temporary licenses. Licenses are X.509 certificates issued by the KrakenD sales team and must be provisioned externally.

11. **Proactive, not reactive** — Since running EE processes **shut down immediately** when the license expires, the operator must act _before_ expiry, not after. Monitor the license at reconciliation time and take action while the gateway is still running.

### Infrastructure

12. **Stateless = simple** — No state migration, no database, no persistent volumes. Just mount or bake the config. KrakenD instances do not communicate with each other.

13. **Health checking** — Use `/__health` (or configured `health_path`) for liveness/readiness probes.

14. **Security context** — Always enforce `runAsUser: 1000`, `readOnlyRootFilesystem: true`, drop all capabilities. (`NET_BIND_SERVICE` is only needed if binding to privileged ports < 1024; port 8080 does not require it.)

---

## 17. Complete Documentation Page Index

> Every page in the KrakenD documentation sidebar (CE and EE) with a 1–2 sentence synopsis. Pages marked **[EE]** are Enterprise-only; all others apply to both CE and EE. URLs follow the pattern `https://www.krakend.io/docs/...` (CE) or `https://www.krakend.io/docs/enterprise/...` (EE).

### 17.1 Getting Started

| Page | Synopsis |
|------|----------|
| [Overview](https://www.krakend.io/docs/overview/) | High-level architecture, design philosophy (stateless, declarative, no coordination), and how the Lura framework underpins KrakenD. |
| [Installing KrakenD](https://www.krakend.io/docs/overview/installing/) | Installation methods: Docker images, `.deb`/`.rpm` packages, OS-specific binaries, and building from source. |
| [Running KrakenD](https://www.krakend.io/docs/overview/run/) | CLI usage (`krakend run`), flags (`-c`, `-p`, `-d`), and environment variable overrides for port, config path, etc. |
| [Verifying KrakenD](https://www.krakend.io/docs/overview/verifying-packages/) | Signature verification of downloaded packages using PGP keys to ensure integrity and authenticity. |
| [GUI: KrakenDesigner](https://www.krakend.io/docs/overview/gui/) | Browser-based configuration editor for building `krakend.json` visually; also available as a Docker container. |
| [Check command](https://www.krakend.io/docs/configuration/check/) | `krakend check -tlc krakend.json` validates syntax, detects unknown fields, and lints the configuration before deployment. |

### 17.2 Configuration Files

| Page | Synopsis |
|------|----------|
| [Understanding the configuration](https://www.krakend.io/docs/configuration/structure/) | Root JSON structure with `version`, `endpoints[]`, `extra_config{}`, and how nested objects compose the full gateway behavior. |
| [Supported config file formats](https://www.krakend.io/docs/configuration/supported-formats/) | Root config must be JSON; YAML/TOML/properties files are only usable as Flexible Config partials/settings. |
| [Flexible Configuration](https://www.krakend.io/docs/configuration/flexible-config/) | Go template system with `{{ include }}`, `{{ template }}`, `{{ marshal }}`, `{{ env }}` and Sprig functions to compose configs from multiple files and environment variables. |
| [Extended Flexible Configuration](https://www.krakend.io/docs/enterprise/configuration/flexible-config/) **[EE]** | Enterprise extension of Flexible Config adding `$ref`-based inline JSON includes, `flexible_config.json` behavioral file (replacing `FC_*` env vars), recursive directory scanning, and `allow_overwrite` for key merging. |
| [Environment variables](https://www.krakend.io/docs/configuration/environment-vars/) | `KRAKEND_PORT`, `FC_ENABLE`, `FC_SETTINGS`, `FC_PARTIALS`, `FC_TEMPLATES`, `FC_OUT` and all `USAGE_` telemetry env vars. |
| [Working directory](https://www.krakend.io/docs/configuration/working-directory/) | Sets the base directory for resolving relative paths in templates, Lua scripts, and external files (default: binary location). |
| [Clustering and multi-DC](https://www.krakend.io/docs/configuration/cluster/) | Strategies for running KrakenD across clusters/data centers; each instance is independent — no synchronization needed. |
| [Versioning & schema](https://www.krakend.io/docs/configuration/audit/) | JSON Schema validation (`$schema`), `krakend audit` command to evaluate best practices, and config version management. |
| [Comments & metadata](https://www.krakend.io/docs/configuration/comments/) | Use `@`-prefixed keys (e.g., `@comment`) for human-readable annotations; these are ignored by KrakenD. |
| [Generating config from code](https://www.krakend.io/docs/configuration/generating-config/) | Programmatic config generation using Go, Python, or other languages to emit the JSON configuration. |
| [Migrating to v2](https://www.krakend.io/docs/configuration/migrating/) | Step-by-step migration guide from KrakenD v1 to v2 (config version 3), covering namespace renames and breaking changes. |

### 17.3 Service Settings

| Page | Synopsis |
|------|----------|
| [Service Settings overview](https://www.krakend.io/docs/service-settings/overview/) | Root-level `extra_config` namespaces affecting the entire service: CORS, TLS, logging, router, HTTP server/transport, security headers. |
| [HTTP Server settings](https://www.krakend.io/docs/service-settings/http-server-settings/) | `max_header_bytes`, `read_timeout`, `write_timeout`, `idle_timeout`, `listen_address` and other HTTP server tuning at root `extra_config`. |
| [Redis Connection Pools](https://www.krakend.io/docs/enterprise/service-settings/redis-connection-pools/) **[EE]** | Centralized `redis` namespace defining named connection pools and clusters with tuning (pool size, retries, timeouts, TLS, OpenTelemetry) reused by quota, rate-limit, and other stateful components. |
| [Virtual Hosts](https://www.krakend.io/docs/enterprise/service-settings/virtual-hosts/) **[EE]** | Serve different endpoint trees based on the `Host` header, allowing multi-tenant or multi-domain gateway configurations on a single instance. |
| [Router options](https://www.krakend.io/docs/service-settings/router-options/) | `router` namespace controlling `return_error_msg`, `disable_redirect_fixed_path`, `auto_options`, `forwarded_by_client_ip`, health path, etc. |
| [HTTP Transport settings](https://www.krakend.io/docs/service-settings/http-transport-settings/) | Global client-side settings for upstream connections: `max_idle_connections`, `idle_connection_timeout`, `dialer_timeout`, `expect_continue_timeout`. |

> **Note:** CORS, TLS, HTTP Security headers, Service/Tiered Rate Limit, Gzip, and Response Headers Modifier are also service settings but are listed under their primary functional sections (17.7 Security, 17.9 Traffic Management, 17.6 Request/Response Manipulation) to avoid duplication.

### 17.4 Routing and Forwarding

| Page | Synopsis |
|------|----------|
| [The endpoint object](https://www.krakend.io/docs/endpoints/) | Structure of `endpoints[]` entries: `endpoint`, `method`, `backend[]`, `input_headers`, `input_query_strings`, `output_encoding`, `timeout`, `cache_ttl`, `extra_config`. |
| [The backend object](https://www.krakend.io/docs/backends/) | Structure of `backend[]` entries: `host`, `url_pattern`, `encoding`, `method`, `mapping`, `allow`/`deny`, `target`, `group`, `is_collection`, `extra_config`. |
| [Forwarding query strings and headers](https://www.krakend.io/docs/endpoints/parameter-forwarding/) | Allowlisting via `input_headers` and `input_query_strings`; default blocks all except explicitly listed. `["*"]` passes everything (discouraged). |
| [No-op (proxy only)](https://www.krakend.io/docs/endpoints/no-op/) | `"output_encoding": "no-op"` + `"encoding": "no-op"` passes request/response bodies without KrakenD inspection; no aggregation, no manipulation. |
| [Wildcard routes](https://www.krakend.io/docs/enterprise/endpoints/wildcard/) **[EE]** | Endpoints ending in `/*` forward all matching paths to backends; requires at least one directory level (`/v1/*` not `/*`); no multiple wildcards. CE cannot validate wildcard paths. |
| [Dynamic routing](https://www.krakend.io/docs/enterprise/endpoints/dynamic-routing/) **[EE]** | Inject `{input_headers.*}`, `{input_query_strings.*}`, `{JWT.*}` variables into backend `url_pattern` or `host` to construct upstream URLs dynamically from request data. REST-only (not gRPC/WS). |
| [CatchAll](https://www.krakend.io/docs/enterprise/endpoints/catch-all/) **[EE]** | Catch-all endpoint that forwards all unmatched traffic to a backend; complementary to wildcard for root-level path forwarding. |
| [Sequential Proxy](https://www.krakend.io/docs/endpoints/sequential-proxy/) | Chain backend calls sequentially — output of one becomes input to the next via `resp0_*`, `resp1_*` variables; namespace `proxy` with `"sequential": true`. |
| [Conditional Routing](https://www.krakend.io/docs/enterprise/backends/conditional/) **[EE]** | `backend/conditional` namespace to set policies (header match, CEL expressions, fallback) determining which backend is hit per request. |
| [Client redirects](https://www.krakend.io/docs/enterprise/backends/client-redirect/) **[EE]** | Allow KrakenD to follow HTTP redirects from backends (301/302) instead of passing them through to clients. |
| [URL Rewrite](https://www.krakend.io/docs/enterprise/endpoints/url-rewrite/) **[EE]** | Regex-based URL rewriting before routing to backends; transforms the incoming path before the router processes it. |
| [HTTP Proxy](https://www.krakend.io/docs/enterprise/backends/http-proxy/) **[EE]** | Forward-proxy support for upstream connections via HTTP/HTTPS/SOCKS5 proxies; set per-backend or globally. |
| [HTTP Per-backend Client settings](https://www.krakend.io/docs/backends/http-client/) | Override global HTTP transport settings per backend: custom timeouts, TLS, proxy, DNS. |
| [Concurrent Requests](https://www.krakend.io/docs/endpoints/concurrent-requests/) | `concurrent_calls` sends N parallel requests to a backend and returns the fastest successful response; improves P99 latency at the cost of extra load. |
| [Service Discovery](https://www.krakend.io/docs/backends/service-discovery/) | `sd` field: `static` (default), `dns` (SRV lookup), or custom resolvers for dynamic backend host resolution; `sd_scheme` defaults to `"http"`. |
| [Traffic shadowing/mirroring](https://www.krakend.io/docs/backends/shadow-backends/) | `shadow": true` sends a copy of requests to a backend without affecting the client response; used for testing new services in production. |
| [Conditional requests and responses (CEL)](https://www.krakend.io/docs/endpoints/common-expression-language-cel/) | CEL expressions in `validation/cel` to conditionally allow/deny requests or skip backends based on headers, params, JWT claims. |
| [Timeouts](https://www.krakend.io/docs/throttling/timeouts/) | Configuring `timeout` at endpoint and backend levels; cascading timeout hierarchy from service → endpoint → backend. |

### 17.5 Non-REST Connectivity

| Page | Synopsis |
|------|----------|
| [Introduction to Non-REST](https://www.krakend.io/docs/non-rest-connectivity/) | Overview of KrakenD's ability to connect to SOAP, gRPC, WebSockets, message queues, Lambda, and GraphQL as backends. |
| [SOAP integration](https://www.krakend.io/docs/enterprise/backends/soap/) **[EE]** | `backend/soap` namespace uses Go templates to craft XML request bodies from URL params/headers/body, proxying SOAP services as REST/JSON endpoints to clients. |
| [WebSockets](https://www.krakend.io/docs/enterprise/websockets/) **[EE]** | `websocket` endpoint namespace enabling RFC-6455 WebSocket proxying via multiplexing (single backend connection for all clients — default) or direct 1:1 communication. Supports connect/disconnect events, retries, backoff strategies, and Socket.IO compatibility. |
| [Introduction to gRPC](https://www.krakend.io/docs/enterprise/grpc/) **[EE]** | Overview of gRPC support covering both client (consume gRPC backends from REST endpoints) and server (accept gRPC requests) modes with proto-based transcoding. |
| [gRPC Client](https://www.krakend.io/docs/enterprise/backends/grpc/) **[EE]** | `backend/grpc` namespace to connect to upstream gRPC services, performing automatic JSON↔Protobuf transcoding using proto files or server reflection. |
| [gRPC Server](https://www.krakend.io/docs/enterprise/grpc/server/) **[EE]** | Accept incoming gRPC connections on KrakenD, transcoding to REST/JSON backends; requires proto file registration and service catalog configuration. |
| [Streaming and SSE](https://www.krakend.io/docs/enterprise/endpoints/streaming/) **[EE]** | Server-Sent Events (SSE) and streaming support: proxy backend event streams (`text/event-stream`) to clients without buffering the entire response. |
| [RabbitMQ Consumer](https://www.krakend.io/docs/backends/amqp/consumer/) | AMQP consumer backend that reads from RabbitMQ queues and returns messages as endpoint responses. |
| [RabbitMQ Producer](https://www.krakend.io/docs/backends/amqp/producer/) | AMQP producer backend that publishes user request bodies to RabbitMQ exchanges. |
| [Pub/Sub (Kafka, NATS, Cloud)](https://www.krakend.io/docs/backends/pubsub/) | `backend/pubsub` namespace supporting Google Cloud Pub/Sub, NATS, AWS SNS/SQS, Azure Service Bus, Kafka, and RabbitMQ as publisher/subscriber backends. |
| [Kafka Advanced PubSub](https://www.krakend.io/docs/enterprise/backends/pubsub/kafka/) **[EE]** | Enterprise Kafka integration with SASL authentication, consumer group management, offset control, TLS, and per-partition configuration. |
| [GraphQL](https://www.krakend.io/docs/backends/graphql/) | `backend/graphql` namespace that lets KrakenD act as a GraphQL client, sending queries/mutations to GraphQL backends and returning JSON. |
| [Lambda functions](https://www.krakend.io/docs/backends/lambda/) | Connect to AWS Lambda functions as backends; pass request data as the Lambda event payload and return the function response. |
| [Event-Driven Async Agents](https://www.krakend.io/docs/async/) | Autonomous consumer agents that subscribe to message queues (AMQP/Kafka) and trigger internal KrakenD endpoint pipelines from incoming events. |
| [AMQP Async Agent](https://www.krakend.io/docs/async/amqp/) | Async Agent driver for RabbitMQ/AMQP: consume from queues or exchanges and process messages through KrakenD's pipeline. |
| [Kafka Async Agent](https://www.krakend.io/docs/enterprise/async/kafka/) **[EE]** | Enterprise Kafka async agent with SASL, consumer groups, and advanced Kafka-specific configuration for event-driven processing. |

### 17.6 Request and Response Manipulation

| Page | Synopsis |
|------|----------|
| [Manipulation toolkit overview](https://www.krakend.io/docs/request-response-manipulation/) | Summary of all data transformation options: filtering (`allow`/`deny`), renaming (`mapping`), grouping (`group`), targeting (`target`), and advanced plugins. |
| [API Composition and aggregation](https://www.krakend.io/docs/endpoints/response-manipulation/) | Multiple backends per endpoint merge responses into a single JSON object; `X-KrakenD-Completed` header indicates full vs partial aggregation. |
| [Basic data manipulation](https://www.krakend.io/docs/backends/data-manipulation/) | `allow`, `deny`, `mapping`, `target`, `group`, and `is_collection` for filtering, renaming, extracting, and grouping backend response fields. |
| [Response manipulation with functions (Content Replacer)](https://www.krakend.io/docs/enterprise/endpoints/content-replacer/) **[EE]** | Regex or literal find-and-replace operations on response bodies using string functions; apply transformations declaratively without custom code. |
| [Response manipulation with queries (JMESPath)](https://www.krakend.io/docs/enterprise/endpoints/jmespath/) **[EE]** | `modifier/jmespath` namespace to query and reshape JSON responses using JMESPath expressions — powerful declarative JSON querying. |
| [Response manipulation with templates (Response Body Generator)](https://www.krakend.io/docs/enterprise/backends/response-body-generator/) **[EE]** | `modifier/response-body-generator` uses Go templates to completely rewrite the body returned by a backend before delivering to clients. |
| [Response from filesystem](https://www.krakend.io/docs/endpoints/serve-static-content/) | Serve static files (JSON, HTML, etc.) directly from the filesystem as endpoint responses without connecting to any backend. |
| [Response caching](https://www.krakend.io/docs/backends/caching/) | `qos/http-cache` namespace enabling in-memory or shared caching of backend responses with TTL-based expiration via `cache_ttl`. |
| [Response manipulation on arrays (Flatmap)](https://www.krakend.io/docs/backends/flatmap/) | `proxy` namespace with `flatmap_filter` to filter, move, delete, and rename elements within nested JSON arrays; requires endpoint with >1 backend. |
| [Response header modification](https://www.krakend.io/docs/enterprise/service-settings/response-headers-modifier/) **[EE]** | Add/remove/overwrite response headers at service, endpoint, or backend level. |
| [Request body field extraction](https://www.krakend.io/docs/enterprise/endpoints/request-body-extractor/) **[EE]** | Extract specific fields from the incoming request body and make them available as URL parameters for backend routing. |
| [Request manipulation with templates (Body Generator)](https://www.krakend.io/docs/enterprise/backends/body-generator/) **[EE]** | `modifier/request-body-generator` uses Go templates to craft the request body sent to backends using data from URL params, headers, query strings, and the original body. Supports Sprig functions. |
| [Request enrichment with GeoIP](https://www.krakend.io/docs/enterprise/endpoints/geoip/) **[EE]** | Enrich requests with geolocation data (country, city, coordinates) based on client IP, using a MaxMind GeoIP2 database. |
| [Static request/response manipulation (Martian)](https://www.krakend.io/docs/backends/martian/) | `modifier/martian` namespace for static header/URL/body modifications using Martian modifier DSL; supports request and response scopes. |
| [Origin response formats (Backend Encodings)](https://www.krakend.io/docs/backends/supported-encodings/) | Supported backend `encoding` values: `json`, `safejson`, `xml`, `rss`, `string`, `no-op`, `fast-json` **[EE]**, `yaml` **[EE]**. |
| [Returned response formats (Content Types)](https://www.krakend.io/docs/endpoints/content-types/) | `output_encoding` options: `json`, `json-collection`, `xml`, `string`, `no-op`, `fast-json` **[EE]**, `yaml` **[EE]**. |
| [Gzip compression](https://www.krakend.io/docs/enterprise/service-settings/gzip/) **[EE]** | Compress responses with gzip before sending to clients. |
| [Status Codes](https://www.krakend.io/docs/endpoints/status-codes/) | How KrakenD determines the HTTP status code returned to clients; behavior with `no-op`, multi-backend aggregation, and error scenarios. |
| [Strategies to return headers and errors](https://www.krakend.io/docs/backends/detailed-errors/) | `return_error_details`, `return_error_code`, `return_error_msg` for controlling how much backend error information reaches clients. |
| [Static responses on failures (Stubs)](https://www.krakend.io/docs/endpoints/static-proxy/) | `proxy` namespace with `static` object returning hardcoded response data when backends fail, acting as a fallback. |
| [JSON Schema request validation](https://www.krakend.io/docs/endpoints/json-schema/) | `validation/json-schema` to validate incoming request bodies against a JSON Schema before forwarding to backends. Available in CE and EE. |
| [JSON Schema response validation](https://www.krakend.io/docs/enterprise/endpoints/response-schema-validator/) **[EE]** | Validate backend response bodies against a JSON Schema; reject non-conforming responses before returning to the client. |
| [Maximum request size](https://www.krakend.io/docs/enterprise/endpoints/maximum-request-size/) **[EE]** | `validation/max-request-size` sets a byte limit on incoming request bodies per endpoint, protecting backends from oversized payloads. |
| [Workflows](https://www.krakend.io/docs/enterprise/endpoints/workflows/) **[EE]** | Chain multiple backend operations with branching logic, conditional execution, and data passing between steps — a declarative orchestration engine inside KrakenD. |

### 17.7 Security

| Page | Synopsis |
|------|----------|
| [Security Overview](https://www.krakend.io/docs/security/overview/) | Summary of KrakenD's security features: TLS, CORS, HTTP security headers, bot detection, and authentication integrations. |
| [CORS](https://www.krakend.io/docs/service-settings/cors/) | Cross-Origin Resource Sharing configuration under `security/cors`: `allow_origins`, `allow_methods`, `max_age`, `allow_credentials`. |
| [TLS/HTTPS](https://www.krakend.io/docs/service-settings/tls/) | Server TLS (`tls`), client TLS for backends (`client_tls`), mutual TLS (mTLS), cipher suites, curve preferences, and minimum TLS version. |
| [HTTP Security headers](https://www.krakend.io/docs/service-settings/security/) | HSTS, CSP, X-Content-Type-Options, X-Frame-Options, Referrer-Policy, and Permissions-Policy via `security/http`. |
| [FIPS-140](https://www.krakend.io/docs/enterprise/security/fips-140/) **[EE]** | Enterprise FIPS-140-2 compliant build using BoringCrypto; enables organizations meeting federal/government security requirements. |
| [Security Policies Engine](https://www.krakend.io/docs/enterprise/security-policies/) **[EE]** | CEL-based policy engine for runtime validation on requests (`req`), responses (`resp`), and JWT claims (`jwt`); evaluate custom RBAC/ABAC rules in nanoseconds. Namespace `security/policies` at endpoint or backend level. |
| [Security Policy language](https://www.krakend.io/docs/enterprise/security-policies/policy-language/) **[EE]** | CEL expression syntax for writing security policies: variables (`req_params`, `req_headers`, `JWT`), operators, and built-in functions. |
| [Security Policy macros](https://www.krakend.io/docs/enterprise/security-policies/advanced-policy-macros/) **[EE]** | Advanced macros extending CEL with convenience functions like `hasQuerystring()`, `hasHeader()`, `isAlphanumeric()`, `matches()`, time functions. |
| [CEL built-in functions](https://www.krakend.io/docs/enterprise/security-policies/built-in-functions/) **[EE]** | Complete reference of built-in CEL functions: string manipulation, crypto, time, IP ranges, regex, URL parsing, etc. |
| [Security Policies Playbook](https://www.krakend.io/docs/enterprise/security-policies/playbook/) **[EE]** | Ready-to-use policy examples: time-based access, IP filtering, geo-restrictions, mandatory headers, claim validation recipes. |

### 17.8 Authentication & Authorization

| Page | Synopsis |
|------|----------|
| [Intro to AuthZ and AuthN](https://www.krakend.io/docs/authorization/) | Overview of authentication/authorization capabilities: JWT validation, signing, token revocation, OAuth2, API keys, and identity provider integrations. |
| [API-Key Authentication](https://www.krakend.io/docs/enterprise/authentication/api-keys/) **[EE]** | `auth/api-keys` with role-based access control (RBAC), key hashing (SHA256/FNV128/SHA1), per-key rate limits (`client_max_rate`), and header/query-string strategies. Keys declared at service level, attached per endpoint. |
| [Basic Authentication](https://www.krakend.io/docs/enterprise/authentication/basic-authentication/) **[EE]** | HTTP Basic Authentication support with hashed passwords for simple username/password protection. |
| [JWT Overview](https://www.krakend.io/docs/authorization/jwt-overview/) | Conceptual overview of JSON Web Token flow in KrakenD: validation → claim propagation → signing of new tokens. |
| [JWT Validation](https://www.krakend.io/docs/authorization/jwt-validation/) | `auth/validator` namespace: `alg`, `jwk_url`, `issuer`, `audience`, `roles_key`, `propagate_claims`, `cache`, `key_identify_strategy` (including `x5t#S256`), `shared_cache_duration` (service level). |
| [JWK Key Caching](https://www.krakend.io/docs/authorization/jwk-caching/) | How KrakenD caches JWK keys from the `jwk_url`; `cache_duration`, `shared_cache_duration` for cross-endpoint caching at service level. |
| [JWT Signing](https://www.krakend.io/docs/authorization/jwt-signing/) | `auth/signer` namespace to create and sign new JWT tokens from backend responses; supports `HS256`, `RS256`, and `ES256`. |
| [Revoking tokens](https://www.krakend.io/docs/authorization/revoking-tokens/) | Token revocation using bloom filters; `auth/revoker` namespace with in-memory or Redis-backed storage for token blacklisting. |
| [Revoke Server](https://www.krakend.io/docs/enterprise/authentication/revoke-server/) **[EE]** | Dedicated revocation server companion for real-time token invalidation across KrakenD nodes via push notifications. |
| [Multiple Identity Providers](https://www.krakend.io/docs/enterprise/authentication/multiple-identity-providers/) **[EE]** | Configure endpoint-level JWT validation with multiple `jwk_url` sources, allowing different IdPs (Auth0 + Okta, etc.) on the same endpoint. |
| [Mutual TLS Authentication (mTLS)](https://www.krakend.io/docs/authorization/mutual-authentication/) | Client certificate validation for mTLS: `client_tls` with `ca_certs`, enforced at the gateway listener level. |
| [OAuth2 Client Credentials](https://www.krakend.io/docs/authorization/client-credentials/) | `auth/client-credentials` for machine-to-machine auth: KrakenD obtains and caches OAuth2 tokens to call protected backends on behalf of the gateway. |
| [Google Cloud Authentication](https://www.krakend.io/docs/enterprise/authentication/gcloud/) **[EE]** | Automatic Google Cloud service account authentication for backends hosted on GCP (Cloud Run, Cloud Functions, etc.). |
| [AWS SigV4 Authentication](https://www.krakend.io/docs/enterprise/authentication/aws-sigv4/) **[EE]** | Sign upstream requests with AWS Signature V4 for accessing AWS services (API Gateway, Lambda, S3) without managing credentials in clients. |
| [NTLM Authentication](https://www.krakend.io/docs/enterprise/authentication/ntlm/) **[EE]** | NTLM authentication support for connecting to legacy Windows-based backend services requiring NTLM handshake. |
| [Auth0 integration](https://www.krakend.io/docs/authorization/auth0/) | Step-by-step guide for integrating Auth0 as the JWT provider with KrakenD. |
| [Keycloak integration](https://www.krakend.io/docs/authorization/keycloak/) | Configuration examples for using Keycloak as the OpenID Connect provider. |
| [Descope integration](https://www.krakend.io/docs/authorization/descope/) | Integration guide for Descope as an identity management provider. |

### 17.9 Traffic Management

| Page | Synopsis |
|------|----------|
| [Traffic Management overview](https://www.krakend.io/docs/throttling/) | Summary of all rate limiting, circuit breaking, timeout, and load-shedding options available in CE and EE. |
| [Endpoint Rate Limit](https://www.krakend.io/docs/endpoints/rate-limit/) | `qos/ratelimit/router` namespace: `max_rate` (global) and `client_max_rate` (per-client) at endpoint level; `key_identify_strategy` for user identification. Note: `capacity` defaults to `max_rate` or `client_max_rate` (not 1). |
| [Backend/Proxy Rate Limit](https://www.krakend.io/docs/backends/rate-limit/) | `qos/ratelimit/proxy` namespace: rate-limit requests from KrakenD to specific backends to protect upstream services. |
| [Service Rate Limit](https://www.krakend.io/docs/enterprise/service-settings/service-rate-limit/) **[EE]** | Global service-level rate limit with optional Redis-backed shared counting across nodes. |
| [Tiered Rate Limit](https://www.krakend.io/docs/enterprise/service-settings/tiered-rate-limit/) **[EE]** | Multi-tier rate limiting with different limits per API key role, JWT claim, or token type. |
| [Redis-backed Rate Limits](https://www.krakend.io/docs/enterprise/throttling/global-rate-limit/) **[EE]** | Redis-backed distributed rate limiting using the shared `redis` connection pool for cluster-wide enforcement. |
| [IP Filtering](https://www.krakend.io/docs/enterprise/throttling/ip-filtering/) **[EE]** | Allow/deny access based on client IP addresses or CIDR ranges; supports trusted proxies and `X-Forwarded-For`. |
| [Circuit Breaker](https://www.krakend.io/docs/backends/circuit-breaker/) | `qos/circuit-breaker` namespace: `interval`, `timeout`, `max_errors`, `log_status_change`; opens circuit on error threshold; success requires HTTP 200 or 201. |
| [HTTP Circuit Breaker](https://www.krakend.io/docs/enterprise/backends/circuit-breaker/) **[EE]** | Enterprise circuit breaker with extended status code configuration and customizable success/failure definitions. |
| [Bot Detector](https://www.krakend.io/docs/throttling/botdetector/) | `security/bot-detector` namespace to block requests from known bots based on User-Agent patterns; CE feature with allowlist/denylist. |
| [Timeouts](https://www.krakend.io/docs/throttling/timeouts/) | Configuring `timeout` at service, endpoint, and backend levels; cascading timeout hierarchy. |

### 17.10 AI Gateway

| Page | Synopsis |
|------|----------|
| [AI Gateway Overview](https://www.krakend.io/docs/ai-gateway/) | Introduction to KrakenD as an AI Gateway: unified LLM interface, vendor-agnostic routing, token monitoring, prompt governance, and cost control. |
| [AI Gateway Security](https://www.krakend.io/docs/enterprise/ai-gateway/security/) **[EE]** | Security features specific to AI workloads: prompt injection prevention, sensitive data masking, and access control for AI endpoints. |
| [AI Budget Control](https://www.krakend.io/docs/enterprise/ai-gateway/budget-control/) **[EE]** | Token usage monitoring, budget enforcement, and cost control using Redis-backed `governance/quota` with `weight_key` for LLM token counting. |
| [AI Governance](https://www.krakend.io/docs/enterprise/ai-gateway/governance/) **[EE]** | Prompt policy enforcement, response guardrails, rate limiting per tenant/team, and prompt validation templates for responsible AI deployment. |
| [Unified LLM Interface](https://www.krakend.io/docs/enterprise/ai-gateway/unified-llm-interface/) **[EE]** | `ai/llm` namespace providing vendor-agnostic request/response format; KrakenD auto-translates between KrakenD's unified format and vendor-specific APIs (OpenAI, Gemini, Mistral, Anthropic, Bedrock). |
| [MCP Gateway](https://www.krakend.io/docs/enterprise/ai-gateway/mcp-gateway/) **[EE]** | KrakenD as a transparent `no-op` proxy between MCP agents and upstream MCP servers, adding security, auth, rate-limiting, and monitoring layers without modifying the MCP protocol payload. |
| [MCP Server](https://www.krakend.io/docs/enterprise/ai-gateway/mcp-server/) **[EE]** | Expose any KrakenD-connected service as MCP tools for AI agents; `ai/mcp` root config declares servers/tools with input/output schemas, linked to MCP entrypoint endpoints. |
| [LLM Routing](https://www.krakend.io/docs/enterprise/ai-gateway/llm-routing/) **[EE]** | Strategies for routing AI requests: direct proxy, conditional routing (header/JWT claim/policy-based), path-based, and multi-LLM aggregation. |
| [OpenAI](https://www.krakend.io/docs/enterprise/ai-gateway/openai/) **[EE]** | Provider-specific configuration for OpenAI (GPT-4, GPT-5): credentials, model selection, streaming, and variable injection. |
| [Google Gemini](https://www.krakend.io/docs/enterprise/ai-gateway/gemini/) **[EE]** | Provider-specific configuration for Google Gemini: API key, model versions, candidates, and safety settings. |
| [Mistral](https://www.krakend.io/docs/enterprise/ai-gateway/mistral/) **[EE]** | Provider-specific configuration for Mistral AI models. |
| [Anthropic](https://www.krakend.io/docs/enterprise/ai-gateway/anthropic/) **[EE]** | Provider-specific configuration for Anthropic Claude models. |
| [AWS Bedrock](https://www.krakend.io/docs/enterprise/ai-gateway/bedrock/) **[EE]** | Provider-specific configuration for AWS Bedrock: IAM-based authentication, model invocation, and region-specific configuration. |
| [Other AI Vendors](https://www.krakend.io/docs/enterprise/ai-gateway/other-vendors/) **[EE]** | How to connect to any OpenAI-compatible API or custom LLM endpoint not natively supported. |
| [AI Monitoring](https://www.krakend.io/docs/enterprise/ai-gateway/usage-monitoring/) **[EE]** | Token usage monitoring, cost tracking, and alerting for AI workloads using OpenTelemetry metrics and logs. |

### 17.11 Governance and Monetization

| Page | Synopsis |
|------|----------|
| [Usage Quota](https://www.krakend.io/docs/enterprise/governance/quota/) **[EE]** | Redis-backed persistent quota system for API monetization: 3-part config (Redis pool → `governance/processors` with named rules/limits per tier → `governance/quota` attachment at service/endpoint/backend). Supports long-term limits (hourly/daily/weekly/monthly/yearly), multi-tier enforcement, `weight_key` for token-based counting, `rejecter_cache` (bloom filter), and `X-Quota-Limit`/`X-Quota-Remaining` response headers. Minimum Redis 7.4 required. |
| [Moesif — Analytics and Monetization](https://www.krakend.io/docs/enterprise/governance/moesif/) **[EE]** | Integration with Moesif for API analytics, user tracking, behavior analysis, and monetization; sends event data to Moesif's platform. |

### 17.12 Monitoring, Logs, and Analytics

| Page | Synopsis |
|------|----------|
| [Logging](https://www.krakend.io/docs/logging/) | `telemetry/logging` namespace: `level` (required: `DEBUG`/`WARNING`/`ERROR`/`CRITICAL`), `prefix`, `format` (default: `"default"`, or `"logstash"`), `syslog`, `stdout`. |
| [Access logs (extended logging)](https://www.krakend.io/docs/logging/extended-logging/) | `router` namespace setting `disable_access_log` to control Apache-style access logs with request details (method, path, status, duration). |
| [Audit command](https://www.krakend.io/docs/logging/audit/) | `krakend audit` evaluates the configuration against best practices and produces a score/report. |
| [OpenTelemetry](https://www.krakend.io/docs/telemetry/opentelemetry/) | `telemetry/opentelemetry` namespace: configure OTLP exporters for traces, metrics, and logs; supports gRPC and HTTP protocols. |
| [OpenTelemetry by endpoint](https://www.krakend.io/docs/telemetry/opentelemetry-by-endpoint/) | Per-endpoint OpenTelemetry configuration to override global settings or add custom attributes. |
| [OpenTelemetry for backends](https://www.krakend.io/docs/telemetry/opentelemetry-for-backends/) | Per-backend OTel configuration for tracing individual upstream service calls. |
| [OpenTelemetry SaaS Auth](https://www.krakend.io/docs/enterprise/telemetry/opentelemetry-security/) **[EE]** | OAuth2/API-key authentication for OTLP exporters when sending to SaaS vendors (Datadog, Honeycomb, etc.). |
| [Grafana dashboards](https://www.krakend.io/docs/telemetry/grafana/) | Pre-built Grafana dashboards for KrakenD: endpoint latency, error rates, throughput, and Redis connection pool metrics. |
| [Jaeger](https://www.krakend.io/docs/telemetry/jaeger/) | Configuration for exporting traces to Jaeger via OpenTelemetry or the legacy OpenCensus exporter. |
| [Prometheus](https://www.krakend.io/docs/telemetry/prometheus/) | Expose Prometheus metrics endpoint (`/__stats/`) for scraping; integration with OpenTelemetry Prometheus exporter. |
| [Datadog](https://www.krakend.io/docs/telemetry/datadog/) | Configuration for Datadog APM traces and metrics collection from KrakenD. |
| [AWS X-Ray](https://www.krakend.io/docs/telemetry/xray/) | Export traces to AWS X-Ray using the OpenTelemetry OTLP exporter. |
| [Azure Monitor](https://www.krakend.io/docs/telemetry/azure/) | Send telemetry to Azure Monitor / Application Insights via OpenTelemetry. |
| [Google Cloud Monitoring](https://www.krakend.io/docs/telemetry/google-cloud/) | Export metrics and traces to Google Cloud Operations (formerly Stackdriver). |
| [New Relic](https://www.krakend.io/docs/enterprise/telemetry/newrelic/) **[EE]** | Direct New Relic integration for distributed tracing and APM metrics. |
| [Logstash / ELK](https://www.krakend.io/docs/telemetry/logstash/) | Configure `"format": "logstash"` in logging for JSON-structured logs compatible with the ELK stack. |
| [InfluxDB](https://www.krakend.io/docs/telemetry/influxdb/) | Legacy metrics exporter to InfluxDB (via OpenCensus, deprecated in favor of OpenTelemetry). |
| [OpenCensus (deprecated)](https://www.krakend.io/docs/telemetry/opencensus/) | Legacy `telemetry/opencensus` exporter; deprecated in favor of `telemetry/opentelemetry`. Still functional but not recommended for new deployments. |
| [HTTP Logger](https://www.krakend.io/docs/enterprise/developer/http-logger/) **[EE]** | Development tool that logs all request/response data to a file or stdout for debugging; not for production use. |

### 17.13 API Documentation and Dev Tools

| Page | Synopsis |
|------|----------|
| [OpenAPI import & export](https://www.krakend.io/docs/enterprise/developer/openapi/) **[EE]** | Import OpenAPI 3.0 specs to auto-generate KrakenD endpoint configuration; export KrakenD config back to OpenAPI for documentation. |
| [Postman](https://www.krakend.io/docs/enterprise/developer/postman/) **[EE]** | Import Postman collections to generate KrakenD configuration. |
| [Config2dot (PNG/SVG)](https://www.krakend.io/docs/enterprise/developer/config2dot/) **[EE]** | `krakend config2dot` generates a Graphviz DOT diagram visualizing all endpoints and backend connections; render as PNG/SVG. |
| [Debug endpoint](https://www.krakend.io/docs/developer/debug-endpoint/) | `"debug_endpoint": true` enables `/__debug/*` endpoints that echo back received headers, body, and params for development testing. |
| [IDE integration](https://www.krakend.io/docs/developer/ide-integration/) | JSON Schema integration for VS Code and JetBrains IDEs providing autocompletion and validation for `krakend.json`. |

### 17.14 Extending with Custom Code

| Page | Synopsis |
|------|----------|
| [Extending KrakenD overview](https://www.krakend.io/docs/extending/) | Extension mechanisms: Lua scripting, Go plugins (`http-server`, `http-client`, `req-resp-modifier`), and custom middleware. |
| [Lua Scripting](https://www.krakend.io/docs/extending/lua/) | Embed Lua scripts via `modifier/lua-endpoint` (endpoint or service level), `modifier/lua-proxy` (endpoint), `modifier/lua-backend` (backend) for lightweight request/response modifications without compiling plugins. |
| [Lua Advanced Helpers](https://www.krakend.io/docs/enterprise/extending/lua-advanced-helpers/) **[EE]** | Enterprise Lua extensions: additional helper functions, HTTP client calls from Lua, and Redis access within Lua scripts. |
| [Go Plugins overview](https://www.krakend.io/docs/extending/plugin-overview/) | How Go plugins (`.so` files) work: compile against matching KrakenD version/OS, register via `plugin` root config and per-component `plugin/` namespaces. |
| [HTTP Server plugins](https://www.krakend.io/docs/extending/http-server-plugins/) | `plugin/http-server` intercepts incoming requests before KrakenD processing; use for custom auth, rate limiting, or request transformation. |
| [HTTP Client plugins](https://www.krakend.io/docs/extending/http-client-plugins/) | `plugin/http-client` modifies outgoing requests to backends; use for custom signing, header injection, or protocol adaptation. |
| [Request/Response Modifier plugins](https://www.krakend.io/docs/extending/plugin-modifiers/) | `plugin/req-resp-modifier` transforms request/response data at the middleware layer (between endpoint and backend). |
| [Middleware Plugins](https://www.krakend.io/docs/enterprise/extending/middleware-plugins/) **[EE]** | Enterprise plugin type for full middleware injection at the endpoint handler pipeline level. |
| [Plugin Generator](https://www.krakend.io/docs/enterprise/extending/plugin-generator/) **[EE]** | CLI scaffolding tool (`krakend generate plugin`) to create Go plugin project boilerplate with correct KrakenD version dependencies. |
| [Redis Injection in Plugins](https://www.krakend.io/docs/enterprise/extending/redis-injection/) **[EE]** | Access the shared Redis connection pool from within Go plugins for stateful operations like counters or caching. |
| [Quota Processors in Plugins](https://www.krakend.io/docs/enterprise/extending/quota-processors/) **[EE]** | Access the governance quota processor from within Go plugins to programmatically check/increment quotas. |
| [Injecting data from plugins](https://www.krakend.io/docs/extending/injecting-data/) | How to pass data between plugins and KrakenD's pipeline using context keys and headers. |

### 17.15 Deployment and Go-Live

| Page | Synopsis |
|------|----------|
| [Deployment overview](https://www.krakend.io/docs/deploying/) | Best practices for production deployment: bake config into Docker image, use health endpoints, and avoid `:watch` in production. |
| [Docker images](https://www.krakend.io/docs/deploying/docker/) | Official Docker images: `krakend/krakend` (CE), `krakend/krakend-ee` (EE), `:watch` tag for development auto-reload (CE must build from `krakend-watch` repo). |
| [Kubernetes](https://www.krakend.io/docs/deploying/kubernetes/) | Kubernetes deployment patterns: Deployment + ConfigMap + Service (ClusterIP); liveness/readiness probes on `/__health`; `runAsUser: 1000`. |
| [Blue/green deployments](https://www.krakend.io/docs/deploying/blue-green/) | Blue/green strategy: deploy new version alongside old, validate health, switch Service selector, drain old pods. |
| [Configuration reload](https://www.krakend.io/docs/deploying/hot-reload/) | `:watch` tag for development (inotify reloads on config change); production uses rolling deployments for zero-downtime config updates. |
| [Enterprise License](https://www.krakend.io/docs/enterprise/overview/license/) | Enterprise license management: X.509 certificate at `/etc/krakend/LICENSE`; `KRAKEND_LICENSE_PATH`, `KRAKEND_LICENSE_BASE64`, `--license` flag; `krakend license valid-for` CLI check; process shuts down on expiry. |

### 17.16 Benchmarks

| Page | Synopsis |
|------|----------|
| [Benchmarks overview](https://www.krakend.io/docs/benchmarks/) | Performance characteristics: sub-millisecond overhead, linear scaling, no coordination overhead, and official benchmark methodology. |
| [KrakenD vs Kong](https://www.krakend.io/docs/benchmarks/kong/) | Head-to-head latency and throughput comparison against Kong Gateway. |
| [KrakenD vs Envoy](https://www.krakend.io/docs/benchmarks/envoy/) | Performance comparison against Envoy proxy in API gateway mode. |
| [KrakenD vs Tyk](https://www.krakend.io/docs/benchmarks/tyk/) | Benchmark comparison against Tyk API Gateway. |

### 17.17 Design Principles and FAQ

| Page | Synopsis |
|------|----------|
| [Design principles](https://www.krakend.io/docs/design/) | KrakenD's philosophy: stateless, immutable, declarative, zero-trust by default, and performance-first design. |
| [Request lifecycle](https://www.krakend.io/docs/design/lifecycle/) | Step-by-step flow of a request through KrakenD: Router → Endpoint middleware → Proxy layer → Backend handlers → Response merge. |
| [Zero-trust API Gateway](https://www.krakend.io/docs/design/zero-trust/) | Default deny-all posture: no headers, query strings, or cookies pass through unless explicitly allowed. |
| [KrakenD vs API Management](https://www.krakend.io/docs/design/vs-api-management/) | Comparison of KrakenD's API Gateway approach vs. full API Management platforms (developer portals, catalogs, etc.). |
| [KrakenD vs REST](https://www.krakend.io/docs/design/vs-rest/) | How KrakenD differs from simple REST proxies: aggregation, transformation, and protocol translation. |
| [FAQ: General](https://www.krakend.io/docs/faq/) | Common questions about KrakenD: licensing, support channels, configuration limitations, and compatibility. |
| [FAQ: Troubleshooting](https://www.krakend.io/docs/faq/troubleshooting/) | Solutions to common issues: empty responses, connection refused, 503 errors, certificate problems, and log interpretation. |
