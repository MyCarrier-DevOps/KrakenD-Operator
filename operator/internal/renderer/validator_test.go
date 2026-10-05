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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// mockExecutor implements CommandExecutor for testing.
type mockExecutor struct {
	output []byte
	err    error
}

func (m *mockExecutor) Execute(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return m.output, m.err
}

func TestNewValidator(t *testing.T) {
	exec := &mockExecutor{}
	v := NewValidator(ValidatorOptions{Executor: exec, BinaryPath: "/usr/bin/krakend"})
	if v.BinaryPath != "/usr/bin/krakend" {
		t.Errorf("expected /usr/bin/krakend, got %s", v.BinaryPath)
	}
	if v.Executor != exec {
		t.Error("expected same executor")
	}
}

func TestNewKrakenDExecutor(t *testing.T) {
	e := NewKrakenDExecutor("/usr/bin/krakend")
	if e.BinaryPath != "/usr/bin/krakend" {
		t.Errorf("expected /usr/bin/krakend, got %s", e.BinaryPath)
	}
}

func TestValidate_Success(t *testing.T) {
	v := NewValidator(ValidatorOptions{
		Executor:   &mockExecutor{output: []byte("Syntax OK!"), err: nil},
		BinaryPath: "krakend",
	})
	err := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_Failure(t *testing.T) {
	v := NewValidator(ValidatorOptions{
		Executor:   &mockExecutor{output: []byte("ERROR: invalid config"), err: exitError(t, 1)},
		BinaryPath: "krakend",
	})
	err := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatal("expected ValidationError type")
	}
	if valErr.Output != "ERROR: invalid config" {
		t.Errorf("unexpected output: %s", valErr.Output)
	}
}

func TestValidationError_Error(t *testing.T) {
	err := &ValidationError{
		Output: "bad config",
		Err:    fmt.Errorf("exit status 1"),
	}
	msg := err.Error()
	if msg == "" {
		t.Error("expected non-empty error message")
	}
}

func TestValidationError_Unwrap(t *testing.T) {
	inner := fmt.Errorf("inner error")
	err := &ValidationError{Err: inner}
	if !errors.Is(err, inner) {
		t.Error("expected Unwrap to return inner error")
	}
}

