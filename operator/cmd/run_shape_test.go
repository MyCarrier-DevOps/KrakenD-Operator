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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests read run()'s source. run() starts a manager against a cluster, so
// no test can drive it to the end; the order and the arguments of its process
// wide setup are what they pin.

// parseRun returns run()'s declaration.
func parseRun(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			return fset, fn
		}
	}
	t.Fatal("main.go has no run function")
	return nil, nil
}

// callee names the function a call expression calls, such as "ctrl.NewManager".
func callee(call *ast.CallExpr) string {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if recv, ok := sel.X.(*ast.Ident); ok {
			return recv.Name + "." + sel.Sel.Name
		}
	}
	return ""
}

// calls returns the calls to name inside n, in source order.
func calls(n ast.Node, name string) []*ast.CallExpr {
	var found []*ast.CallExpr
	ast.Inspect(n, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && callee(call) == name {
			found = append(found, call)
		}
		return true
	})
	return found
}

// expr renders an argument that is a plain identifier or selector.
func expr(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return expr(e.X) + "." + e.Sel.Name
	}
	return ""
}

// optionsValue returns the value given to key in run()'s ctrl.Options literal.
func optionsValue(t *testing.T, run *ast.FuncDecl, key string) ast.Expr {
	t.Helper()
	var value ast.Expr
	ast.Inspect(run, func(node ast.Node) bool {
		if kv, ok := node.(*ast.KeyValueExpr); ok && expr(kv.Key) == key {
			value = kv.Value
		}
		return true
	})
	if value == nil {
		t.Fatalf("run() gives ctrl.Options no %s", key)
	}
	return value
}

// grpc-go requires its logger before any gRPC call, and the OTLP gRPC
// exporters that Setup builds are gRPC clients.
func TestRun_RoutesGRPCLoggingFirst(t *testing.T) {
	_, run := parseRun(t)

	if len(run.Body.List) == 0 {
		t.Fatal("run() is empty")
	}
	first, ok := run.Body.List[0].(*ast.ExprStmt)
	if !ok {
		t.Fatalf("run()'s first statement is a %T, want the call that installs the grpc logger", run.Body.List[0])
	}
	if call, ok := first.X.(*ast.CallExpr); !ok || callee(call) != "telemetry.InstallGRPCLogging" {
		t.Errorf("run()'s first statement is not telemetry.InstallGRPCLogging")
	}
}

// TraceKubeAPI changes the rest.Config it is given, so a second call would nest
// the transports and record two client spans for each request. It runs once,
// before the manager copies the config into its clients, with the provider
// Setup returns: a no-op one when traces are off.
func TestRun_TracesTheKubernetesAPIOnceBeforeTheManager(t *testing.T) {
	_, run := parseRun(t)

	traced := calls(run, "telemetry.TraceKubeAPI")
	manager := calls(run, "ctrl.NewManager")
	if len(traced) != 1 || len(manager) != 1 {
		t.Fatalf("run() calls TraceKubeAPI %d times and NewManager %d times, want once each", len(traced), len(manager))
	}
	if traced[0].Pos() >= manager[0].Pos() {
		t.Error("TraceKubeAPI is not called before ctrl.NewManager")
	}
	if got, want := expr(traced[0].Args[0]), expr(manager[0].Args[0]); got == "" || got != want {
		t.Errorf("TraceKubeAPI wraps %q, but the manager is built from %q", got, want)
	}
	if got := expr(traced[0].Args[1]); got != "tel.TracerProvider" {
		t.Errorf("TraceKubeAPI is given %q, want tel.TracerProvider", got)
	}
}

