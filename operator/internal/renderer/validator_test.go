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
	"testing"
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
	err := v.Validate(context.Background(), []byte(`{"version":3}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_Failure(t *testing.T) {
	v := NewValidator(ValidatorOptions{
		Executor:   &mockExecutor{output: []byte("ERROR: invalid config"), err: exitError(t, 1)},
		BinaryPath: "krakend",
	})
	err := v.Validate(context.Background(), []byte(`{"version":3}`))
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
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(`{"version":3,"endpoints":[{"endpoint":"/api"},{"endpoint":"/*"}]}`)
	out, err := v.PrepareValidationCopy(input, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// When eeWithoutFallback is false, no stripping occurs
	if string(out) != string(input) {
		t.Error("expected unchanged output when eeWithoutFallback is false")
	}
}

func TestPrepareValidationCopy_StripsWildcard(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(`{"endpoints":[{"endpoint":"/api","method":"GET"},{"endpoint":"/*","method":"GET"}],"version":3}`)
	out, err := v.PrepareValidationCopy(input, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	endpoints := config["endpoints"].([]any)
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint after stripping wildcard, got %d", len(endpoints))
	}
	ep := endpoints[0].(map[string]any)
	if ep["endpoint"] != "/api" {
		t.Errorf("expected /api endpoint, got %v", ep["endpoint"])
	}
}

func TestPrepareValidationCopy_NoEndpoints(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(`{"version":3}`)
	out, err := v.PrepareValidationCopy(input, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(input) {
		t.Error("expected unchanged output when no endpoints key")
	}
}

func TestPrepareValidationCopy_AllWildcardEndpoints(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(`{"endpoints":[{"endpoint":"/*","method":"GET"}],"version":3}`)
	out, err := v.PrepareValidationCopy(input, true)
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
	if len(endpoints) != 0 {
		t.Fatalf("expected 0 endpoints after stripping all wildcards, got %d", len(endpoints))
	}
}

func TestPrepareValidationCopy_EmptyEndpointsArray(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(`{"endpoints":[],"version":3}`)
	out, err := v.PrepareValidationCopy(input, true)
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
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	_, err := v.PrepareValidationCopy([]byte(`{invalid`), true)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestPrepareValidationCopy_StripsEEExtraConfig(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(
		`{"version":3,"extra_config":{"backend/redis":{"host":"dragonfly:6379"},"telemetry/logging":{"level":"DEBUG"}}}`,
	)
	out, err := v.PrepareValidationCopy(input, false)
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
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(`{"version":3,"extra_config":{"backend/redis":{"host":"dragonfly:6379"}}}`)
	out, err := v.PrepareValidationCopy(input, false)
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

func TestPrepareValidationCopy_StripsEEExtraConfigAndWildcard(t *testing.T) {
	v := NewValidator(ValidatorOptions{Executor: &mockExecutor{}, BinaryPath: "krakend"})
	input := []byte(
		`{"version":3,"extra_config":{"backend/redis":{"host":"dragonfly:6379"}},"endpoints":[{"endpoint":"/api","method":"GET"},{"endpoint":"/*","method":"GET"}]}`,
	)
	out, err := v.PrepareValidationCopy(input, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if _, exists := config["extra_config"]; exists {
		t.Error("expected extra_config block to be removed")
	}
	endpoints := config["endpoints"].([]any)
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint after stripping wildcard, got %d", len(endpoints))
	}
	ep := endpoints[0].(map[string]any)
	if ep["endpoint"] != "/api" {
		t.Errorf("expected /api endpoint, got %v", ep["endpoint"])
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
	if err := v.Validate(context.Background(), []byte(`{"version":3}`)); err != nil {
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
	err := v.Validate(context.Background(), []byte(`{"version":3}`))
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
