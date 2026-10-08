package utils

import (
	"slices"
	"testing"
)

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
