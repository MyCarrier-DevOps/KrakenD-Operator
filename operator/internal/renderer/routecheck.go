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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

func init() { //nolint:gochecknoinits // gin's debug mode prints every route it registers
	gin.SetMode(gin.ReleaseMode)
}

// errRouteConflict is the verdict for routes the KrakenD router would refuse,
// found before krakend check runs.
var errRouteConflict = errors.New("route conflict")

// gatewayRoute marks a route the gateway itself registers, not an endpoint.
const gatewayRoute = -1

// ginRoute is one registration KrakenD's router makes. index is the position
// of the endpoint in the config's endpoints array, or gatewayRoute.
type ginRoute struct {
	index  int
	method string
	path   string
	any    bool
}

// routeRefusal is a registration the router refuses; index as in ginRoute.
type routeRefusal struct {
	index   int
	message string
}

// line renders the refusal as one lint-style line. A refusal of the gateway's
// own route has no endpoint to point at, so it is not a pointer; r is the
// route being registered.
func (f routeRefusal) line(r ginRoute) string {
	if f.index == gatewayRoute {
		return fmt.Sprintf("- gateway route %s: %s", r.describe(), f.message)
	}
	return fmt.Sprintf("- at '/endpoints/%d/endpoint': %s", f.index, f.message)
}

// describe names the route's method and path; a route for every method reads
// "any method".
func (r ginRoute) describe() string {
	if r.any {
		return "any method " + r.path
	}
	return r.method + " " + r.path
}

// routedConfig is the part of a krakend.json the router reads.
type routedConfig struct {
	Debug       bool                       `json:"debug_endpoint"`
	Echo        bool                       `json:"echo_endpoint"`
	ExtraConfig map[string]json.RawMessage `json:"extra_config"`
	Endpoints   []struct {
		Endpoint string `json:"endpoint"`
		Method   string `json:"method"`
	} `json:"endpoints"`
}

// RouterOptions are the "router" extra_config keys that add routes.
type RouterOptions struct {
	HealthPath    string `json:"health_path"`
	DisableHealth bool   `json:"disable_health"`
	AutoOptions   bool   `json:"auto_options"`
}

// ParseRouterOptions reads a "router" extra_config block. A block that does
// not decode, for example one with a wrongly typed key, reads as the defaults:
// the schema lint reports it.
func ParseRouterOptions(block json.RawMessage) RouterOptions {
	var opts RouterOptions
	if json.Unmarshal(block, &opts) != nil {
		return RouterOptions{}
	}
	return opts
}

// HealthRoute returns the path the health endpoint is served on, or "" when
// it is disabled.
func (o RouterOptions) HealthRoute() string {
	switch {
	case o.DisableHealth:
		return ""
	case o.HealthPath != "":
		return o.HealthPath
	}
	return DefaultHealthPath
}

// DefaultHealthPath is where KrakenD serves its health endpoint unless told
// otherwise.
const DefaultHealthPath = "/__health"

// routeConflicts registers every route of doc in a gin engine, in the order
// the KrakenD runtime does, and returns one lint-pointer line per refused
// registration. It catches everything `krakend check -t` catches (-t
// registers the same endpoints in the same gin version), plus the routes -t
// never registers and the runtime panics on: the health endpoint and the
// auto_options routes.
func routeConflicts(doc []byte) ([]string, error) {
	var cfg routedConfig
	if err := json.Unmarshal(doc, &cfg); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, nil // a wrongly typed field is the schema lint's to report
		}
		return nil, fmt.Errorf("decoding config for the route check: %w", err)
	}
	var lines []string
	var accepted []ginRoute
	engine := gin.New()
	for _, r := range ginRoutesOf(cfg) {
		refusal := registerRoute(engine, r)
		if refusal == "" {
			accepted = append(accepted, r)
			continue
		}
		for _, f := range clashRefusals(accepted, r, refusal) {
			lines = append(lines, f.line(r))
		}
		// gin can leave its tree half-updated after refusing a route; rebuild it.
		engine = engineWith(accepted)
	}
	return lines, nil
}

// ginRoutesOf lists the routes the runtime registers, in its order: the
// health endpoint, the debug and echo endpoints, every endpoint, then one
// OPTIONS route per distinct path when auto_options is on.
func ginRoutesOf(cfg routedConfig) []ginRoute {
	opts := RouterOptions{}
	if raw, ok := cfg.ExtraConfig["router"]; ok {
		opts = ParseRouterOptions(raw)
	}
	var routes []ginRoute
	if health := opts.HealthRoute(); health != "" {
		routes = append(routes, ginRoute{index: gatewayRoute, method: http.MethodGet, path: health})
	}
	if cfg.Debug {
		routes = append(routes, ginRoute{index: gatewayRoute, path: "/__debug/*param", any: true})
	}
	if cfg.Echo {
		routes = append(routes, ginRoute{index: gatewayRoute, path: "/__echo/*param", any: true})
	}
	firstByPath := map[string]int{}
	for i, ep := range cfg.Endpoints {
		p := ginPath(ep.Endpoint)
		routes = append(routes, ginRoute{index: i, method: strings.ToUpper(ep.Method), path: p})
		if _, ok := firstByPath[p]; !ok {
			firstByPath[p] = i
		}
	}
	if opts.AutoOptions {
		paths := make([]string, 0, len(firstByPath))
		for p := range firstByPath {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			routes = append(routes, ginRoute{index: firstByPath[p], method: http.MethodOptions, path: p})
		}
	}
	return routes
}

// ginPath converts an endpoint path to the pattern KrakenD registers: a
// leading slash, and each extracted parameter "{name}" as ":name".
func ginPath(endpoint string) string {
	p := "/" + strings.TrimPrefix(endpoint, "/")
	for _, param := range PathParams(p) {
		p = strings.ReplaceAll(p, "{"+param+"}", ":"+param)
	}
	return p
}

// registerRoute adds r to engine and returns gin's refusal, or "" when gin
// accepts it.
func registerRoute(engine *gin.Engine, r ginRoute) (refusal string) {
	defer func() {
		if rec := recover(); rec != nil {
			refusal = fmt.Sprint(rec)
		}
	}()
	noop := func(*gin.Context) {}
	if r.any {
		engine.Any(r.path, noop)
	} else {
		engine.Handle(r.method, r.path, noop)
	}
	return ""
}

func engineWith(routes []ginRoute) *gin.Engine {
	engine := gin.New()
	for _, r := range routes {
		registerRoute(engine, r)
	}
	return engine
}

// clashRefusals reports refused route r and, when one accepted route alone
// clashes with it, that route too, so both endpoints are named.
func clashRefusals(accepted []ginRoute, r ginRoute, refusal string) []routeRefusal {
	out := []routeRefusal{{index: r.index, message: refusal}}
	if registerRoute(gin.New(), r) != "" {
		return out // r is refused on its own, so no accepted route is to blame
	}
	for _, a := range accepted {
		if registerRoute(engineWith([]ginRoute{a}), r) == "" {
			continue
		}
		if a.index == gatewayRoute {
			out[0].message = fmt.Sprintf("%s (%s is the gateway's own route)", refusal, a.describe())
		} else if a.index != r.index {
			out = append(out, routeRefusal{index: a.index,
				message: fmt.Sprintf("%s clashes with %s: %s", a.describe(), r.describe(), refusal)})
		}
		break
	}
	return out
}
