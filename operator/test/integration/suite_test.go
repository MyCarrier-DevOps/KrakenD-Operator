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
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	licenseutil "github.com/mycarrier-devops/krakend-operator/internal/util/license"
	"github.com/mycarrier-devops/krakend-operator/test/utils"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	k8sclient "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	k3sContainer *k3s.K3sContainer
	k8sClient    client.Client
	restConfig   *rest.Config
	ctx          context.Context
	cancel       context.CancelFunc
	// mgrClient is the manager's own client, which the controllers use.
	mgrClient client.Client
	// suiteCache records typed Secret and ConfigMap requests to the
	// manager's cache.
	suiteCache *typedCoreReads
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

	// Start an ephemeral K3s cluster via testcontainers, with the image and
	// args the e2e suite shares (see utils.K3sImage and utils.K3sArgs).
	var err error
	k3sContainer, err = k3s.Run(ctx, utils.K3sImage, testcontainers.WithCmdArgs(utils.K3sArgs()...))
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

	// Install CRDs into the K3s cluster using the apiextensions client: the
	// operator's own, then the optional third-party ones it watches when
	// present at startup.
	for _, dir := range [][]string{
		{"..", "..", "config", "crd", "bases"},
		{"testdata", "crds"},
	} {
		crdDir, err := filepath.Abs(filepath.Join(dir...))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to resolve CRD directory path %v: %v\n", dir, err)
			return 1
		}
		if err := installCRDs(ctx, cfg, crdDir); err != nil {
			fmt.Fprintf(os.Stderr, "failed to install CRDs from %s: %v\n", crdDir, err)
			return 1
		}
	}

	restConfig = cfg
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		return 1
	}

	// Start a controller manager in the background.
	// Run the manager as the operator's ServiceAccount, bound to the
	// generated manager ClusterRole, so every scenario below also proves
	// the RBAC is sufficient: a missing verb fails a test with Forbidden.
	// No webhook server runs in this suite: the admission webhooks' cached
	// lists and policy reads are exercised under the role only in e2e.
	mgrCfg, err := operatorRBACConfig(ctx, cfg, k8sClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up operator RBAC: %v\n", err)
		return 1
	}
	mgr, err := ctrl.NewManager(mgrCfg, ctrl.Options{
		Scheme: scheme,
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: controller.UncachedObjects()},
		},
		Cache: cache.Options{ByObject: controller.CacheByObject()},
		NewCache: func(config *rest.Config, opts cache.Options) (cache.Cache, error) {
			c, err := cache.New(config, opts)
			if err != nil {
				return nil, err
			}
			suiteCache = &typedCoreReads{Cache: c}
			return suiteCache, nil
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manager: %v\n", err)
		return 1
	}

	// Wire up the Gateway and AutoConfig controllers with a real renderer and
	// the marker validator (the krakend binary is not available here), behind
	// one config checker.
	krakendRenderer := renderer.New(renderer.Options{})
	checker := configcheck.New(mgr.GetClient(), krakendRenderer, suiteValidator, 1)
	if err := (&controller.KrakenDGatewayReconciler{
		Client:    mgr.GetClient(),
		Scheme:    scheme,
		Recorder:  mgr.GetEventRecorderFor("krakendgateway-controller"),
		Renderer:  krakendRenderer,
		Checker:   checker,
		APIReader: mgr.GetAPIReader(),
		Clock:     clock.RealClock{},

		LicenseParser: licenseutil.NewX509LicenseParser(),
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
		Client:    mgr.GetClient(),
		Scheme:    scheme,
		Recorder:  mgr.GetEventRecorderFor("krakendbackendpolicy-controller"),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup policy controller: %v\n", err)
		return 1
	}

	// Wire up the AutoConfig controller with the real fetcher (behind a gate
	// for hosts that must hang), embedded CUE
	// evaluator, filter and generator.
	slowFetcher = &gatedFetcher{Fetcher: autoconfig.NewFetcher(mgr.GetClient())}
	if err := (&controller.KrakenDAutoConfigReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  scheme,
		Recorder:                mgr.GetEventRecorderFor("krakendautoconfig-controller"),
		Fetcher:                 slowFetcher,
		CUEEvaluator:            autoconfig.NewCUEEvaluator(),
		Filter:                  autoconfig.NewFilter(),
		Generator:               autoconfig.NewGenerator(),
		Checker:                 checker,
		Clock:                   clock.RealClock{},
		MaxConcurrentReconciles: 4,
		FetchTimeout:            20 * time.Second,
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup autoconfig controller: %v\n", err)
		return 1
	}

	mgrClient = mgr.GetClient()

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
		metadataOnly("ConfigMap"),
		metadataOnly("Secret"),
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

// rejectMarker is an endpoint path the suite's validator rejects. Any config
// without it passes: the krakend binary is not available in integration
// tests.
const rejectMarker = "/integration-reject"

// rejectHugeMarker is a rejectMarker path for which the validator also
// prints an oversized error report.
const rejectHugeMarker = rejectMarker + "-huge"

// markerValidator rejects a config containing rejectMarker the way krakend
// check does, with a *renderer.ValidationError, and counts the rejections so
// tests can tell how often the controller re-validated.
type markerValidator struct {
	rejections atomic.Int64
}

// suiteValidator is the validator the suite's gateway controller uses.
var suiteValidator = &markerValidator{}

// slowFetcher is the AutoConfig reconciler's fetcher in the suite.
var slowFetcher *gatedFetcher

// gatedFetcher blocks every fetch of a host ending in ".slow.invalid" until
// the fetch's context ends or Release is called, counting the fetches it
// holds, and passes every other fetch to the real fetcher.
type gatedFetcher struct {
	autoconfig.Fetcher
	held atomic.Int32

	mu      sync.Mutex
	release chan struct{}
}

// Release ends the fetches the gate holds now, which fail with an error; later
// fetches are held again.
func (g *gatedFetcher) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		close(g.release)
		g.release = nil
	}
}

