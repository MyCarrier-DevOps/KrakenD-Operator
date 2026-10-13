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
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runChildEnv marks the process runOperator starts to run the operator in.
const runChildEnv = "OPERATOR_TEST_RUN_CHILD"

// logRecord is the part of a stdout log record the test reads.
type logRecord struct {
	Body         struct{ Value string }
	SeverityText string
}

// runOperator runs run() in a process of its own, the way main does, and
// returns its exit status and what it wrote. test names the calling test, which
// the child process runs and which then calls run(). The child has an empty
// kubeconfig and an environment of its own, with no OTEL_* variable except
// those in env, given as KEY=value.
func runOperator(t *testing.T, test string, env ...string) (exit int, stdout, stderr *bytes.Buffer) {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append([]string{runChildEnv + "=1", "KUBECONFIG=" + kubeconfig, "HOME=" + t.TempDir(),
		"GOCOVERDIR=" + os.Getenv("GOCOVERDIR")}, env...)
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, stdout, stderr
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), stdout, stderr
	}
	t.Fatalf("starting the operator: %v", err)
	return 0, nil, nil
}

// controller-runtime fulfils its root logger once per process, so only the
// first SetLogger counts and a test in this process cannot pin it. The test
// runs the operator in a process of its own, against an empty kubeconfig so
// that it stops after logging that it cannot load one, and reads what that
// process writes: a record of ctrl.Log reaches the stdout exporter only if
// nothing set the root logger before the telemetry pipeline did.
func TestRun_ControllerRuntimeLogsReachTheStdoutExporter(t *testing.T) {
	if os.Getenv(runChildEnv) == "1" {
		os.Exit(run())
	}
	exit, stdout, stderr := runOperator(t, "TestRun_ControllerRuntimeLogsReachTheStdoutExporter")
	if exit != 1 {
		t.Fatalf("exit status = %d, want 1 (no Kubernetes configuration)\nstdout:\n%s\nstderr:\n%s",
			exit, stdout, stderr)
	}
	bodies := map[string]string{}
	for _, line := range bytes.Split(stdout.Bytes(), []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var r logRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		bodies[r.Body.Value] = r.SeverityText
	}
	if got, ok := bodies["starting the operator"]; !ok || got != "INFO" {
		t.Errorf("no INFO record \"starting the operator\" on stdout\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if got, ok := bodies["unable to load the Kubernetes client configuration"]; !ok || got != "ERROR" {
		t.Errorf("no ERROR record for the kubeconfig on stdout\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing: every record goes through the pipeline", stderr)
	}
}

// Setup fails before any exporter is built when a setting cannot be used, and
// run() then exits with status 1 and says why on stderr, the only place left
// to say it. The endpoint keeps the test independent of any default: nothing
// dials it.
func TestRun_AnUnusableTelemetrySettingExitsWithStatus1(t *testing.T) {
	if os.Getenv(runChildEnv) == "1" {
		os.Exit(run())
	}

	exit, stdout, stderr := runOperator(t, "TestRun_AnUnusableTelemetrySettingExitsWithStatus1",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:1", "OTEL_TRACES_EXPORTER=jaeger")

	if exit != 1 {
		t.Errorf("exit status = %d, want 1\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	const want = `setting up telemetry: OTEL_TRACES_EXPORTER="jaeger" is not supported`
	if !strings.HasPrefix(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to start with %q", stderr, want)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: no log pipeline exists yet", stdout)
	}
}
