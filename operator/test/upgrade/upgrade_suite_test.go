//go:build upgrade

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

// Package upgrade tests what Helm does with the operator's CRDs: the move from
// a chart that ships them in crds/ to one that renders them as templates, then
// upgrade, rollback and uninstall. It runs the real helm binary against an
// ephemeral K3s cluster. No operator pod runs, so the suite shows Helm's
// behaviour with this chart, not the operator's.
package upgrade

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"

	"github.com/mycarrier-devops/krakend-operator/test/utils"
)

const (
	// publishedChartRepo and publishedChartVersion name the last chart that ships
	// the CRDs in crds/, which is what users upgrade from.
	publishedChartRepo    = "https://mycarrier-devops.github.io/KrakenD-Operator"
	publishedChartVersion = "0.14.0"

	// chartKubeVersionFloor is the Kubernetes version the repository chart
	// requires, and clusterKubeVersionFloor the one it is lowered to here.
	chartKubeVersionFloor   = ">=1.33.0-0"
	clusterKubeVersionFloor = ">=1.32.0-0"
)

var (
	// k3sContainer is the ephemeral K3s cluster the suite owns.
	k3sContainer *k3s.K3sContainer

	// kubeconfigFile holds the kubeconfig of that cluster. Every kubectl and helm
	// command gets it explicitly, so nothing reads the caller's kubeconfig.
	kubeconfigFile string

	// workDir holds the chart fixtures and helm's own state for this run.
	workDir string

	// operatorDir is the Go module directory, where helm and kubectl run.
	operatorDir string

	// oldChart is the published chart that ships its CRDs in crds/, newChart the
	// repository chart that renders them as templates.
	oldChart, newChart string
)

// helmVersion is the helm binary on PATH. The specs choose the documented flags
// and the scenarios from it, so it is read when the spec tree is built.
var helmVersion = detectHelmVersion()

// helmRelease is a parsed `helm version --short`.
type helmRelease struct {
	major, minor int
	raw          string
}

// detectHelmVersion reads the version of the helm binary on PATH. It does not
// contact a cluster. An unreadable version leaves major at 0, which the suite
// setup reports.
func detectHelmVersion() helmRelease {
	out, err := exec.Command("helm", "version", "--short").Output()
	raw := strings.TrimSpace(string(out))
	if err != nil {
		return helmRelease{raw: raw}
	}
	m := regexp.MustCompile(`^v(\d+)\.(\d+)`).FindStringSubmatch(raw)
	if m == nil {
		return helmRelease{raw: raw}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return helmRelease{major: major, minor: minor, raw: raw}
}

// supported reports whether the helm binary has --take-ownership: Helm 3.18 or
// later, or Helm 4.
func (v helmRelease) supported() bool {
	return v.major >= 4 || (v.major == 3 && v.minor >= 18)
}

// TestUpgrade runs the Helm upgrade suite in an ephemeral K3s cluster created
// via testcontainers.
func TestUpgrade(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting krakend-operator Helm upgrade suite\n")
	RunSpecs(t, "Helm upgrade suite")
}

var _ = BeforeSuite(func() {
	ctx := context.Background()
	var err error

	Expect(helmVersion.supported()).To(BeTrue(),
		"the suite needs Helm 3.18 or later, or Helm 4; `helm version --short` gave %q", helmVersion.raw)

	operatorDir, err = filepath.Abs("../..")
	Expect(err).NotTo(HaveOccurred())
	workDir, err = os.MkdirTemp("", "krakend-upgrade-*")
	Expect(err).NotTo(HaveOccurred())

	By("starting an ephemeral K3s cluster")
	k3sContainer, err = k3s.Run(ctx, utils.K3sImage, testcontainers.WithCmdArgs(utils.K3sArgs()...))
	Expect(err).NotTo(HaveOccurred(), "Failed to start K3s container")

	By("writing the kubeconfig of the K3s cluster")
	kubeConfigYaml, err := k3sContainer.GetKubeConfig(ctx)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve kubeconfig from K3s")
	kubeconfigFile = filepath.Join(workDir, "kubeconfig")
	Expect(os.WriteFile(kubeconfigFile, kubeConfigYaml, 0o600)).To(Succeed())

	By("waiting for the K3s nodes")
	mustRun("waiting for the K3s nodes to be Ready", "kubectl", "wait", "--for=condition=Ready", "node", "--all",
		"--timeout=120s")

	By("fetching the published chart that ships its CRDs in crds/")
	oldChart = pullPublishedChart()

	By("preparing the repository chart")
	newChart = copyRepositoryChart("new")
})

var _ = AfterSuite(func() {
	By("terminating the K3s cluster")
	if k3sContainer != nil {
		if err := k3sContainer.Terminate(context.Background()); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to terminate K3s container: %v\n", err)
		}
	}

	By("removing the fixtures and the kubeconfig")
	if workDir != "" {
		os.RemoveAll(workDir) //nolint:errcheck // best-effort cleanup
	}
})

