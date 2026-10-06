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
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRouteConflicts_ParameterClashNamesBothEndpoints(t *testing.T) {
	doc := `{"version":3,"endpoints":[
		{"endpoint":"/users/{id}","method":"GET"},
		{"endpoint":"/users/{userId}/orders","method":"GET"}]}`

	lines, err := routeConflicts(context.Background(), []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "- at '/endpoints/1/endpoint': ") ||
		!strings.HasPrefix(lines[1], "- at '/endpoints/0/endpoint': ") {
		t.Fatalf("lines = %q, want endpoint 1 (refused) then endpoint 0 (clashes with it)", lines)
	}
	if !strings.Contains(lines[0], "conflicts with existing wildcard ':id'") {
		t.Errorf("line = %q, want gin's refusal", lines[0])
	}
}

func TestRouteConflicts_MirrorsTheRuntimeRouter(t *testing.T) {
	tests := []struct {
		name, doc string
		refused   string // substring of the first line; "" means no lines
		blames    []int  // endpoint index each line points at, in order
	}{
		{"static beside parameter", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/static","method":"GET"}]}`, "", nil},
		{"trailing slash is distinct", `{"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/a/","method":"GET"}]}`, "", nil},
		{"methods have separate trees", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/{name}","method":"POST"}]}`, "", nil},
		{"parameter beside static prefix", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/{x}/b","method":"GET"}]}`, "", nil},
		{"mid-segment braces are literal", `{"endpoints":[{"endpoint":"/a/b{id}","method":"GET"}]}`, "", nil},
		{"suffix after a parameter", `{"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/{id}.json","method":"GET"}]}`, "conflicts with existing wildcard", []int{1, 0}},
		{"exact duplicate", `{"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/a","method":"GET"}]}`, "handlers are already registered", []int{1, 0}},
		{"double slash duplicate", `{"endpoints":[{"endpoint":"/a//b","method":"GET"},{"endpoint":"/a/b","method":"GET"}]}`, "handlers are already registered", []int{1, 0}},
		{"unnamed wildcard", `{"endpoints":[{"endpoint":"/a/*","method":"GET"}]}`, "wildcards must be named", []int{0}},
		{"custom health path", `{"extra_config":{"router":{"health_path":"/healthz"}},"endpoints":[{"endpoint":"/healthz","method":"GET"}]}`, "the gateway's own route", []int{0}},
		{"health path is GET only", `{"extra_config":{"router":{"health_path":"/healthz"}},"endpoints":[{"endpoint":"/healthz","method":"POST"}]}`, "", nil},
		{"health disabled", `{"extra_config":{"router":{"disable_health":true}},"endpoints":[{"endpoint":"/__health","method":"GET"}]}`, "", nil},
		{"auto options joins methods", `{"extra_config":{"router":{"auto_options":true}},"endpoints":[{"endpoint":"/a/{id}","method":"GET"},{"endpoint":"/a/{name}","method":"POST"}]}`, "conflicts with existing wildcard", []int{1, 0}},
		{"auto options registers one route per path", `{"extra_config":{"router":{"auto_options":true}},"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/a","method":"POST"}]}`, "", nil},
		{"auto options cleans the path first", `{"extra_config":{"router":{"auto_options":true}},"endpoints":[{"endpoint":"a","method":"GET"},{"endpoint":"/a","method":"POST"}]}`, "", nil},
		{"echo beside root parameter", `{"echo_endpoint":true,"endpoints":[{"endpoint":"/{x}","method":"GET"}]}`, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := routeConflicts(context.Background(), []byte(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			if tt.refused == "" {
				if len(lines) != 0 {
					t.Errorf("lines = %q, want none", lines)
				}
				return
			}
			if len(lines) != len(tt.blames) || !strings.Contains(lines[0], tt.refused) {
				t.Fatalf("lines = %q, want %d lines, the first containing %q", lines, len(tt.blames), tt.refused)
			}
			for i, idx := range tt.blames {
				if want := fmt.Sprintf("- at '/endpoints/%d/endpoint': ", idx); !strings.HasPrefix(lines[i], want) {
					t.Errorf("lines[%d] = %q, want prefix %q", i, lines[i], want)
				}
			}
		})
	}
}

func TestRouteConflicts_ARefusalOnItsOwnBlamesNoNeighbour(t *testing.T) {
	doc := `{"extra_config":{"router":{"disable_health":true}},"endpoints":[
		{"endpoint":"/ok","method":"GET"},
		{"endpoint":"/a/*","method":"GET"}]}`

	lines, err := routeConflicts(context.Background(), []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "- at '/endpoints/1/endpoint': ") {
		t.Errorf("lines = %q, want one line blaming endpoint 1, not the unrelated /ok", lines)
	}
}

func TestRouteConflicts_AGatewayRouteRefusalIsNotAnEndpointPointer(t *testing.T) {
	tests := []struct{ name, doc string }{
		{"health path refused alone", `{"extra_config":{"router":{"health_path":"/h/*"}},"endpoints":[]}`},
		{"debug route clashes with health path", `{"debug_endpoint":true,"extra_config":{"router":{"health_path":"/__debug/x"}},"endpoints":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, err := routeConflicts(context.Background(), []byte(tt.doc))
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) == 0 {
				t.Fatal("lines = none, want the gateway's route refused")
			}
			for _, l := range lines {
				if strings.Contains(l, "/endpoints/-1") || !strings.HasPrefix(l, "- gateway route ") {
					t.Errorf("line = %q, want a '- gateway route ...' line, not an endpoint pointer", l)
				}
			}
		})
	}
}

func TestRouteConflicts_NamesAnyMethodGatewayRoutes(t *testing.T) {
	doc := `{"echo_endpoint":true,"endpoints":[{"endpoint":"/__echo/x","method":"GET"}]}`

	lines, err := routeConflicts(context.Background(), []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "(any method /__echo/*param is the gateway's own route)") {
		t.Errorf("lines = %q, want the echo route named as an any-method route", lines)
	}
}

func TestRouteConflicts_ATypeMismatchIsLeftToTheSchemaLint(t *testing.T) {
	doc := `{"debug_endpoint":"yes","endpoints":[{"endpoint":"/a","method":"GET"}]}`

	lines, err := routeConflicts(context.Background(), []byte(doc))
	if err != nil || len(lines) != 0 {
		t.Errorf("routeConflicts = %q, %v, want no lines and no error (krakend check reports the type)", lines, err)
	}
}

func TestRouteConflicts_ACancelledContextIsAnErrorNotAVerdict(t *testing.T) {
	doc := `{"endpoints":[{"endpoint":"/a","method":"GET"},{"endpoint":"/a","method":"GET"}]}`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lines, err := routeConflicts(ctx, []byte(doc))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("routeConflicts = %q, %v, want the context error so the caller reports unavailable", lines, err)
	}
}

func TestRouteConflicts_StopsAfterTheFirstRefusedRoutes(t *testing.T) {
	const others, refused = 10000, 1024
	var b strings.Builder
	b.WriteString(`{"endpoints":[`)
	for i := 0; i < others; i++ {
		fmt.Fprintf(&b, `{"endpoint":"/r%d","method":"GET"},`, i)
	}
	for i := 0; i < refused; i++ {
		fmt.Fprintf(&b, `{"endpoint":"/r%d","method":"GET"},`, others-1)
	}
	doc := strings.TrimSuffix(b.String(), ",") + `]}`

	lines, err := routeConflicts(context.Background(), []byte(doc))

	if err != nil {
		t.Fatal(err)
	}
	// Each refusal names the refused route and the one it clashes with.
	if want := 2*MaxRouteRefusals + 1; len(lines) != want {
		t.Fatalf("%d lines, want %d: %d refusals and the stop notice", len(lines), want, MaxRouteRefusals)
	}
	wantNotice := fmt.Sprintf("- route check stopped after %d refused routes", MaxRouteRefusals)
	if last := lines[len(lines)-1]; last != wantNotice {
		t.Errorf("last line = %q, want %q", last, wantNotice)
	}
}

func TestClashRefusals_ACancelledContextIsAnError(t *testing.T) {
	accepted := []ginRoute{{index: 0, method: "GET", path: "/users/:id"}}
	refused := ginRoute{index: 1, method: "GET", path: "/users/:userId/orders"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := clashRefusals(ctx, accepted, refused, "refused")

	if !errors.Is(err, context.Canceled) {
		t.Errorf("clashRefusals = %v, %v, want the context error", got, err)
	}
}

func TestRouteRefusals_ReportsWhenItStoppedAtTheCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"endpoints":[{"endpoint":"/dup","method":"GET"}`)
	for i := 0; i < MaxRouteRefusals+3; i++ {
		b.WriteString(`,{"endpoint":"/dup","method":"GET"}`)
	}
	b.WriteString(`]}`)

	refusals, capped, err := routeRefusals(context.Background(), []byte(b.String()))

	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != MaxRouteRefusals || !capped {
		t.Errorf("%d refusals, capped %t, want %d and capped", len(refusals), capped, MaxRouteRefusals)
	}
}

func TestRouteRefusals_AGatewayRouteRefusalNamesNoEntry(t *testing.T) {
	doc := `{"debug_endpoint":true,"extra_config":{"router":{"health_path":"/__debug/x"}},"endpoints":[]}`

	refusals, capped, err := routeRefusals(context.Background(), []byte(doc))

	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 1 || len(refusals[0].Indices) != 0 || capped {
		t.Errorf("refusals = %+v, capped %t, want one that names no entry", refusals, capped)
	}
}