func TestValidationCopy_CEIsUnchanged(t *testing.T) {
	input := []byte(`{"version":3,"endpoints":[{"endpoint":"/api"},{"endpoint":"/*"}]}`)
	out, _, err := validationCopy(input, v1alpha1.EditionCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A CE render is checked as it is rendered
	if string(out) != string(input) {
		t.Error("expected a CE render to be unchanged")
	}
}

func TestValidationCopy_NoEndpoints(t *testing.T) {
	input := []byte(`{"version":3}`)
	out, _, err := validationCopy(input, v1alpha1.EditionEE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(input) {
		t.Error("expected unchanged output when no endpoints key")
	}
}

func TestValidationCopy_EmptyEndpointsArray(t *testing.T) {
	input := []byte(`{"endpoints":[],"version":3}`)
	out, _, err := validationCopy(input, v1alpha1.EditionEE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	endpoints := config["endpoints"].([]any)
	if endpoints == nil {
		t.Fatal("endpoints should be an empty array, not null")
	}
}

func TestValidationCopy_InvalidJSON(t *testing.T) {
	_, _, err := validationCopy([]byte(`{invalid`), v1alpha1.EditionEE)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidationCopy_ValidatesTheRedisNamespace(t *testing.T) {
	input := []byte(`{"version":3,"extra_config":{"redis":{"connection_pools":[{"name":"default","address":"r:6379"}]}}}`)
	for _, edition := range []v1alpha1.Edition{v1alpha1.EditionCE, v1alpha1.EditionEE} {
		out, findings, err := validationCopy(input, edition)
		if err != nil || len(findings) != 0 {
			t.Fatalf("%s: validationCopy = %v, %v", edition, findings, err)
		}
		if !strings.Contains(string(out), `"redis"`) {
			t.Errorf("%s: the copy dropped the redis namespace; krakend check must lint it", edition)
		}
	}
}

// recordingExecutor records the command it was asked to run and succeeds.
type recordingExecutor struct {
	name string
	args []string
}

func (r *recordingExecutor) Execute(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name, r.args = name, args
	return nil, nil
}

// exitError returns the *exec.ExitError a real process exiting with code
// produces.
func exitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sh exit %d: expected *exec.ExitError, got %v", code, err)
	}
	return err
}

// fakeKrakenD writes an executable shell script standing in for the krakend
// binary and returns its path.
func fakeKrakenD(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "krakend")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidate_LintsOffline(t *testing.T) {
	rec := &recordingExecutor{}
	v := NewValidator(ValidatorOptions{Executor: rec, BinaryPath: "/usr/local/bin/krakend"})
	if err := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.name != "/usr/local/bin/krakend" {
		t.Errorf("ran %q, want the configured binary", rec.name)
	}
	if len(rec.args) != 5 || !slices.Equal(rec.args[:4], []string{"check", "-t", "-n", "-c"}) {
		t.Fatalf("args = %q, want [check -t -n -c <file>]", rec.args)
	}
	if slices.Contains(rec.args, "-l") {
		t.Errorf("args %q include -l, which lints against the online schema", rec.args)
	}
}

func TestValidate_MissingBinaryIsTransient(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "no-such-krakend")
	v := NewValidator(ValidatorOptions{Executor: NewKrakenDExecutor(bin), BinaryPath: bin})
	err := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	var valErr *ValidationError
	if errors.As(err, &valErr) {
		t.Fatalf("a missing binary was reported as an invalid config: %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected the error to wrap fs.ErrNotExist, got %v", err)
	}
}

func TestValidate_KilledProcessIsTransient(t *testing.T) {
	bin := fakeKrakenD(t, "kill -9 $$")
	v := NewValidator(ValidatorOptions{Executor: NewKrakenDExecutor(bin), BinaryPath: bin})
	err := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	if err == nil {
		t.Fatal("expected an error for a killed process")
	}
	var valErr *ValidationError
	if errors.As(err, &valErr) {
		t.Fatalf("a process killed by a signal was reported as an invalid config: %v", err)
	}
}

func TestValidate_DeadlineIsTransient(t *testing.T) {
	bin := fakeKrakenD(t, "exec sleep 5")
	v := NewValidator(ValidatorOptions{
		Executor:   NewKrakenDExecutor(bin),
		BinaryPath: bin,
		Timeout:    200 * time.Millisecond,
	})
	start := time.Now()
	err := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Validate took %s; the timeout did not stop krakend check", elapsed)
	}
	var valErr *ValidationError
	if errors.As(err, &valErr) {
		t.Fatalf("a timed-out run was reported as an invalid config: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the error to wrap context.DeadlineExceeded, got %v", err)
	}
}

func TestValidate_RejectionIsAVerdict(t *testing.T) {
	bin := fakeKrakenD(t, `echo "ERROR: bad endpoint"; exit 1`)
	v := NewValidator(ValidatorOptions{Executor: NewKrakenDExecutor(bin), BinaryPath: bin})
	err := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected *ValidationError for a non-zero exit, got %v", err)
	}
	if valErr.Output != "ERROR: bad endpoint\n" {
		t.Errorf("output = %q", valErr.Output)
	}
}

func TestValidate_TempFileErrorIsStable(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})

	first := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	second := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)

	if first == nil || second == nil {
		t.Fatalf("expected temp-file errors, got %v and %v", first, second)
	}
	var verdict *ValidationError
	if errors.As(first, &verdict) {
		t.Errorf("a temp-file failure is not a verdict: %v", first)
	}
	if first.Error() != second.Error() {
		t.Errorf("error text changes between calls, so a status message built from it never settles:\n%q\n%q",
			first, second)
	}
	if !errors.Is(first, fs.ErrNotExist) {
		t.Errorf("errors.Is(fs.ErrNotExist) = false for %v", first)
	}
}

func TestValidate_VerdictOutputDoesNotCarryTheTempPath(t *testing.T) {
	bin := fakeKrakenD(t, `echo "ERROR parsing the configuration file:	'$5': bad"; exit 1`)
	v := NewValidator(ValidatorOptions{Executor: NewKrakenDExecutor(bin), BinaryPath: bin})

	first := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	second := v.Validate(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)

	var firstVerdict, secondVerdict *ValidationError
	if !errors.As(first, &firstVerdict) || !errors.As(second, &secondVerdict) {
		t.Fatalf("expected verdicts, got %v and %v", first, second)
	}
	if first.Error() != second.Error() {
		t.Errorf("verdict text changes between runs:\n%q\n%q", first, second)
	}
}

