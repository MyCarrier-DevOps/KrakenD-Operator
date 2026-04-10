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

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	k3sContainer   *k3s.K3sContainer
	kubeconfigFile string
	k8sClient      client.Client
	ctx            context.Context
	cancel         context.CancelFunc
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	// Start an ephemeral K3s cluster via testcontainers.
	var err error
	k3sContainer, err = k3s.Run(ctx, "rancher/k3s:v1.31.6-k3s1",
		testcontainers.WithCmdArgs(
			"--disable=traefik",
			"--disable=metrics-server",
		),
	)
	if err != nil {
		panic("failed to start K3s container: " + err.Error())
	}

	// Extract kubeconfig from the K3s cluster.
	kubeConfigYaml, err := k3sContainer.GetKubeConfig(ctx)
	if err != nil {
		panic("failed to get kubeconfig: " + err.Error())
	}

	tmpFile, err := os.CreateTemp("", "integration-kubeconfig-*.yaml")
	if err != nil {
		panic("failed to create temp kubeconfig: " + err.Error())
	}
	kubeconfigFile = tmpFile.Name()
	if _, err := tmpFile.Write(kubeConfigYaml); err != nil {
		panic("failed to write kubeconfig: " + err.Error())
	}
	_ = tmpFile.Close()

	// Build rest.Config from the kubeconfig.
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeConfigYaml)
	if err != nil {
		panic("failed to build rest config: " + err.Error())
	}

	// Wait for K3s API server to be ready.
	waitForNodes(kubeconfigFile)

	// Install CRDs into the K3s cluster.
	crdDir, _ := filepath.Abs(filepath.Join("..", "..", "config", "crd", "bases"))
	installCRDs(kubeconfigFile, crdDir)

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic("failed to create client: " + err.Error())
	}

	// Start a controller manager in the background.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		panic("failed to create manager: " + err.Error())
	}

	// Wire up the Gateway controller with a real renderer and no-op validator.
	if err := (&controller.KrakenDGatewayReconciler{
		Client:    mgr.GetClient(),
		Scheme:    scheme,
		Recorder:  mgr.GetEventRecorderFor("krakendgateway-controller"),
		Renderer:  renderer.New(renderer.Options{}),
		Validator: &noopValidator{},
	}).SetupWithManager(mgr); err != nil {
		panic("failed to setup gateway controller: " + err.Error())
	}

	// Wire up the Endpoint controller.
	if err := (&controller.KrakenDEndpointReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Recorder: mgr.GetEventRecorderFor("krakendendpoint-controller"),
	}).SetupWithManager(mgr); err != nil {
		panic("failed to setup endpoint controller: " + err.Error())
	}

	// Wire up the BackendPolicy controller.
	if err := (&controller.KrakenDBackendPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Recorder: mgr.GetEventRecorderFor("krakendbackendpolicy-controller"),
	}).SetupWithManager(mgr); err != nil {
		panic("failed to setup policy controller: " + err.Error())
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic("failed to start manager: " + err.Error())
		}
	}()

	code := m.Run()

	cancel()
	if err := testcontainers.TerminateContainer(k3sContainer); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to terminate K3s container: %v\n", err)
	}
	_ = os.Remove(kubeconfigFile)
	os.Exit(code)
}

// noopValidator performs no validation (CE binary not available in integration tests).
type noopValidator struct{}

func (n *noopValidator) Validate(_ context.Context, _ []byte) error {
	return nil
}

func (n *noopValidator) PrepareValidationCopy(jsonData []byte, _ bool) ([]byte, error) {
	return jsonData, nil
}

// waitForNodes waits for all K3s nodes to become Ready.
func waitForNodes(kubeconfig string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "wait", "--for=condition=Ready", "node", "--all", "--timeout=5s")
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		if err := cmd.Run(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	panic("K3s nodes did not become ready within 60s")
}

// installCRDs applies all CRD YAML files from the given directory.
func installCRDs(kubeconfig, crdDir string) {
	entries, err := os.ReadDir(crdDir)
	if err != nil {
		panic("failed to read CRD directory: " + err.Error())
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		crdPath := filepath.Join(crdDir, entry.Name())
		cmd := exec.Command("kubectl", "apply", "-f", crdPath)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		out, err := cmd.CombinedOutput()
		if err != nil {
			panic(fmt.Sprintf("failed to install CRD %s: %s: %v", entry.Name(), string(out), err))
		}
	}
	// Wait for CRDs to be established.
	cmd := exec.Command("kubectl", "wait", "--for=condition=Established", "crd", "--all", "--timeout=30s")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	if out, err := cmd.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("CRDs not established: %s: %v", string(out), err))
	}
}
