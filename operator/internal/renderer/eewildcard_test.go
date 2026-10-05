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
