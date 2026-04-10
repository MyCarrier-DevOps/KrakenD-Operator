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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	k8sclient "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	k3sContainer *k3s.K3sContainer
	k8sClient    client.Client
	ctx          context.Context
	cancel       context.CancelFunc
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

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
		fmt.Fprintf(os.Stderr, "failed to start K3s container: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := testcontainers.TerminateContainer(k3sContainer); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to terminate K3s container: %v\n", err)
		}
	}()

	// Extract kubeconfig from the K3s cluster.
	kubeConfigYaml, err := k3sContainer.GetKubeConfig(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get kubeconfig: %v\n", err)
		os.Exit(1)
	}

	// Build rest.Config from the kubeconfig.
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeConfigYaml)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build rest config: %v\n", err)
		os.Exit(1)
	}

	// Wait for K3s nodes to be ready using client-go.
	if err := waitForNodes(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "K3s nodes did not become ready: %v\n", err)
		os.Exit(1)
	}

	// Install CRDs into the K3s cluster using the apiextensions client.
	crdDir, _ := filepath.Abs(filepath.Join("..", "..", "config", "crd", "bases"))
	if err := installCRDs(ctx, cfg, crdDir); err != nil {
		fmt.Fprintf(os.Stderr, "failed to install CRDs: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		os.Exit(1)
	}

	// Start a controller manager in the background.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manager: %v\n", err)
		os.Exit(1)
	}

	// Wire up the Gateway controller with a real renderer and no-op validator.
	if err := (&controller.KrakenDGatewayReconciler{
		Client:    mgr.GetClient(),
		Scheme:    scheme,
		Recorder:  mgr.GetEventRecorderFor("krakendgateway-controller"),
		Renderer:  renderer.New(renderer.Options{}),
		Validator: &noopValidator{},
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup gateway controller: %v\n", err)
		os.Exit(1)
	}

	// Wire up the Endpoint controller.
	if err := (&controller.KrakenDEndpointReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Recorder: mgr.GetEventRecorderFor("krakendendpoint-controller"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup endpoint controller: %v\n", err)
		os.Exit(1)
	}

	// Wire up the BackendPolicy controller.
	if err := (&controller.KrakenDBackendPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Recorder: mgr.GetEventRecorderFor("krakendbackendpolicy-controller"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup policy controller: %v\n", err)
		os.Exit(1)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited with error: %v\n", err)
		}
	}()

	os.Exit(m.Run())
}

// noopValidator performs no validation (CE binary not available in integration tests).
type noopValidator struct{}

func (n *noopValidator) Validate(_ context.Context, _ []byte) error {
	return nil
}

func (n *noopValidator) PrepareValidationCopy(jsonData []byte, _ bool) ([]byte, error) {
	return jsonData, nil
}

// waitForNodes polls the Kubernetes API until all nodes report Ready.
func waitForNodes(ctx context.Context, cfg *rest.Config) error {
	clientset, err := k8sclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	return wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, nil // retry on transient errors
		}
		if len(nodes.Items) == 0 {
			return false, nil
		}
		for _, node := range nodes.Items {
			ready := false
			for _, c := range node.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				return false, nil
			}
		}
		return true, nil
	})
}

// installCRDs reads CRD YAML files from the given directory and applies them
// using the apiextensions clientset.
func installCRDs(ctx context.Context, cfg *rest.Config, crdDir string) error {
	extClient, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating apiextensions client: %w", err)
	}

	entries, err := os.ReadDir(crdDir)
	if err != nil {
		return fmt.Errorf("reading CRD directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		crdPath := filepath.Join(crdDir, entry.Name())
		data, err := os.ReadFile(crdPath)
		if err != nil {
			return fmt.Errorf("reading CRD file %s: %w", entry.Name(), err)
		}

		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), len(data)).Decode(&crd); err != nil {
			return fmt.Errorf("decoding CRD %s: %w", entry.Name(), err)
		}

		_, err = extClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating CRD %s: %w", crd.Name, err)
		}
	}

	// Wait for all CRDs to be established.
	return wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		crdList, err := extClient.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		for _, crd := range crdList.Items {
			established := false
			for _, c := range crd.Status.Conditions {
				if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
					established = true
					break
				}
			}
			if !established {
				return false, nil
			}
		}
		return true, nil
	})
}
