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
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
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
			if want := "configcheck." + fn.Name.Name; !startsSpan(fn, want) {
				t.Errorf("(*Checker).%s starts no span %q as its first statement, from its ctx", fn.Name.Name, want)
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

// startsSpan reports whether the first statement of fn rebinds its context
// parameter from a call that takes that same context first and names the
// span: ctx, span := c.start(ctx, name, ...) or the tracing.Start form. A span
// started later, or from another context, leaves the work before it, or the
// spans below it, outside the check.
func startsSpan(fn *ast.FuncDecl, name string) bool {
	if len(fn.Body.List) == 0 || len(fn.Type.Params.List) == 0 || len(fn.Type.Params.List[0].Names) == 0 {
		return false
	}
	ctx := fn.Type.Params.List[0].Names[0].Name
	assign, ok := fn.Body.List[0].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) == 0 || len(assign.Rhs) == 0 {
		return false
	}
	if lhs, ok := assign.Lhs[0].(*ast.Ident); !ok || lhs.Name != ctx {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	if first, ok := call.Args[0].(*ast.Ident); !ok || first.Name != ctx {
		return false
	}
	for _, arg := range call.Args {
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil && s == name {
				return true
			}
		}
	}
	return false
}

// tracedChecker returns a checker over objs, with a real validator whose
// krakend runs are spans, recording its spans to rec.
func tracedChecker(rec *tracingtest.Recorder, objs ...client.Object) *Checker {
	v := renderer.NewValidator(renderer.ValidatorOptions{
		Executor: telemetry.TraceExecutor(okExecutor{}, rec.Tracer()), BinaryPath: "krakend",
	})
	return New(newReader(objs...), renderer.New(renderer.Options{}), v, 1, rec.Tracer())
}

// A check is a span below the caller's, and each content check it runs is a
// span below it, with the wait for a validation slot and the krakend run
// below that.
func TestChecker_CheckRenderedSpansTheSlotWaitAndTheKrakendRun(t *testing.T) {
	rec := tracingtest.New(t)
	c := tracedChecker(rec, endpoint("a", "/a"))
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile KrakenDGateway")
	in, err := c.Gather(ctx, gateway(v1alpha1.EditionCE), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderer.New(renderer.Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.CheckRendered(ctx, in, out, nil); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := rec.Ended()
	spans.RequireChild(t, "reconcile KrakenDGateway", "configcheck.CheckRendered")
	spans.RequireChild(t, "configcheck.CheckRendered", "configcheck.validate")
	spans.RequireChild(t, "configcheck.validate", "configcheck.slot")
	spans.RequireChild(t, "configcheck.validate", "krakend check")
}

// A check renders in process before it lints: the render is a span of its
// own below the check, beside the content check it feeds.
func TestChecker_CheckRootSpansItsRender(t *testing.T) {
	rec := tracingtest.New(t)
	c := New(newReader(), renderer.New(renderer.Options{}), &fakeValidator{}, 1, rec.Tracer())

	if _, err := c.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, nil); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "configcheck.CheckRoot", "configcheck.render")
	spans.RequireChild(t, "configcheck.CheckRoot", "configcheck.lint")
}

// A check answered from the memo runs no krakend, so without a mark it would
// read like one a Go-side rule refused: each content check says whether the
// memo answered it.
func TestChecker_EachContentCheckSaysWhetherTheMemoAnsweredIt(t *testing.T) {
	rec := tracingtest.New(t)
	c := tracedChecker(rec)
	memo := mapMemo{}
	root := Root{Gateway: gateway(v1alpha1.EditionCE)}

	for range 2 {
		if _, err := c.CheckRoot(context.Background(), root, memo); err != nil {
			t.Fatal(err)
		}
	}

	spans := rec.Ended()
	ran := spans.Named("configcheck.lint").With(attribute.Bool("configcheck.memo_hit", false))
	hit := spans.Named("configcheck.lint").With(attribute.Bool("configcheck.memo_hit", true))
	if len(ran) != 1 || len(hit) != 1 {
		t.Fatalf("%d lint checks run and %d answered from the memo, want 1 and 1; spans: %s", len(ran), len(hit), spans)
	}
	if parent := spans.Parent(spans.One(t, "krakend check")); parent == nil ||
		parent.SpanContext().SpanID() != ran[0].SpanContext().SpanID() {
		t.Errorf("krakend check has parent %v, want the check that ran; spans: %s", parent, spans)
	}
}

// An endpoint's check judges each policy it references on its own first:
// those checks are spans below the endpoint's.
func TestChecker_CheckEndpointSpansItsPolicyChecksBelowIt(t *testing.T) {
	rec := tracingtest.New(t)
	c := New(newReader(policy("p")), renderer.New(renderer.Options{}), &fakeValidator{}, 1, rec.Tracer())
	unit := EndpointUnit{Gateway: gateway(v1alpha1.EditionCE), Endpoint: withPolicy(endpoint("a", "/a"), "p")}

	if _, err := c.CheckEndpoint(context.Background(), unit, nil); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "configcheck.CheckEndpoint", "configcheck.CheckPolicy")
	if len(spans.Named("configcheck.lint")) != 2 {
		t.Errorf("want one lint below the policy's check and one below the endpoint's; spans: %s", spans)
	}
}

// A caller can name why it runs a check, and the check's span carries it.
func TestChecker_ACheckCarriesThePurposeItsCallerNames(t *testing.T) {
	rec := tracingtest.New(t)
	c := tracedChecker(rec)
	ctx := WithPurpose(context.Background(), "combined")

	if _, err := c.CheckRoot(ctx, Root{Gateway: gateway(v1alpha1.EditionCE)}, nil); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	if len(spans.Named("configcheck.CheckRoot").With(attribute.String("configcheck.purpose", "combined"))) != 1 {
		t.Errorf("the check lacks configcheck.purpose=combined; spans: %s", spans)
	}
}

// Conflicts and SameConfig render in process too: those renders are spans
// below the check that asked for them.
func TestChecker_ConflictsAndSameConfigSpanTheirRenders(t *testing.T) {
	rec := tracingtest.New(t)
	c := New(newReader(endpoint("a", "/a")), renderer.New(renderer.Options{}), &fakeValidator{}, 1, rec.Tracer())

	if _, err := c.Conflicts(context.Background(), gateway(v1alpha1.EditionCE), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SameConfig(context.Background(), gateway(v1alpha1.EditionCE), gateway(v1alpha1.EditionCE)); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	below := map[string]int{}
	for _, render := range spans.Named("configcheck.render") {
		if parent := spans.Parent(render); parent != nil {
			below[parent.Name()]++
		}
	}
	if below["configcheck.Conflicts"] != 1 || below["configcheck.SameConfig"] != 2 {
		t.Errorf("renders below Conflicts and SameConfig = %v, want 1 and 2; spans: %s", below, spans)
	}
}

// A group's check is a lint below it.
func TestChecker_CheckGroupSpansItsLint(t *testing.T) {
	rec := tracingtest.New(t)
	c := New(newReader(), renderer.New(renderer.Options{}), &fakeValidator{}, 1, rec.Tracer())

	if _, err := c.CheckGroup(context.Background(), Group{Gateway: gateway(v1alpha1.EditionCE)}, nil); err != nil {
		t.Fatal(err)
	}

	rec.Ended().RequireChild(t, "configcheck.CheckGroup", "configcheck.lint")
}