// gate returns the channel that ends the fetches held now.
func (g *gatedFetcher) gate() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release == nil {
		g.release = make(chan struct{})
	}
	return g.release
}

func (g *gatedFetcher) Fetch(ctx context.Context, source autoconfig.FetchSource) (*autoconfig.FetchResult, error) {
	if u, err := url.Parse(source.URL); err == nil && strings.HasSuffix(u.Hostname(), ".slow.invalid") {
		g.held.Add(1)
		defer g.held.Add(-1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-g.gate():
			return nil, errors.New("slow host released")
		}
	}
	return g.Fetcher.Fetch(ctx, source)
}

func (v *markerValidator) Validate(_ context.Context, jsonData []byte, _ v1alpha1.Edition) error {
	if !bytes.Contains(jsonData, []byte(rejectMarker)) {
		return nil
	}
	v.rejections.Add(1)
	output := "ERROR: rejected by the integration test validator"
	if bytes.Contains(jsonData, []byte(rejectHugeMarker)) {
		// Far past the CRDs' 32768-character condition message limit, like
		// one bad policy referenced by a thousand backends.
		output = strings.Repeat(
			"ERROR at '/endpoints/0/backend/0/extra_config': additional properties not allowed\n", 1500)
	}
	return &renderer.ValidationError{Output: output, Err: errors.New("exit status 1")}
}

func (v *markerValidator) Lint(ctx context.Context, jsonData []byte, edition v1alpha1.Edition) error {
	return v.Validate(ctx, jsonData, edition)
}

// The identity the suite's manager impersonates.
const (
	operatorNamespace      = "krakend-operator-system"
	operatorServiceAccount = "controller-manager"
)

// metadataOnly returns the metadata-only form of a core kind, as the
// controllers watch it.
func metadataOnly(kind string) client.Object {
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind))
	return m
}

