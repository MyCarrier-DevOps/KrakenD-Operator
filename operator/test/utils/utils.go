/*
Copyright 2026.

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
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck // dot-import required by Ginkgo DSL
)

const (
	prometheusOperatorVersion = "v0.77.1"
	prometheusOperatorURL     = "https://github.com/prometheus-operator/prometheus-operator/" +
		"releases/download/%s/bundle.yaml"

	certmanagerVersion = "v1.20.1"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	// dragonflyCRDURL is the Dragonfly operator CRD manifest used to satisfy the operator's
	// unstructured watches and resource creation when dragonfly.enabled is true.
	dragonflyCRDURL = "https://raw.githubusercontent.com/dragonflydb/dragonfly-operator/" +
		"main/config/crd/bases/dragonflydb.io_dragonflies.yaml"

	// istioCRDURL is the Istio networking CRD manifest (VirtualService, Gateway, etc.)
	// used to satisfy the operator's unstructured watches when istio.enabled is true.
	istioCRDURL = "https://raw.githubusercontent.com/istio/istio/master/" +
		"manifests/charts/base/files/crd-all.gen.yaml"

	// externalSecretsCRDURL is the External Secrets Operator CRD bundle used to satisfy
	// the operator's unstructured watches when license.externalSecret is enabled.
	externalSecretsCRDURL = "https://raw.githubusercontent.com/external-secrets/" +
		"external-secrets/main/deploy/crds/bundle.yaml"
)

// kubeconfigPath holds the path to the kubeconfig file for the ephemeral K3s cluster.
// Set by the suite setup; all kubectl commands use this automatically.
var kubeconfigPath string

// SetKubeconfig stores the kubeconfig path for the ephemeral cluster.
func SetKubeconfig(path string) { kubeconfigPath = path }

// GetKubeconfig returns the current kubeconfig path.
func GetKubeconfig() string { return kubeconfigPath }

func warnError(err error) {
	fmt.Fprintf(GinkgoWriter, "warning: %v\n", err) //nolint:errcheck // best-effort log
}

// Run executes the provided command within this context.
// If a kubeconfig has been set (ephemeral K3s cluster), it is injected via KUBECONFIG env var.
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir() //nolint:errcheck // best-effort directory resolution
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err) //nolint:errcheck // best-effort log
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	if kubeconfigPath != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfigPath)
	}
	// When DOCKER_HOST is set (e.g. for testcontainers), also propagate CONTAINER_HOST
	// so that podman CLI commands (used by Makefile as CONTAINER_TOOL) connect to the same daemon.
	if dh := os.Getenv("DOCKER_HOST"); dh != "" {
		cmd.Env = append(cmd.Env, "CONTAINER_HOST="+dh)
	}
	command := strings.Join(cmd.Args, " ")
	fmt.Fprintf(GinkgoWriter, "running: %q\n", command) //nolint:errcheck // best-effort log
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
func InstallPrometheusOperator() error {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "create", "-f", url)
	_, err := Run(cmd)
	return err
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsPrometheusCRDsInstalled checks if any Prometheus CRDs are installed
// by verifying the existence of key CRDs related to Prometheus.
func IsPrometheusCRDsInstalled() bool {
	// List of common Prometheus CRDs
	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range prometheusCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
	// Delete stale leader election leases so a fresh install can acquire leadership immediately.
	for _, lease := range []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	} {
		cmd = exec.Command("kubectl", "delete", "lease", lease, "-n", "kube-system", "--ignore-not-found")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "10m",
	)
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait for the cert-manager webhook to be fully operational by verifying
	// it can validate a dry-run Certificate resource. The webhook deployment
	// may report Available before its TLS CA bundle is fully propagated.
	cmd = exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
	cmd.Stdin = strings.NewReader(`
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: cert-manager-readiness-probe
  namespace: cert-manager
spec:
  secretName: readiness-probe-tls
  issuerRef:
    name: does-not-exist
    kind: Issuer
  dnsNames:
  - readiness-probe.example.com
`)
	// Retry until the webhook accepts the request (even if the issuer doesn't exist,
	// the webhook validation itself passes which proves the CA bundle is propagated).
	for range 30 {
		if _, err := Run(cmd); err == nil {
			return nil
		}
		// Re-create the command since Run() consumes it.
		cmd = exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
		cmd.Stdin = strings.NewReader(`
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: cert-manager-readiness-probe
  namespace: cert-manager
spec:
  secretName: readiness-probe-tls
  issuerRef:
    name: does-not-exist
    kind: Issuer
  dnsNames:
  - readiness-probe.example.com
`)
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("cert-manager webhook did not become ready after waiting")
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	content, err := os.ReadFile(filename) //nolint:gosec // filename is from test fixture, not user input
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	//nolint:gosec // filename is from test fixture, not user input
	if err = os.WriteFile(filename, out.Bytes(), 0o644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

// InstallDragonflyCRD applies the Dragonfly operator CRD to the cluster.
func InstallDragonflyCRD() error {
	cmd := exec.Command("kubectl", "apply", "-f", dragonflyCRDURL)
	_, err := Run(cmd)
	return err
}

// UninstallDragonflyCRD removes the Dragonfly operator CRD from the cluster.
func UninstallDragonflyCRD() {
	cmd := exec.Command("kubectl", "delete", "-f", dragonflyCRDURL, "--ignore-not-found")
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsDragonflyCRDInstalled checks if the Dragonfly CRD is present in the cluster.
func IsDragonflyCRDInstalled() bool {
	cmd := exec.Command("kubectl", "get", "crd", "dragonflies.dragonflydb.io")
	_, err := Run(cmd)
	return err == nil
}

// InstallIstioCRDs applies the Istio networking CRDs to the cluster.
func InstallIstioCRDs() error {
	cmd := exec.Command("kubectl", "apply", "-f", istioCRDURL)
	_, err := Run(cmd)
	return err
}

// UninstallIstioCRDs removes the Istio networking CRDs from the cluster.
func UninstallIstioCRDs() {
	cmd := exec.Command("kubectl", "delete", "-f", istioCRDURL, "--ignore-not-found")
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsIstioCRDInstalled checks if the Istio VirtualService CRD is present in the cluster.
func IsIstioCRDInstalled() bool {
	cmd := exec.Command("kubectl", "get", "crd", "virtualservices.networking.istio.io")
	_, err := Run(cmd)
	return err == nil
}

// InstallExternalSecretsCRDs applies the External Secrets Operator CRDs to the cluster.
func InstallExternalSecretsCRDs() error {
	// Use server-side apply because some CRDs exceed the 262144-byte annotation limit
	// imposed by client-side kubectl apply.
	cmd := exec.Command("kubectl", "apply", "--server-side", "-f", externalSecretsCRDURL)
	_, err := Run(cmd)
	return err
}

// UninstallExternalSecretsCRDs removes the External Secrets Operator CRDs from the cluster.
func UninstallExternalSecretsCRDs() {
	cmd := exec.Command("kubectl", "delete", "-f", externalSecretsCRDURL, "--ignore-not-found")
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsExternalSecretsCRDInstalled checks if the ExternalSecret CRD is present in the cluster.
func IsExternalSecretsCRDInstalled() bool {
	cmd := exec.Command("kubectl", "get", "crd", "externalsecrets.external-secrets.io")
	_, err := Run(cmd)
	return err == nil
}
