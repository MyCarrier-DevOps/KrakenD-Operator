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

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"

	"github.com/mycarrier-devops/krakend-operator/test/utils"
)

var (
	// projectImage is the name of the image which will be built and loaded
	// with the code source changes to be tested.
	projectImage = "ghcr.io/mycarrier-devops/krakend-operator:e2e"

	// k3sContainer is the ephemeral K3s cluster container.
	k3sContainer *k3s.K3sContainer

	// kubeconfigFile is a temporary file holding the kubeconfig for the K3s cluster.
	kubeconfigFile string
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an
// ephemeral K3s cluster created via testcontainers, ensuring a clean environment for every run.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting krakend-operator e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	ctx := context.Background()

	By("starting an ephemeral K3s cluster")
	var err error
	k3sContainer, err = k3s.Run(ctx, "rancher/k3s:v1.31.6-k3s1",
		testcontainers.WithCmdArgs(
			"--disable=traefik",
			"--disable=metrics-server",
		),
	)
	Expect(err).NotTo(HaveOccurred(), "Failed to start K3s container")

	By("extracting kubeconfig from the K3s cluster")
	kubeConfigYaml, err := k3sContainer.GetKubeConfig(ctx)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve kubeconfig from K3s")

	tmpFile, err := os.CreateTemp("", "k3s-kubeconfig-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	kubeconfigFile = tmpFile.Name()
	_, err = tmpFile.Write(kubeConfigYaml)
	Expect(err).NotTo(HaveOccurred())
	Expect(tmpFile.Close()).To(Succeed())
	utils.SetKubeconfig(kubeconfigFile)
	_, _ = fmt.Fprintf(GinkgoWriter, "K3s kubeconfig written to %s\n", kubeconfigFile)

	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	By("loading the manager(Operator) image into the K3s cluster")
	err = k3sContainer.LoadImages(ctx, projectImage)
	Expect(err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into K3s")

	By("waiting for K3s API server to be ready")
	cmd = exec.Command("kubectl", "wait", "--for=condition=Ready", "node", "--all", "--timeout=60s")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "K3s nodes did not become ready")

	By("installing cert-manager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install cert-manager")

	By("installing Dragonfly CRD")
	Expect(utils.InstallDragonflyCRD()).To(Succeed(), "Failed to install Dragonfly CRD")

	By("installing Istio CRDs")
	Expect(utils.InstallIstioCRDs()).To(Succeed(), "Failed to install Istio CRDs")

	By("installing External Secrets CRDs")
	Expect(utils.InstallExternalSecretsCRDs()).To(Succeed(), "Failed to install External Secrets CRDs")
})

var _ = AfterSuite(func() {
	By("removing kubeconfig temp file")
	if kubeconfigFile != "" {
		os.Remove(kubeconfigFile) //nolint:errcheck // best-effort cleanup
	}

	By("terminating the K3s cluster")
	if k3sContainer != nil {
		ctx := context.Background()
		if err := k3sContainer.Terminate(ctx); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: failed to terminate K3s container: %v\n", err)
		}
	}

	// Clean up any docker images built during the test.
	cleanImg := exec.Command("docker", "rmi", "-f", projectImage)
	cleanImg.Dir, _ = filepath.Abs(".")
	_ = cleanImg.Run()
})