// run executes a command in the module directory against the suite's cluster and
// returns its combined output. KUBECONFIG is always the suite's own, and the
// caller's kubeconfig, kube context and helm state never reach the command.
func run(name string, args ...string) (string, error) {
	return runWithStdin("", name, args...)
}

// runWithStdin is run with stdin as the command's standard input.
func runWithStdin(stdin, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = operatorDir
	cmd.Env = isolatedEnv()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	out, err := cmd.CombinedOutput()
	_, _ = fmt.Fprintf(GinkgoWriter, "%s\n", out)
	if err != nil {
		return string(out), fmt.Errorf("%q failed: %w: %s", command, err, out)
	}
	return string(out), nil
}

// isolatedEnv is the process environment without anything that selects a
// cluster or helm's state, plus the suite's own kubeconfig and helm directories.
func isolatedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "KUBECONFIG" || key == "KUBERNETES_MASTER" || strings.HasPrefix(key, "HELM_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"KUBECONFIG="+kubeconfigFile,
		"HELM_CACHE_HOME="+filepath.Join(workDir, "helm", "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(workDir, "helm", "config"),
		"HELM_DATA_HOME="+filepath.Join(workDir, "helm", "data"),
	)
}

// mustRun runs a command and fails the spec, naming what the step was doing and
// what the command printed, when it does not succeed.
func mustRun(step, name string, args ...string) string {
	GinkgoHelper()
	out, err := run(name, args...)
	Expect(err).NotTo(HaveOccurred(), "%s", step)
	return out
}

// pullPublishedChart downloads the published chart, the artifact users run, and
// returns the directory it was unpacked to.
func pullPublishedChart() string {
	GinkgoHelper()
	dir := filepath.Join(workDir, "old")
	mustRun("pulling chart "+publishedChartVersion+" from "+publishedChartRepo, "helm", "pull", "krakend-operator",
		"--repo", publishedChartRepo, "--version", publishedChartVersion, "--untar", "--untardir", dir)
	return filepath.Join(dir, "krakend-operator")
}

// copyRepositoryChart copies the repository chart to a new directory named name
// and lowers its kubeVersion floor to the K3s version the suite runs.
//
// The chart requires Kubernetes 1.33 and K3s runs 1.32, because 1.33 removed the
// feature gate rootless podman needs. The version constraint is the only part
// of Helm's adoption, ownership, resource-policy and rollback handling that
// reads the cluster version, and none of it does.
func copyRepositoryChart(name string) string {
	GinkgoHelper()
	dst := filepath.Join(workDir, name)
	Expect(os.CopyFS(dst, os.DirFS(filepath.Join(operatorDir, "..", "charts", "krakend-operator")))).To(Succeed())
	chartYAML := filepath.Join(dst, "Chart.yaml")
	data, err := os.ReadFile(chartYAML)
	Expect(err).NotTo(HaveOccurred())
	Expect(string(data)).To(ContainSubstring(chartKubeVersionFloor),
		"the chart's kubeVersion is no longer %s: update the suite's shim", chartKubeVersionFloor)
	Expect(os.WriteFile(chartYAML,
		[]byte(strings.Replace(string(data), chartKubeVersionFloor, clusterKubeVersionFloor, 1)), 0o600)).To(Succeed())
	return dst
}
