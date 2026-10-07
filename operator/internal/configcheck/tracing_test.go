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

package configcheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every exported method of *Checker is a span named configcheck.<Method>,
// whatever the methods are called: a check added later without its span
// fails here.
func TestChecker_EveryExportedMethodStartsItsOwnSpan(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	methods := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() || receiverType(fn) != "Checker" {
				continue
			}
			methods++
			if want := "configcheck." + fn.Name.Name; !startsSpan(fn.Body, want) {
				t.Errorf("(*Checker).%s starts no span %q", fn.Name.Name, want)
			}
		}
	}
	if methods == 0 {
		t.Fatal("found no exported method of *Checker")
	}
}

func receiverType(fn *ast.FuncDecl) string {
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, _ := expr.(*ast.Ident)
	if ident == nil {
		return ""
	}
	return ident.Name
}

// startsSpan reports whether body makes a call with name as a string
// argument: tracing.Start or the Checker's own c.start.
func startsSpan(body *ast.BlockStmt, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && s == name {
					found = true
				}
			}
		}
		return !found
	})
	return found
}
