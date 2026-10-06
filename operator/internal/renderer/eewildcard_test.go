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
	"runtime"
	"strings"
	"testing"
)

func TestEEWildcardFindings_ACancelledContextIsAnErrorNotAVerdict(t *testing.T) {
	endpoints := []any{
		map[string]any{"endpoint": "/a/*", "method": "GET"},
		map[string]any{"endpoint": "/a/b", "method": "GET"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	findings, err := eeWildcardFindings(ctx, endpoints)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("eeWildcardFindings = %q, %v, want the context error so the caller reports unavailable", findings, err)
	}
}

func TestEEWildcardFindings_ShapesAreComputedPerRouteNotPerPair(t *testing.T) {
	const others, wildcards = 1000, 100
	endpoints := make([]any, 0, others+wildcards)
	for i := 0; i < others; i++ {
		endpoints = append(endpoints, map[string]any{"endpoint": fmt.Sprintf("/r%d/{id}", i), "method": "GET"})
	}
	for i := 0; i < wildcards; i++ {
		endpoints = append(endpoints, map[string]any{"endpoint": fmt.Sprintf("/w%d/*", i), "method": "GET"})
	}
	calls := 0
	shapeOf = func(path string) string { calls++; return routeShape(path) }
	t.Cleanup(func() { shapeOf = routeShape })

	findings, err := eeWildcardFindings(context.Background(), endpoints)

	if err != nil || len(findings) != 0 {
		t.Fatalf("eeWildcardFindings = %q, %v, want none", findings, err)
	}
	if limit := 2 * (others + wildcards); calls > limit {
		t.Errorf("%d shape computations for %d wildcards over %d routes, want at most %d (per route, not per pair)",
			calls, wildcards, others+wildcards, limit)
	}
}

func TestEEWildcardFindings_StopsAfterTheFirstConflicts(t *testing.T) {
	const depth = 300
	endpoints := make([]any, 0, 2*depth)
	for i := 1; i <= depth; i++ {
		prefix := "/n/" + strings.Repeat("a/", i)
		endpoints = append(endpoints,
			map[string]any{"endpoint": prefix + "*", "method": "GET"},
			map[string]any{"endpoint": prefix + "x", "method": "GET"})
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	findings, err := eeWildcardFindings(context.Background(), endpoints)

	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	// Each conflict yields one line per endpoint, then the stop notice.
	if want := 2*MaxRouteRefusals + 1; len(findings) != want {
		t.Fatalf("%d findings, want %d: %d conflicts and the stop notice", len(findings), want, MaxRouteRefusals)
	}
	wantNotice := fmt.Sprintf("- EE wildcard check stopped after %d conflicts", MaxRouteRefusals)
	if last := findings[len(findings)-1]; last != wantNotice {
		t.Errorf("last finding = %q, want %q", last, wantNotice)
	}
	const budget = 8 << 20
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > budget {
		t.Errorf("%d bytes allocated for %d routes, want at most %d", allocated, len(endpoints), budget)
	}
}

// Routes conflict by the shape gin registers them under: a brace group lura
// does not parse as a parameter is literal text, never the parameter a
// literal colon spells.
func TestEEWildcardFindings_ComparesRoutesByTheirRegisteredShape(t *testing.T) {
	cases := []struct {
		name, wildcard, other string
		wantFindings          int
	}{
		{"an unparsed brace group is not a colon parameter", "/v1/jobs{x}/*", "/v1/jobs:x/y", 0},
		{"a dotted brace group is not a colon parameter", "/users/{user.id}/*", "/users/:user.id/x", 0},
		{"a parsed parameter still conflicts", "/v1/{id}/*", "/v1/{id}/x", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoints := []any{
				map[string]any{"endpoint": tc.wildcard, "method": "GET"},
				map[string]any{"endpoint": tc.other, "method": "GET"},
			}

			findings, err := eeWildcardFindings(context.Background(), endpoints)

			if err != nil || len(findings) != tc.wantFindings {
				t.Errorf("eeWildcardFindings = %q, %v, want %d findings", findings, err, tc.wantFindings)
			}
		})
	}
}
