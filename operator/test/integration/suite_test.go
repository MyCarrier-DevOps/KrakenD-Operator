//go:build integration

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

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	k8sclient "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	os.Exit(runTests(m))
}

// runTests sets up the K3s cluster, controllers, runs the test suite, and
// returns the exit code. Using a separate function lets deferred cleanup
// (container termination) run even when setup fails partway through.
func runTests(m *testing.M) int {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	// Start an ephemeral K3s cluster via testcontainers.
	// K3s 1.32.x is used instead of 1.33.x because K8s 1.33 removed the
	// KubeletInUserNamespace feature gate (graduated to GA).
	// client-go v0.33.0 supports +/-1 minor version skew per the K8s policy.
	// Kubelet args work around rootless podman constraints:
	//   - KubeletInUserNamespace: graceful fallback when /dev/kmsg unavailable
	//   - cgroups-per-qos=false + enforce-node-allocatable="": skip cgroup
	//     hierarchy creation that fails in rootless cgroupv2 containers
	var err error
	k3sContainer, err = k3s.Run(ctx, "rancher/k3s:v1.32.13-k3s1",
		testcontainers.WithCmdArgs(
			"--disable=traefik",
			"--disable=metrics-server",
			"--kubelet-arg=feature-gates=KubeletInUserNamespace=true",
			"--kubelet-arg=cgroups-per-qos=false",
			"--kubelet-arg=enforce-node-allocatable=",
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start K3s container: %v\n", err)
		return 1
	}
	defer func() {
		cancel()
		terminateCtx, terminateCancel := context.WithTimeout(
			context.Background(), 30*time.Second,
		)
		if err := k3sContainer.Terminate(terminateCtx); err != nil {
			fmt.Fprintf(os.Stderr,
				"warning: failed to terminate K3s container: %v\n", err)
		}
		terminateCancel()
	}()

	// Extract kubeconfig from the K3s cluster.
	kubeConfigYaml, err := k3sContainer.GetKubeConfig(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get kubeconfig: %v\n", err)
		return 1
	}

	// Build rest.Config from the kubeconfig.
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeConfigYaml)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build rest config: %v\n", err)
		return 1
	}

	// Wait for K3s nodes to be ready using client-go.
	if err := waitForNodes(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "K3s nodes did not become ready: %v\n", err)
		return 1
	}

	// Install CRDs into the K3s cluster using the apiextensions client.
	crdDir, err := filepath.Abs(
		filepath.Join("..", "..", "config", "crd", "bases"),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve CRD directory path: %v\n", err)
		return 1
	}
	if err := installCRDs(ctx, cfg, crdDir); err != nil {
		fmt.Fprintf(os.Stderr, "failed to install CRDs: %v\n", err)
		return 1
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		return 1
	}

	// Start a controller manager in the background.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manager: %v\n", err)
		return 1
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
		return 1
	}

	// Wire up the Endpoint controller.
	if err := (&controller.KrakenDEndpointReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Recorder: mgr.GetEventRecorderFor("krakendendpoint-controller"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup endpoint controller: %v\n", err)
		return 1
	}

	// Wire up the BackendPolicy controller.
	if err := (&controller.KrakenDBackendPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   scheme,
		Recorder: mgr.GetEventRecorderFor("krakendbackendpolicy-controller"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup policy controller: %v\n", err)
		return 1
	}

	// Wire up the AutoConfig controller with the real fetcher, embedded CUE
	// evaluator, filter and generator.
	if err := (&controller.KrakenDAutoConfigReconciler{
		Client:       mgr.GetClient(),
		Scheme:       scheme,
		Recorder:     mgr.GetEventRecorderFor("krakendautoconfig-controller"),
		Fetcher:      autoconfig.NewFetcher(mgr.GetClient()),
		CUEEvaluator: autoconfig.NewCUEEvaluator(),
		Filter:       autoconfig.NewFilter(),
		Generator:    autoconfig.NewGenerator(),
		Clock:        clock.RealClock{},
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup autoconfig controller: %v\n", err)
		return 1
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited with error: %v\n", err)
			cancel()
		}
	}()

	// controller-runtime's WaitForCacheSync only reports on informers that
	// already exist at the time it is called. Controller sources (For/Owns/
	// Watches) lazily create their informers when each controller's Start()
	// runs in the background, which races the call below. Pre-create an
	// informer for every type any registered controller watches so the gate
	// that follows actually covers them, matching what its comment claims.
	watchedTypes := []client.Object{
		&v1alpha1.KrakenDGateway{},
		&v1alpha1.KrakenDEndpoint{},
		&v1alpha1.KrakenDBackendPolicy{},
		&v1alpha1.KrakenDAutoConfig{},
		&corev1.ConfigMap{},
		&corev1.Secret{},
		&appsv1.Deployment{},
		&corev1.Service{},
		&corev1.ServiceAccount{},
		&policyv1.PodDisruptionBudget{},
		&autoscalingv2.HorizontalPodAutoscaler{},
		&batchv1.Job{},
	}
	informerCtx, informerCancel := context.WithTimeout(ctx, 30*time.Second)
	for _, obj := range watchedTypes {
		if _, err := mgr.GetCache().GetInformer(informerCtx, obj); err != nil {
			informerCancel()
			fmt.Fprintf(os.Stderr, "failed to pre-create informer for %T: %v\n", obj, err)
			cancel()
			return 1
		}
	}
	informerCancel()

	// Ensure the manager's caches have synced before running any test, so a
	// test never races a manager that failed to start. Every type any
	// registered controller watches has its informer pre-created above, so
	// this genuinely waits for all of them, not just whichever controller
	// sources happened to start first. Bound the wait so a stuck informer
	// fails fast and cleanly (allowing the deferred K3s cleanup to run)
	// instead of hanging until the `go test -timeout` kill, which panics
	// without unwinding defers and would leak the K3s container.
	// Note: cache sync is only a startup precondition; the widened eventually()
	// deadline in the tests is what absorbs K3s cold-start watch-propagation
	// latency after sync.
	syncCtx, syncCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		fmt.Fprintln(os.Stderr, "failed to sync manager cache before running tests")
		cancel()
		return 1
	}

	// Warm up the reconcile path once before any real test runs. Caches
	// synced above only proves informers have their initial list; it does
	// not prove K3s will actually deliver a subsequent watch event, and
	// roughly 1 in 9 local runs a watch-delivery stall left the first test's
	// KrakenDGateway create event undelivered for 60s+. Absorbing that stall
	// here turns it into a clear setup failure instead of a flaky first test.
	warmUpStart := time.Now()
	if err := warmUpControllers(ctx, k8sClient); err != nil {
		fmt.Fprintf(os.Stderr, "warm-up failed after %s: %v\n", time.Since(warmUpStart), err)
		cancel()
		return 1
	}
	fmt.Fprintf(os.Stderr, "warm-up reconciled in %s\n", time.Since(warmUpStart))

	return m.Run()
}

