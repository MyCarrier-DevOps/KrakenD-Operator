//go:build integration

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

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// pinnedKrakenD extracts the krakend binary and its musl loader from the
// image the Dockerfile pins and returns a wrapper script that runs it here.
func pinnedKrakenD(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the extracted musl binary runs on Linux hosts only")
	}
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ARG KRAKEND_IMAGE=(\S+)$`).FindSubmatch(dockerfile)
	if m == nil {
		t.Fatal("Dockerfile has no ARG KRAKEND_IMAGE")
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{Image: string(m[1]), Entrypoint: []string{"sleep", "600"}},
		Started:          true,
	})
	if err != nil {
		t.Fatalf("starting %s: %v", m[1], err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	_, out, err := c.Exec(ctx, []string{"sh", "-c", "ls /lib/ld-musl-*.so.1"}, tcexec.Multiplexed())
	if err != nil {
		t.Fatal(err)
	}
	listing, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string]string{"/usr/bin/krakend": "krakend.bin", strings.TrimSpace(string(listing)): "loader"}
	for src, dst := range files {
		r, err := c.CopyFileFromContainer(ctx, src)
		if err != nil {
			t.Fatalf("copying %s: %v", src, err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := filepath.Join(dir, "krakend")
	script := fmt.Sprintf("#!/bin/sh\nexec %s %s \"$@\"\n", filepath.Join(dir, "loader"), filepath.Join(dir, "krakend.bin"))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return wrapper
}

type parityCase struct {
	name      string
	gateway   *v1alpha1.KrakenDGateway
	endpoints []*v1alpha1.KrakenDEndpoint
	// admitted is the expected admission verdict. A case the binary's router
	// test passes but admission rejects is a runtime-only clash -t never
	// registers (the health endpoint, auto_options).
	admitted bool
}

func parityGateway(edition v1alpha1.Edition, router *v1alpha1.RouterConfig) *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "parity"},
		Spec: v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: edition,
			Config: v1alpha1.GatewayConfig{Router: router}},
	}
}

func parityEndpoint(name string, age time.Duration, method string, paths ...string) *v1alpha1.KrakenDEndpoint {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "parity",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age))},
		Spec: v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	for _, p := range paths {
		ep.Spec.Endpoints = append(ep.Spec.Endpoints, v1alpha1.EndpointEntry{Endpoint: p, Method: method,
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}})
	}
	return ep
}

func parityCases() []parityCase {
	ce := parityGateway(v1alpha1.EditionCE, nil)
	return []parityCase{
		{"valid", ce, []*v1alpha1.KrakenDEndpoint{
			parityEndpoint("a", time.Hour, "GET", "/a/{id}"), parityEndpoint("b", 0, "GET", "/b")}, true},
		{"prefix parameter clash", ce, []*v1alpha1.KrakenDEndpoint{
			parityEndpoint("a", time.Hour, "GET", "/users/{id}"),
			parityEndpoint("b", 0, "GET", "/users/{userId}/orders")}, false},
		{"suffix after parameter", ce, []*v1alpha1.KrakenDEndpoint{
			parityEndpoint("a", time.Hour, "GET", "/files/{id}"),
			parityEndpoint("b", 0, "GET", "/files/{id}.json")}, false},
		{"same shape resolves oldest-wins", ce, []*v1alpha1.KrakenDEndpoint{
			parityEndpoint("a", time.Hour, "GET", "/a/{id}"), parityEndpoint("b", 0, "GET", "/a/{name}")}, true},
		{"double slash resolves oldest-wins", ce, []*v1alpha1.KrakenDEndpoint{
			parityEndpoint("a", time.Hour, "GET", "/a//b"), parityEndpoint("b", 0, "GET", "/a/b")}, true},
		{"unnamed wildcard on CE", ce, []*v1alpha1.KrakenDEndpoint{parityEndpoint("a", 0, "GET", "/files/*")}, false},
		{"unnamed wildcard on EE", parityGateway(v1alpha1.EditionEE, nil),
			[]*v1alpha1.KrakenDEndpoint{parityEndpoint("a", 0, "GET", "/files/*")}, true},
		{"reserved path", ce, []*v1alpha1.KrakenDEndpoint{parityEndpoint("a", 0, "GET", "/__health")}, false},
		{"custom health path clash", parityGateway(v1alpha1.EditionCE, &v1alpha1.RouterConfig{HealthPath: "/healthz"}),
			[]*v1alpha1.KrakenDEndpoint{parityEndpoint("a", 0, "GET", "/healthz")}, false},
		{"auto options clash", parityGateway(v1alpha1.EditionCE, &v1alpha1.RouterConfig{AutoOptions: true}),
			[]*v1alpha1.KrakenDEndpoint{
				parityEndpoint("a", time.Hour, "GET", "/a/{id}"), parityEndpoint("b", 0, "POST", "/a/{name}")}, false},
	}
}

// routerTestRejects runs the binary's `check -t -n` directly on a CE render.
// A CE validation copy equals the render.
func routerTestRejects(t *testing.T, bin string, rendered []byte) (bool, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "krakend.json")
	if err := os.WriteFile(file, rendered, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "check", "-t", "-n", "-c", file).CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("running krakend check: %v", err)
	}
	return err != nil, string(out)
}

// TestConfigCheckParity pins the admission shortcut: admission runs
// `krakend check -n` plus the in-process route check instead of `-t`, which
// is sound only while every config `-t -n` rejects is rejected by one of them.
func TestConfigCheckParity(t *testing.T) {
	bin := pinnedKrakenD(t)
	validator := renderer.NewValidator(renderer.ValidatorOptions{
		Executor: renderer.NewKrakenDExecutor(bin), BinaryPath: bin,
	})

	t.Run("binary version", func(t *testing.T) {
		out, err := exec.Command(bin, "version").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "KrakenD Version: "+configcheck.ValidatorVersion+".") {
			t.Fatalf("krakend version = %q (%v), want %s.x", out, err, configcheck.ValidatorVersion)
		}
	})

	policyChecker := configcheck.New(nil, renderer.New(renderer.Options{}), validator, 1)
	t.Run("LintPolicy valid policy alone", func(t *testing.T) {
		policy := &v1alpha1.KrakenDBackendPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "parity"},
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				CircuitBreaker: &v1alpha1.CircuitBreakerSpec{Interval: 60, Timeout: 10, MaxErrors: 3},
			},
		}
		verdict, err := policyChecker.LintPolicy(ctx, policy)
		if err != nil {
			t.Fatalf("LintPolicy: %v", err)
		}
		if !verdict.OK {
			t.Fatalf("a valid policy alone is refused by the real binary: %+v", verdict.Findings)
		}
	})

	t.Run("LintPolicy refuses a policy the binary rejects", func(t *testing.T) {
		policy := &v1alpha1.KrakenDBackendPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "parity"},
			Spec: v1alpha1.KrakenDBackendPolicySpec{
				Raw: &k8sruntime.RawExtension{Raw: []byte(`{"backend/grpc":{"unknown":1}}`)},
			},
		}
		verdict, err := policyChecker.LintPolicy(ctx, policy)
		if err != nil {
			t.Fatalf("LintPolicy: %v", err)
		}
		if verdict.OK || len(verdict.Findings) == 0 {
			t.Fatalf("verdict = %+v, want a refusal with findings", verdict)
		}
	})

	for _, tc := range parityCases() {
		t.Run(tc.name, func(t *testing.T) {
			scheme := k8sruntime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = v1alpha1.AddToScheme(scheme)
			objs := []client.Object{}
			for _, ep := range tc.endpoints {
				objs = append(objs, ep)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
				WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointGateway, fieldindex.EndpointGatewayKeys).
				WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointPolicy, fieldindex.EndpointPolicyKeys).
				Build()
			r := renderer.New(renderer.Options{})
			checker := configcheck.New(reader, r, validator, 1)

			admission, err := checker.CheckGateway(ctx, tc.gateway, nil)
			if err != nil {
				t.Fatalf("admission check: %v", err)
			}
			if admission.OK != tc.admitted {
				t.Errorf("admitted = %v, want %v (findings %+v)", admission.OK, tc.admitted, admission.Findings)
			}
			in, err := checker.Gather(ctx, tc.gateway, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Render(in)
			if err != nil {
				t.Fatal(err)
			}
			controller, err := checker.CheckRendered(ctx, in, out)
			if err != nil {
				t.Fatal(err)
			}
			if controller.OK != admission.OK {
				t.Errorf("controller verdict %v differs from admission verdict %v", controller.OK, admission.OK)
			}
			if tc.gateway.Spec.Edition == v1alpha1.EditionCE {
				if rejected, output := routerTestRejects(t, bin, out.JSON); rejected && admission.OK {
					t.Fatalf("krakend check -t -n rejects what admission admits:\n%s", output)
				}
			}
		})
	}
}