// typedCoreReads wraps the manager's cache and records every request it
// receives for a typed Secret or ConfigMap: an informer, a Get or a List.
// The controllers watch those kinds as metadata only and read their content
// live, so the record stays empty unless something caches their data.
type typedCoreReads struct {
	cache.Cache
	mu   sync.Mutex
	seen []string
}

func (r *typedCoreReads) note(obj runtime.Object) {
	switch obj.(type) {
	case *corev1.Secret, *corev1.SecretList, *corev1.ConfigMap, *corev1.ConfigMapList:
		r.record(fmt.Sprintf("%T", obj))
	}
}

func (r *typedCoreReads) record(what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, what)
}

func (r *typedCoreReads) records() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

func (r *typedCoreReads) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	r.note(obj)
	return r.Cache.Get(ctx, key, obj, opts...)
}

func (r *typedCoreReads) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.note(list)
	return r.Cache.List(ctx, list, opts...)
}

func (r *typedCoreReads) GetInformerForKind(
	ctx context.Context, gvk schema.GroupVersionKind, opts ...cache.InformerGetOption,
) (cache.Informer, error) {
	if gvk.Group == "" && (gvk.Kind == "Secret" || gvk.Kind == "ConfigMap") {
		r.record("GetInformerForKind " + gvk.Kind)
	}
	return r.Cache.GetInformerForKind(ctx, gvk, opts...)
}

func (r *typedCoreReads) IndexField(
	ctx context.Context, obj client.Object, field string, extract client.IndexerFunc,
) error {
	r.note(obj)
	return r.Cache.IndexField(ctx, obj, field, extract)
}

func (r *typedCoreReads) GetInformer(
	ctx context.Context, obj client.Object, opts ...cache.InformerGetOption,
) (cache.Informer, error) {
	r.note(obj)
	return r.Cache.GetInformer(ctx, obj, opts...)
}

// operatorRBACConfig binds the generated manager ClusterRole
// (config/rbac/role.yaml) to the operator's ServiceAccount and returns cfg
// impersonating that ServiceAccount, so the manager runs with exactly the
// permissions a deployment grants and a missing verb fails a test. It returns
// once the API server's authorizer allows the ServiceAccount to list gateways.
func operatorRBACConfig(ctx context.Context, cfg *rest.Config, c client.Client) (*rest.Config, error) {
	rolePath, err := filepath.Abs(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		return nil, fmt.Errorf("resolving role path: %w", err)
	}
	data, err := os.ReadFile(rolePath)
	if err != nil {
		return nil, fmt.Errorf("reading generated role: %w", err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(&role); err != nil {
		return nil, fmt.Errorf("decoding generated role: %w", err)
	}
	role.ObjectMeta = metav1.ObjectMeta{Name: "krakend-operator-integration-manager"}
	if err := c.Create(ctx, &role); err != nil {
		return nil, fmt.Errorf("creating manager role: %w", err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: operatorServiceAccount, Namespace: operatorNamespace,
		}},
	}
	if err := c.Create(ctx, binding); err != nil {
		return nil, fmt.Errorf("creating manager role binding: %w", err)
	}
	user := "system:serviceaccount:" + operatorNamespace + ":" + operatorServiceAccount
	if err := waitForServiceAccountAccess(ctx, c, user); err != nil {
		return nil, err
	}
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: user}
	return impersonated, nil
}

// waitForServiceAccountAccess polls a SubjectAccessReview until the binding is
// visible to the authorizer, so the manager never starts against a stale view.
func waitForServiceAccountAccess(ctx context.Context, c client.Client, user string) error {
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			review := &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{
					User: user,
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Group: v1alpha1.GroupVersion.Group, Resource: "krakendgateways", Verb: "list",
					},
				},
			}
			if err := c.Create(ctx, review); err != nil {
				return false, err
			}
			return review.Status.Allowed, nil
		})
	if err != nil {
		return fmt.Errorf("manager role binding never took effect for %s: %w", user, err)
	}
	return nil
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