// The builder registers each webhook on the server the manager holds when it is
// constructed; wrapping mgr.GetWebhookServer() afterwards would leave those
// registrations untraced.
func TestRun_BuildsTheManagerWithTheTracedWebhookServer(t *testing.T) {
	_, run := parseRun(t)

	servers := calls(run, "telemetry.TraceWebhookServer")
	if len(servers) != 1 {
		t.Fatalf("run() calls TraceWebhookServer %d times, want once", len(servers))
	}
	if got := expr(servers[0].Args[1]); got != "tel.TracerProvider" {
		t.Errorf("TraceWebhookServer is given %q, want tel.TracerProvider", got)
	}
	var assigned string
	ast.Inspect(run, func(node ast.Node) bool {
		if as, ok := node.(*ast.AssignStmt); ok && len(as.Rhs) == 1 && as.Rhs[0] == ast.Expr(servers[0]) {
			assigned = expr(as.Lhs[0])
		}
		return true
	})
	if got := expr(optionsValue(t, run, "WebhookServer")); got == "" || got != assigned {
		t.Errorf("ctrl.Options.WebhookServer = %q, want the traced server %q", got, assigned)
	}
	if len(calls(run, "mgr.GetWebhookServer")) != 0 {
		t.Error("run() reaches for mgr.GetWebhookServer, whose registrations would be untraced")
	}
}

// The reconcilers' reads are span events only through the client the manager
// builds with newManagerClient.
func TestRun_BuildsTheManagerClientWithReadEvents(t *testing.T) {
	_, run := parseRun(t)

	if got := expr(optionsValue(t, run, "NewClient")); got != "newManagerClient" {
		t.Errorf("ctrl.Options.NewClient = %q, want newManagerClient", got)
	}
}

// The context mgr.Start gets carries no span, so informer, lease and
// list-watch requests start no trace of their own.
func TestRun_StartsTheManagerWithNoAmbientSpan(t *testing.T) {
	_, run := parseRun(t)

	starts := calls(run, "mgr.Start")
	if len(starts) != 1 {
		t.Fatalf("run() calls mgr.Start %d times, want once", len(starts))
	}
	arg, ok := starts[0].Args[0].(*ast.CallExpr)
	if !ok || callee(arg) != "ctrl.SetupSignalHandler" {
		t.Error("mgr.Start is not given ctrl.SetupSignalHandler() directly: its context may carry a span")
	}
	if len(calls(run, "trace.ContextWithSpan"))+len(calls(run, "tracing.Start")) != 0 {
		t.Error("run() starts a span, which would parent every request of the manager's")
	}
}

// parseCommand parses the production files of cmd.
func parseCommand(t *testing.T) map[string]*ast.File {
	t.Helper()
	files := map[string]*ast.File{}
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = file
	}
	return files
}

// importers returns the production files under dirs that import path.
func importers(t *testing.T, path string, dirs ...string) []string {
	t.Helper()
	var found []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(name string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				if got, _ := strconv.Unquote(spec.Path.Value); got == path {
					found = append(found, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

// controller-runtime's root logger is set once per process: a zap logger set
// before InstallLogging would silently turn the routing through OpenTelemetry
// off.
func TestCommand_SetsNoLoggerOfItsOwn(t *testing.T) {
	if files := importers(t, "sigs.k8s.io/controller-runtime/pkg/log/zap", "../cmd", "../internal", "../api"); len(files) != 0 {
		t.Errorf("%v import controller-runtime's zap logger", files)
	}
	_, run := parseRun(t)
	if len(calls(run, "ctrl.SetLogger"))+len(calls(run, "logf.SetLogger")) != 0 {
		t.Error("run() sets the root logger: only telemetry.InstallLogging may")
	}
}

// controller-runtime's TokenReview and SubjectAccessReview filter runs on the
// scrape request's context: an otelhttp handler around the metrics server would
// trace every scrape.
func TestCommand_WrapsNoMetricsHandlerInOtelHTTP(t *testing.T) {
	for name, file := range parseCommand(t) {
		for _, spec := range file.Imports {
			if strings.Contains(spec.Path.Value, "otelhttp") {
				t.Errorf("%s imports %s: the metrics server must not be wrapped", name, spec.Path.Value)
			}
		}
	}
}
