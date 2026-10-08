package utils

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stubHelm puts a helm executable that runs script first on PATH.
func stubHelm(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil { //nolint:gosec // the stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestOperatorManifest_ReturnsStdoutWithoutHelmWarnings(t *testing.T) {
	stubHelm(t, `echo "kind: Deployment"; echo "WARNING: x" >&2`)
	got, err := OperatorManifest("ns", "repo/op:e2e", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "kind: Deployment\n" {
		t.Errorf("manifest = %q, want only helm's stdout", got)
	}
}

func TestOperatorManifest_CarriesHelmStderrInTheError(t *testing.T) {
	stubHelm(t, `echo boom >&2; exit 1`)
	_, err := OperatorManifest("ns", "repo/op:e2e", false)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want one that carries helm's stderr", err)
	}
}

func TestOperatorTemplateArgs_RendersTheReleaseForTheImage(t *testing.T) {
	got, err := operatorTemplateArgs("krakend-operator-system", "ghcr.io/mycarrier-devops/krakend-operator:e2e", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"template", "krakend-operator", "../charts/krakend-operator", "--namespace", "krakend-operator-system",
		"--kube-version", "1.33.0", "--set", "image.repository=ghcr.io/mycarrier-devops/krakend-operator",
		"--set", "image.tag=e2e", "--set", "replicaCount=1", "--include-crds",
	}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestOperatorTemplateArgs_LeavesOutTheCRDsOnRequest(t *testing.T) {
	got, err := operatorTemplateArgs("ns", "repo/op:e2e", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slices.Contains(got, "--include-crds") {
		t.Errorf("args = %q, want no --include-crds", got)
	}
}

func TestOperatorTemplateArgs_SplitsTheTagAfterARegistryPort(t *testing.T) {
	got, err := operatorTemplateArgs("ns", "localhost:5000/op:e2e", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"image.repository=localhost:5000/op", "image.tag=e2e"} {
		if !slices.Contains(got, want) {
			t.Errorf("args = %q, want %q among them", got, want)
		}
	}
}

func TestOperatorTemplateArgs_RefusesAnImageWithoutATag(t *testing.T) {
	if got, err := operatorTemplateArgs("ns", "localhost:5000/op", true); err == nil {
		t.Errorf("args = %q, want an error for an image without a tag", got)
	}
}
