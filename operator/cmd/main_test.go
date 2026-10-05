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

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// The 3-slot limit on concurrent krakend executions is per pod: the gateway
// controller and every webhook must share the one Checker that holds it.
func TestMain_BuildsOneConfigCheckerWithTheSharedSlots(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls []*ast.CallExpr
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "New" {
					if pkgName, ok := sel.X.(*ast.Ident); ok && pkgName.Name == "configcheck" {
						calls = append(calls, call)
					}
				}
				return true
			})
		}
	}

	if len(calls) != 1 {
		t.Fatalf("cmd builds %d config checkers, want exactly 1 shared by the controller and the webhooks", len(calls))
	}
	last := calls[0].Args[len(calls[0].Args)-1]
	if slots, ok := last.(*ast.Ident); !ok || slots.Name != "configCheckSlots" {
		t.Errorf("the checker's slots argument is %T %v, want configCheckSlots", last, last)
	}
	if slots := configCheckSlots; slots != 3 {
		t.Errorf("configCheckSlots = %d, want 3", slots)
	}
}
