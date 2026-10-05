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
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// fakeValidator records which mode ran, for which edition, and on which config,
// and answers with err.
type fakeValidator struct {
	err      error
	calls    []string
	editions []v1alpha1.Edition
	seen     []string
}

func (f *fakeValidator) Validate(_ context.Context, jsonData []byte, edition v1alpha1.Edition) error {
	return f.record("validate", jsonData, edition)
}

func (f *fakeValidator) Lint(_ context.Context, jsonData []byte, edition v1alpha1.Edition) error {
	return f.record("lint", jsonData, edition)
}

func (f *fakeValidator) record(mode string, jsonData []byte, edition v1alpha1.Edition) error {
	f.calls, f.editions, f.seen = append(f.calls, mode), append(f.editions, edition), append(f.seen, string(jsonData))
	return f.err
}

// okExecutor stands in for a krakend binary that accepts every config, so a
// real KrakenDValidator runs its edition copy, EE rules and route check.
type okExecutor struct{}

func (okExecutor) Execute(context.Context, string, ...string) ([]byte, error) {
	return []byte("Syntax OK!"), nil
}

func realValidator() renderer.Validator {
	return renderer.NewValidator(renderer.ValidatorOptions{Executor: okExecutor{}, BinaryPath: "krakend"})
}

func newReader(objs ...client.Object) client.Reader {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointGateway, fieldindex.EndpointGatewayKeys).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointPolicy, fieldindex.EndpointPolicyKeys).
		Build()
}

func gateway(edition v1alpha1.Edition) *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: edition},
	}
}

func endpoint(name string, paths ...string) *v1alpha1.KrakenDEndpoint {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	for _, p := range paths {
		ep.Spec.Endpoints = append(ep.Spec.Endpoints, v1alpha1.EndpointEntry{
			Endpoint: p, Method: "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}},
		})
	}
	return ep
}

func newChecker(v renderer.Validator, objs ...client.Object) *Checker {
	return New(newReader(objs...), renderer.New(renderer.Options{}), v, 1)
}

func TestCheckGateway_LintsCurrentEndpointsWithTheCandidate(t *testing.T) {
	v := &fakeValidator{}
	c := newChecker(v, endpoint("a", "/a"), endpoint("b", "/b"))

	verdict, err := c.CheckGateway(context.Background(), gateway(v1alpha1.EditionCE),
		[]v1alpha1.KrakenDEndpoint{*endpoint("b", "/b2"), *endpoint("c", "/c")})

	if err != nil || !verdict.OK {
		t.Fatalf("verdict = %+v, err = %v; want OK", verdict, err)
	}
	if len(v.calls) != 1 || v.calls[0] != "lint" || v.editions[0] != v1alpha1.EditionCE {
		t.Fatalf("calls = %v %v, want one CE lint", v.calls, v.editions)
	}
	for _, want := range []string{`"/a"`, `"/b2"`, `"/c"`} {
		if !strings.Contains(v.seen[0], want) {
			t.Errorf("config %s lacks %s", v.seen[0], want)
		}
	}
	if strings.Contains(v.seen[0], `"/b"`) {
		t.Errorf("config still holds the replaced entry: %s", v.seen[0])
	}
}

// The AutoConfig controller passes an entry-less copy of a stale endpoint to
// model its deletion. It must render nothing: not its paths, not its
// component schemas.
func TestCheckGateway_EmptyReplacementRemovesTheEndpoint(t *testing.T) {
	stale := endpoint("stale", "/stale")
	stale.Spec.ComponentSchemas = map[string]runtime.RawExtension{"staleschema": {Raw: []byte(`{"type":"object"}`)}}
	v := &fakeValidator{}
	c := newChecker(v, stale, endpoint("kept", "/kept"))
	removal := stale.DeepCopy()
	removal.Spec.Endpoints = nil

	verdict, err := c.CheckGateway(context.Background(), gateway(v1alpha1.EditionEE), []v1alpha1.KrakenDEndpoint{*removal})

	if err != nil || !verdict.OK {
		t.Fatalf("verdict = %+v, err = %v; want OK", verdict, err)
	}
	if strings.Contains(v.seen[0], "/stale") || strings.Contains(v.seen[0], "staleschema") || !strings.Contains(v.seen[0], "/kept") {
		t.Errorf("linted %s, want /kept only", v.seen[0])
	}
}

func TestCheckGateway_RouteClashAcrossEndpointsNamesBothEntries(t *testing.T) {
	c := newChecker(realValidator(), endpoint("a", "/users/{id}"))

	verdict, err := c.CheckGateway(context.Background(), gateway(v1alpha1.EditionCE),
		[]v1alpha1.KrakenDEndpoint{*endpoint("b", "/other", "/users/{userId}/orders")})
	if err != nil {
		t.Fatal(err)
	}
	got := map[types.NamespacedName]int{}
	for _, f := range verdict.Findings {
		got[f.Endpoint] = f.Index
	}
	a, b := types.NamespacedName{Namespace: "ns", Name: "a"}, types.NamespacedName{Namespace: "ns", Name: "b"}
	if verdict.OK || len(got) != 2 || got[a] != 0 || got[b] != 1 {
		t.Errorf("findings = %+v, want ns/a entry 0 and ns/b entry 1", verdict.Findings)
	}
}

func TestCheckGateway_AttributesLintOutputToTheSpecEntry(t *testing.T) {
	v := &fakeValidator{err: &renderer.ValidationError{
		Output: "ERROR linting the configuration file:\tjsonschema validation failed with 'file:///etc/krakend/schema.json#'\n" +
			"- at '/endpoints/1/extra_config': additional properties 'qos/circuit-breakr' not allowed\n",
		Err: errors.New("exit status 1"),
	}}
	c := newChecker(v, endpoint("a", "/a"), endpoint("b", "/y", "/x"))

	verdict, err := c.CheckGateway(context.Background(), gateway(v1alpha1.EditionCE), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Rendered order is /a, /x, /y: index 1 is b's /x, which is b's spec.endpoints[1].
	b := types.NamespacedName{Namespace: "ns", Name: "b"}
	if verdict.OK || len(verdict.Findings) != 1 || verdict.Findings[0].Endpoint != b || verdict.Findings[0].Index != 1 {
		t.Errorf("findings = %+v, want ns/b spec.endpoints[1]", verdict.Findings)
	}
}