// capturingExecutor records every document krakend check was asked to
// validate, and reports it valid.
type capturingExecutor struct {
	checked [][]byte
}

func (e *capturingExecutor) Execute(_ context.Context, _ string, args ...string) ([]byte, error) {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			data, err := os.ReadFile(args[i+1])
			if err != nil {
				return nil, err
			}
			e.checked = append(e.checked, data)
		}
	}
	return []byte("Syntax OK!"), nil
}

// endpointPaths lists the "endpoint" of every entry of a rendered document, in order.
func endpointPaths(t *testing.T, doc []byte) []string {
	t.Helper()
	var cfg struct {
		Endpoints []struct {
			Endpoint string `json:"endpoint"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(doc, &cfg); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(cfg.Endpoints))
	for _, ep := range cfg.Endpoints {
		paths = append(paths, ep.Endpoint)
	}
	return paths
}

func TestValidate_EEWildcardIsCheckedAsAParameterRoute(t *testing.T) {
	exec := &capturingExecutor{}
	v := NewValidator(ValidatorOptions{Executor: exec, BinaryPath: "krakend"})
	rendered := []byte(`{"version":3,"endpoints":[` +
		`{"endpoint":"/a/{id}/*","method":"GET"},{"endpoint":"/v1/*","method":"GET"}]}`)

	if err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(exec.checked) != 1 {
		t.Fatalf("krakend check ran %d times, want 1", len(exec.checked))
	}
	want := []string{"/a/{id}/{Wildcard}", "/v1/{Wildcard}"}
	if got := endpointPaths(t, exec.checked[0]); !slices.Equal(got, want) {
		t.Errorf("checked endpoints = %v, want %v (index-aligned)", got, want)
	}
}

func TestValidate_EEWildcardConflictsWithSameMethodRouteUnderPrefix(t *testing.T) {
	exec := &capturingExecutor{}
	v := NewValidator(ValidatorOptions{Executor: exec, BinaryPath: "krakend"})
	rendered := []byte(`{"version":3,"endpoints":[` +
		`{"endpoint":"/p","method":"GET"},{"endpoint":"/p/*","method":"GET"},` +
		`{"endpoint":"/p/static","method":"GET"},{"endpoint":"/p/x","method":"POST"},` +
		`{"endpoint":"/pq","method":"GET"}]}`)

	err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate = %v; the EE router refuses GET /p/static next to GET /p/*", err)
	}
	for _, want := range []string{"- at '/endpoints/1/endpoint'", "- at '/endpoints/2/endpoint'"} {
		if !strings.Contains(verr.Output, want) {
			t.Errorf("output %q lacks %q", verr.Output, want)
		}
	}
	for _, accepted := range []string{"/endpoints/0/", "/endpoints/3/", "/endpoints/4/"} {
		if strings.Contains(verr.Output, accepted) {
			t.Errorf("output %q blames %s, which the EE router accepts", verr.Output, accepted)
		}
	}
	sources := []types.NamespacedName{
		{Namespace: "ns", Name: "p"}, {Namespace: "ns", Name: "wild"}, {Namespace: "ns", Name: "static"},
		{Namespace: "ns", Name: "post"}, {Namespace: "ns", Name: "pq"},
	}
	blamed := map[types.NamespacedName]bool{}
	for _, a := range Attribute(rendered, sources, verr.Output) {
		blamed[a.Endpoint] = true
	}
	if len(blamed) != 2 || !blamed[sources[1]] || !blamed[sources[2]] {
		t.Errorf("findings blame %v, want exactly %s and %s", blamed, sources[1], sources[2])
	}
	if len(exec.checked) != 0 {
		t.Errorf("krakend check ran %d time(s); the verdict was already known", len(exec.checked))
	}
}

// lintingExecutor reports a lint finding on the "/later" endpoint, at the
// index that endpoint has in the document it was asked to check.
type lintingExecutor struct {
	t *testing.T
}

func (e lintingExecutor) Execute(_ context.Context, _ string, args ...string) ([]byte, error) {
	data, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		return nil, err
	}
	at := slices.Index(endpointPaths(e.t, data), "/later")
	return []byte(fmt.Sprintf("- at '/endpoints/%d/extra_config': additional properties not allowed\n", at)),
		exitError(e.t, 1)
}

func TestValidate_FindingAfterWildcardEntriesBlamesItsOwnEndpoint(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: lintingExecutor{t: t}, BinaryPath: "krakend"})
	rendered := []byte(`{"version":3,"endpoints":[` +
		`{"endpoint":"/v1/*","method":"GET"},{"endpoint":"/later","method":"GET"}]}`)
	sources := []types.NamespacedName{{Namespace: "ns", Name: "prefix"}, {Namespace: "ns", Name: "later"}}

	err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate = %v, want a ValidationError", err)
	}
	got := Attribute(rendered, sources, verr.Output)
	if len(got) != 1 || got[0].Endpoint != sources[1] {
		t.Errorf("attribution = %+v, want the single finding blamed on %s, not a neighbour", got, sources[1])
	}
}

func TestValidate_EEWildcardParameterIsNotAnOutputParam(t *testing.T) {
	exec := &capturingExecutor{}
	v := NewValidator(ValidatorOptions{Executor: exec, BinaryPath: "krakend"})
	rendered := []byte(`{"version":3,"endpoints":[` +
		`{"endpoint":"/ok","method":"GET","backend":[{"url_pattern":"/ok"}]},` +
		`{"endpoint":"/v1/*","method":"GET","backend":[{"url_pattern":"/x/{Wildcard}"}]}]}`)
	sources := []types.NamespacedName{{Namespace: "ns", Name: "ok"}, {Namespace: "ns", Name: "wild"}}

	err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate = %v; EE has no input parameter named Wildcard, so /x/{Wildcard} is undefined there", err)
	}
	got := Attribute(rendered, sources, verr.Output)
	if len(got) != 1 || got[0].Endpoint != sources[1] {
		t.Errorf("attribution = %+v, want one finding blamed on %s", got, sources[1])
	}
	if len(exec.checked) != 0 {
		t.Errorf("krakend check ran %d time(s); the verdict was already known", len(exec.checked))
	}
}

func TestValidate_EEWildcardEndpointAllowsOneBackend(t *testing.T) {
	exec := &capturingExecutor{}
	v := NewValidator(ValidatorOptions{Executor: exec, BinaryPath: "krakend"})
	rendered := []byte(`{"version":3,"endpoints":[` +
		`{"endpoint":"/ok","method":"GET","backend":[{"url_pattern":"/a"},{"url_pattern":"/b"}]},` +
		`{"endpoint":"/v1/*","method":"GET","backend":[{"url_pattern":"/x"},{"url_pattern":"/y"}]}]}`)
	sources := []types.NamespacedName{{Namespace: "ns", Name: "ok"}, {Namespace: "ns", Name: "wild"}}

	err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate = %v; EE refuses a wildcard endpoint with more than one backend", err)
	}
	if !strings.Contains(verr.Output, "wildcard endpoint can only have 1 backend") {
		t.Errorf("output %q lacks EE's message", verr.Output)
	}
	got := Attribute(rendered, sources, verr.Output)
	if len(got) != 1 || got[0].Endpoint != sources[1] {
		t.Errorf("attribution = %+v, want one finding blamed on %s", got, sources[1])
	}
}

func TestValidate_EEWildcardMayUseAParameterTheEndpointDeclares(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: &capturingExecutor{}, BinaryPath: "krakend"})
	rendered := []byte(`{"version":3,"endpoints":[` +
		`{"endpoint":"/a/{Wildcard}/*","method":"GET","backend":[{"url_pattern":"/x/{Wildcard}"}]}]}`)

	if err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE); err != nil {
		t.Errorf("Validate = %v; the endpoint declares {Wildcard} itself, so EE resolves it", err)
	}
}

func TestLint_RunsOfflineLintWithoutTheRouterTest(t *testing.T) {
	rec := &recordingExecutor{}
	v := NewValidator(ValidatorOptions{Executor: rec, BinaryPath: "/usr/local/bin/krakend"})

	if err := v.Lint(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE); err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(rec.args) != 4 || !slices.Equal(rec.args[:3], []string{"check", "-n", "-c"}) {
		t.Fatalf("args = %q, want [check -n -c <file>]", rec.args)
	}
}

func TestLint_ExitStatusIsAVerdict(t *testing.T) {
	v := NewValidator(ValidatorOptions{
		Executor: &mockExecutor{output: []byte("ERROR linting"), err: exitError(t, 1)}, BinaryPath: "krakend",
	})
	err := v.Lint(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Output != "ERROR linting" {
		t.Fatalf("err = %v, want *ValidationError carrying the output", err)
	}
}

func TestLint_DeadlineIsTransient(t *testing.T) {
	bin := fakeKrakenD(t, "exec sleep 5")
	v := NewValidator(ValidatorOptions{Executor: NewKrakenDExecutor(bin), BinaryPath: bin, Timeout: 200 * time.Millisecond})
	err := v.Lint(context.Background(), []byte(`{"version":3}`), v1alpha1.EditionCE)
	var valErr *ValidationError
	if errors.As(err, &valErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a transient deadline error", err)
	}
}

func TestValidate_RouteCheckRunsOnTheEditionsCopy(t *testing.T) {
	doc := []byte(`{"version":3,"endpoints":[{"endpoint":"/files/*","method":"GET"},{"endpoint":"/other","method":"GET"}]}`)
	for _, mode := range []string{"validate", "lint"} {
		run := func(v *KrakenDValidator, edition v1alpha1.Edition) error {
			if mode == "lint" {
				return v.Lint(context.Background(), doc, edition)
			}
			return v.Validate(context.Background(), doc, edition)
		}
		t.Run(mode, func(t *testing.T) {
			ee := &recordingExecutor{}
			if err := run(NewValidator(ValidatorOptions{Executor: ee, BinaryPath: "krakend"}), v1alpha1.EditionEE); err != nil {
				t.Errorf("EE: %v, want valid (the copy routes /files/{Wildcard})", err)
			}
			if ee.args == nil {
				t.Error("EE: krakend check did not run")
			}
			ce := &recordingExecutor{}
			err := run(NewValidator(ValidatorOptions{Executor: ce, BinaryPath: "krakend"}), v1alpha1.EditionCE)
			var valErr *ValidationError
			if !errors.As(err, &valErr) || !strings.Contains(valErr.Output, "- at '/endpoints/0/endpoint': wildcards must be named") {
				t.Errorf("CE: err = %v, want the unnamed wildcard refused at endpoint 0", err)
			}
			if ce.args != nil {
				t.Errorf("CE: krakend check ran with %q after the route check refused the config", ce.args)
			}
		})
	}
}

func TestValidate_EERootWildcardIsRefusedByTheRouteCheck(t *testing.T) {
	exec := &capturingExecutor{}
	v := NewValidator(ValidatorOptions{Executor: exec, BinaryPath: "krakend"})
	rendered := []byte(`{"version":3,"endpoints":[{"endpoint":"/ok","method":"GET"},{"endpoint":"/*","method":"GET"}]}`)

	err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE)
	var verr *ValidationError
	if !errors.As(err, &verr) || !errors.Is(err, errRouteConflict) ||
		!strings.HasPrefix(verr.Output, "- at '/endpoints/1/endpoint': ") {
		t.Fatalf("Validate = %v, want a route-conflict verdict blaming endpoint 1", err)
	}
	if len(exec.checked) != 0 {
		t.Errorf("krakend check ran %d time(s) after the route check refused the config", len(exec.checked))
	}
}

func TestEditionFor(t *testing.T) {
	cases := []struct {
		name       string
		edition    v1alpha1.Edition
		ceFallback bool
		want       v1alpha1.Edition
	}{
		{"EE gateway", v1alpha1.EditionEE, false, v1alpha1.EditionEE},
		{"EE gateway in CE fallback", v1alpha1.EditionEE, true, v1alpha1.EditionCE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &v1alpha1.KrakenDGateway{Spec: v1alpha1.KrakenDGatewaySpec{Edition: tc.edition}}
			if got := EditionFor(gw, tc.ceFallback); got != tc.want {
				t.Errorf("EditionFor = %s, want %s", got, tc.want)
			}
		})
	}
}