// warmUpControllers creates a throwaway namespace and a minimal
// KrakenDGateway, then polls until the gateway controller has reconciled it
// (status.phase set), or fails with a clear message, naming the last Get
// error if the final Get failed, if that does not happen within 3 minutes. A
// failed Get (the gateway not yet cached, or a transient API error) does not
// end the poll. The namespace uses a distinct prefix from the "test-"
// namespaces the suite's tests create, and is deleted without waiting for
// the deletion to finish (best effort; the whole K3s cluster is torn down
// at suite exit regardless).
func warmUpControllers(ctx context.Context, c client.Client) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "warmup-"},
	}
	if err := c.Create(ctx, ns); err != nil {
		return fmt.Errorf("creating warm-up namespace: %w", err)
	}
	defer func() {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer deleteCancel()
		_ = c.Delete(deleteCtx, ns)
	}()

	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup-gw", Namespace: ns.Name},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
	if err := c.Create(ctx, gw); err != nil {
		return fmt.Errorf("creating warm-up gateway: %w", err)
	}

	// lastGetErr is the error of the most recent Get, if it failed, for the
	// timeout message.
	var lastGetErr error
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			var cur v1alpha1.KrakenDGateway
			lastGetErr = c.Get(ctx, client.ObjectKeyFromObject(gw), &cur)
			if lastGetErr != nil {
				// Not yet in the cache, or a transient API error: keep polling.
				return false, nil
			}
			return cur.Status.Phase != "", nil
		},
	)
	if err != nil {
		if lastGetErr != nil {
			err = fmt.Errorf("%w (last Get error: %v)", err, lastGetErr)
		}
		return fmt.Errorf(
			"controllers did not reconcile a warm-up KrakenDGateway within 3m: %w", err,
		)
	}
	return nil
}

// warmUpClient returns a fake client for warmUpControllers whose
// KrakenDGateway Gets return getErr(n) for the nth Get (1-based) when it is
// non-nil, and otherwise the stored gateway as reconciled (status.phase set).
func warmUpClient(getErr func(n int) error) client.Client {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	var gets int
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context,
				c client.WithWatch,
				key client.ObjectKey,
				obj client.Object,
				opts ...client.GetOption,
			) error {
				gw, ok := obj.(*v1alpha1.KrakenDGateway)
				if !ok {
					return c.Get(ctx, key, obj, opts...)
				}
				gets++
				if err := getErr(gets); err != nil {
					return err
				}
				if err := c.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				gw.Status.Phase = v1alpha1.PhasePending
				return nil
			},
		}).
		Build()
}

func TestWarmUpControllers_TimeoutNamesLastGetError(t *testing.T) {
	c := warmUpClient(func(int) error {
		return apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("krakendgateways").GroupResource(), "warmup-gw")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := warmUpControllers(ctx, c)
	if err == nil || !strings.Contains(err.Error(), `krakendgateways.gateway.krakend.io "warmup-gw" not found`) {
		t.Fatalf("expected the timeout error to name the last Get error, got: %v", err)
	}
}

func TestWarmUpControllers_KeepsPollingThroughTransientGetErrors(t *testing.T) {
	c := warmUpClient(func(n int) error {
		if n <= 2 {
			return apierrors.NewServiceUnavailable("etcdserver: leader changed")
		}
		return nil
	})

	if err := warmUpControllers(context.Background(), c); err != nil {
		t.Fatalf("expected warm-up to ride out transient Get errors, got: %v", err)
	}
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

	return wait.PollUntilContextTimeout(
		ctx,
		2*time.Second,
		60*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, fmt.Errorf("listing nodes: %w", err)
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
		},
	)
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
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(&crd); err != nil {
			return fmt.Errorf("decoding CRD %s: %w", entry.Name(), err)
		}

		_, err = extClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating CRD %s: %w", crd.Name, err)
		}
	}

	// Wait for all CRDs to be established.
	return wait.PollUntilContextTimeout(
		ctx,
		time.Second,
		30*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			crdList, err := extClient.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, fmt.Errorf("listing CRDs: %w", err)
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
		},
	)
}
