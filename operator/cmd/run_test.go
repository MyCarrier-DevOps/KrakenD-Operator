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
	"testing"
)

// runChildEnv marks the process TestRun_ControllerRuntimeLogsReachTheStdoutExporter
// starts to run the operator in.
const runChildEnv = "OPERATOR_TEST_RUN_CHILD"

// logRecord is the part of a stdout log record the test reads.
type logRecord struct {
	Body         struct{ Value string }
	SeverityText string
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
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRun_ControllerRuntimeLogsReachTheStdoutExporter$")
	// An environment of its own: no OTEL_* variable and no real kubeconfig.
	cmd.Env = []string{runChildEnv + "=1", "KUBECONFIG=" + kubeconfig, "HOME=" + t.TempDir(),
		"GOCOVERDIR=" + os.Getenv("GOCOVERDIR")}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v, want exit status 1 (no Kubernetes configuration)\nstdout:\n%s\nstderr:\n%s",
			err, &stdout, &stderr)
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
		t.Errorf("no INFO record \"starting the operator\" on stdout\nstdout:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if got, ok := bodies["unable to load the Kubernetes client configuration"]; !ok || got != "ERROR" {
		t.Errorf("no ERROR record for the kubeconfig on stdout\nstdout:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing: every record goes through the pipeline", &stderr)
	}
}
