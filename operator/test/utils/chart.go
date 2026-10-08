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

package utils

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	// chartPath is the operator chart, relative to the project directory.
	chartPath = "../charts/krakend-operator"
	// chartRelease is the Helm release name the rendered objects are named after.
	chartRelease = "krakend-operator"
	// chartKubeVersion is the Kubernetes version the chart is rendered for. The chart
	// requires 1.33, and the e2e cluster runs 1.32 (rootless podman needs it).
	chartKubeVersion = "1.33.0"
)

// OperatorManifest renders the operator chart for image into namespace and returns the
// YAML, with the CRDs when crds is true.
//
// The suite applies the render with kubectl instead of running helm install: the chart
// refuses Kubernetes older than 1.33, helm install reads the version from the cluster
// and has no flag to override it, and the e2e cluster is 1.32. The chart has no hooks,
// so applying the render is equivalent to an install.
func OperatorManifest(namespace, image string, crds bool) (string, error) {
	args, err := operatorTemplateArgs(namespace, image, crds)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("helm", args...)
	prepareCommand(cmd)
	// Output keeps stderr out of the YAML: a helm warning there would corrupt the manifest.
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%q failed: %w: %s", "helm "+strings.Join(args, " "), err, exitErr.Stderr)
		}
		return "", fmt.Errorf("%q failed: %w", "helm "+strings.Join(args, " "), err)
	}
	return string(out), nil
}

// operatorTemplateArgs is the helm template command line for OperatorManifest.
func operatorTemplateArgs(namespace, image string, crds bool) ([]string, error) {
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return nil, fmt.Errorf("image %q has no tag", image)
	}
	args := []string{
		"template", chartRelease, chartPath, "--namespace", namespace,
		"--kube-version", chartKubeVersion,
		"--set", "image.repository=" + image[:i], "--set", "image.tag=" + image[i+1:],
		"--set", "replicaCount=1",
	}
	if crds {
		args = append(args, "--include-crds")
	}
	return args, nil
}
