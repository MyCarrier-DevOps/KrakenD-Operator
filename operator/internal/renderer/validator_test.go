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

func TestPrepareValidationCopy_NoStripping(t *testing.T) {
	input := []byte(`{"version":3,"endpoints":[{"endpoint":"/api"},{"endpoint":"/*"}]}`)
	out, _, err := validationCopy(input, v1alpha1.EditionCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// When eeWithoutFallback is false, no stripping occurs
	if string(out) != string(input) {
		t.Error("expected unchanged output when eeWithoutFallback is false")
	}
}

func TestPrepareValidationCopy_NoEndpoints(t *testing.T) {
	input := []byte(`{"version":3}`)
	out, _, err := validationCopy(input, v1alpha1.EditionEE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(input) {
		t.Error("expected unchanged output when no endpoints key")
	}
}

func TestPrepareValidationCopy_EmptyEndpointsArray(t *testing.T) {
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

func TestPrepareValidationCopy_InvalidJSON(t *testing.T) {
	_, _, err := validationCopy([]byte(`{invalid`), v1alpha1.EditionEE)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestPrepareValidationCopy_StripsEEExtraConfig(t *testing.T) {
	input := []byte(
		`{"version":3,"extra_config":{"backend/redis":{"host":"dragonfly:6379"},"telemetry/logging":{"level":"DEBUG"}}}`,
	)
	out, _, err := validationCopy(input, v1alpha1.EditionCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	ec, ok := config["extra_config"].(map[string]any)
	if !ok {
		t.Fatal("expected extra_config to exist")
	}
	if _, exists := ec["backend/redis"]; exists {
		t.Error("expected backend/redis to be stripped")
	}
	if _, exists := ec["telemetry/logging"]; !exists {
		t.Error("expected telemetry/logging to remain")
	}
}

func TestPrepareValidationCopy_StripsEEExtraConfigRemovesEmptyBlock(t *testing.T) {
	input := []byte(`{"version":3,"extra_config":{"backend/redis":{"host":"dragonfly:6379"}}}`)
	out, _, err := validationCopy(input, v1alpha1.EditionCE)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if _, exists := config["extra_config"]; exists {
		t.Error("expected extra_config block to be removed when empty")
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
		`{"endpoint":"/*","method":"GET"},{"endpoint":"/a/{id}/*","method":"GET"},{"endpoint":"/v1/*","method":"GET"}]}`)

	if err := v.Validate(context.Background(), rendered, v1alpha1.EditionEE); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(exec.checked) != 1 {
		t.Fatalf("krakend check ran %d times, want 1", len(exec.checked))
	}
	want := []string{"/*", "/a/{id}/{Wildcard}", "/v1/{Wildcard}"}
	if got := endpointPaths(t, exec.checked[0]); !slices.Equal(got, want) {
		t.Errorf("checked endpoints = %v, want %v (index-aligned; /* is left for krakend check to reject, as EE does)",
			got, want)
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
	if len(exec.checked) != 0 {
		t.Errorf("krakend check ran %d time(s); the verdict was already known", len(exec.checked))
	}
}
