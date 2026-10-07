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

package tracing_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// freshContexts are the production calls to context.Background or
// context.TODO allowed per file, with why. Anything else takes its caller's
// context, so the caller's span and deadline reach every call below it.
var freshContexts = map[string]int{
	// Field indexes are registered once at startup, before any request.
	"internal/fieldindex/fieldindex.go": 3,
	// The unlabelled counters are given their zero when they are created.
	"internal/telemetry/metrics.go": 1,
	// The test span recorder is shut down when its test ends.
	"internal/tracing/tracingtest/tracingtest.go": 1,
	// Telemetry is set up before the signal context exists, and flushed after
	// it is cancelled.
	"cmd/main.go": 2,
}

func TestNoRequestPathStartsAFreshContext(t *testing.T) {
	root := filepath.Join("..", "..")
	got := map[string]int{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			n, dot := countFreshContexts(file)
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if dot {
				t.Errorf("%s dot-imports context: write context.Background and context.TODO "+
					"qualified so they can be found", rel)
			}
			if n > 0 {
				got[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for file, n := range got {
		if n > freshContexts[file] {
			t.Errorf("%s calls context.Background() or context.TODO() %d times, %d allowed: "+
				"take the caller's context", file, n, freshContexts[file])
		}
	}
}

// countFreshContexts counts the context.Background and context.TODO calls in
// file, and reports whether it dot-imports context, which hides them.
func countFreshContexts(file *ast.File) (int, bool) {
	n := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" &&
			(sel.Sel.Name == "Background" || sel.Sel.Name == "TODO") {
			n++
		}
		return true
	})
	return n, false
}

func TestCountFreshContexts(t *testing.T) {
	tests := []struct {
		name, src string
		wantN     int
		wantDot   bool
	}{
		{"a plain call", `package p
import "context"
var _ = context.Background()`, 1, false},
		{"a renamed import", `package p
import ctx "context"
var _ = ctx.TODO()`, 1, false},
		{"a method value", `package p
import "context"
var bg = context.Background`, 1, false},
		{"a method value passed on", `package p
import "context"
func f(func() context.Context) {}
func g() { f(context.TODO) }`, 1, false},
		{"a dot import", `package p
import . "context"
var _ = Background()`, 0, true},
		{"another package's Background", `package p
import "context"
type c struct{ Background int }
var _ = context.WithCancel
var x c
var _ = x.Background`, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "src.go", tt.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			n, dot := countFreshContexts(file)
			if n != tt.wantN || dot != tt.wantDot {
				t.Errorf("got %d uses, dot import %v; want %d, %v", n, dot, tt.wantN, tt.wantDot)
			}
		})
	}
}
