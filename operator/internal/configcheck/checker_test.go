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

package configcheck

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// fakeValidator records which mode ran, for which edition, and on which config,
// and answers with err. It is safe for concurrent use; read the recorded
// fields only after the checks have returned.
type fakeValidator struct {
	mu       sync.Mutex
	err      error
	calls    []string
	editions []v1alpha1.Edition
	seen     []string
}

func (f *fakeValidator) Validate(_ context.Context, jsonData []byte, edition v1alpha1.Edition) error {
	return f.record("validate", jsonData, edition)
}

func (f *fakeValidator) Lint(_ context.Context, jsonData []byte, edition v1alpha1.Edition) error {
	return f.record("lint", jsonData, edition)
}

func (f *fakeValidator) record(mode string, jsonData []byte, edition v1alpha1.Edition) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.editions, f.seen = append(f.calls, mode), append(f.editions, edition), append(f.seen, string(jsonData))
	return f.err
}

// okExecutor stands in for a krakend binary that accepts every config, so a
// real KrakenDValidator runs its edition copy, EE rules and route check.
type okExecutor struct{}

func (okExecutor) Execute(context.Context, string, ...string) ([]byte, error) {
	return []byte("Syntax OK!"), nil
}

func realValidator() renderer.Validator {
	return renderer.NewValidator(renderer.ValidatorOptions{Executor: okExecutor{}, BinaryPath: "krakend"})
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func newReader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointGateway, fieldindex.EndpointGatewayKeys).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointPolicy, fieldindex.EndpointPolicyKeys).
		Build()
}

func gateway(edition v1alpha1.Edition) *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: edition},
	}
}

func endpoint(name string, paths ...string) *v1alpha1.KrakenDEndpoint {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	}
	for _, p := range paths {
		ep.Spec.Endpoints = append(ep.Spec.Endpoints, v1alpha1.EndpointEntry{
			Endpoint: p, Method: "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}},
		})
	}
	return ep
}

// degraded marks gw as the gateway controller leaves it while the license is
// in fallback.
func degraded(gw *v1alpha1.KrakenDGateway) *v1alpha1.KrakenDGateway {
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionLicenseDegraded, Status: metav1.ConditionTrue, Reason: "Fallback",
	})
	return gw
}

func policy(name string) *v1alpha1.KrakenDBackendPolicy {
	return &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{Interval: 60, Timeout: 10, MaxErrors: 3},
		},
	}
}

// withPolicy makes ep's first backend reference the policy of that name.
func withPolicy(ep *v1alpha1.KrakenDEndpoint, name string) *v1alpha1.KrakenDEndpoint {
	ep.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: name}
	return ep
}

func newChecker(v renderer.Validator, objs ...client.Object) *Checker {
	return New(newReader(objs...), renderer.New(renderer.Options{}), v, 1, nil)
}

// The AutoConfig precheck passes an entry-less copy of a stale endpoint to
// model its deletion: it must no longer win a route.
func TestConflicts_AnEmptyReplacementRemovesTheEndpoint(t *testing.T) {
	older := endpoint("a", "/x")
	newer := endpoint("b", "/x")
	chk := newChecker(&fakeValidator{}, older, newer)
	gone := *older.DeepCopy()
	gone.Spec.Endpoints = nil

	conflicts, err := chk.Conflicts(context.Background(), gateway(v1alpha1.EditionCE), []v1alpha1.KrakenDEndpoint{gone})

	if err != nil || len(conflicts.Lost) != 0 {
		t.Errorf("Conflicts = %+v, %v; want none: with ns/a removed, ns/b serves /x alone", conflicts, err)
	}
}

func TestCheckGroup_ARouteClashAcrossEndpointsIsResolvedOldestFirst(t *testing.T) {
	c := newChecker(realValidator(), endpoint("a", "/users/{id}"))
	replace := []v1alpha1.KrakenDEndpoint{*endpoint("b", "/other", "/users/{userId}/orders")}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}

	verdict, err := c.CheckGroup(context.Background(), Group{
		Gateway: gateway(v1alpha1.EditionCE), Endpoints: []v1alpha1.KrakenDEndpoint{*endpoint("a", "/users/{id}"), replace[0]},
	}, nil)
	if err != nil || !verdict.OK || !slices.Equal(verdict.Masked, []types.NamespacedName{b}) {
		t.Fatalf("verdict = %+v, %v; want OK with ns/b masked: the render leaves the newer entry out", verdict, err)
	}
	conflicts, err := c.Conflicts(context.Background(), gateway(v1alpha1.EditionCE), replace)
	if err != nil {
		t.Fatal(err)
	}
	got := conflicts.Lost[b]
	if len(got) != 1 || got[0].Endpoint != "/users/{userId}/orders" || got[0].Winner.Name != "a" {
		t.Errorf("ns/b lost %+v, want GET /users/{userId}/orders to ns/a", got)
	}
}

func TestSameConfig(t *testing.T) {
	tests := []struct {
		name string
		edit func(gw *v1alpha1.KrakenDGateway)
		want bool
	}{
		{"image, version, replicas and postRestartJob do not reach the config", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Image = "registry/krakend:other"
			gw.Spec.Version = "2.12"
			gw.Spec.Replicas = ptr.To[int32](5)
			gw.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "true"}
		}, true},
		{"a root setting reaches the config", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Config.Timeout = "5s"
		}, false},
		{"the edition changes the validation even when the bytes match", func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Edition = v1alpha1.EditionEE
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newChecker(&fakeValidator{}, endpoint("a", "/a"))
			old := gateway(v1alpha1.EditionCE)
			gw := old.DeepCopy()
			tt.edit(gw)

			got, err := c.SameConfig(context.Background(), old, gw)

			if err != nil || got != tt.want {
				t.Errorf("SameConfig = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestCheckRendered_AVerdictCarriesTheRejectionsOutputAndStage(t *testing.T) {
	rejection := &renderer.ValidationError{
		Output: "- at '/endpoints/0/extra_config': bad", Err: errors.New("exit status 1"), Stage: renderer.StageCheck,
	}
	c := newChecker(&fakeValidator{err: rejection}, endpoint("a", "/a"))
	in, err := c.Gather(context.Background(), gateway(v1alpha1.EditionCE), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderer.New(renderer.Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}

	verdict, err := c.CheckRendered(context.Background(), in, out, nil)

	if err != nil || verdict.OK || verdict.Output != rejection.Output || verdict.Stage != renderer.StageCheck {
		t.Errorf("verdict = %+v, err = %v; want the rejection's output and stage", verdict, err)
	}
}

func TestCheck_TransientValidatorErrorIsAnError(t *testing.T) {
	c := newChecker(&fakeValidator{err: errors.New("running krakend check: fork/exec: no such file")}, endpoint("a", "/a"))
	if verdict, err := c.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, nil); err == nil {
		t.Fatalf("verdict = %+v, err = nil; want the validator failure as an error", verdict)
	}
}

// An admission request that cannot get a validation slot before its deadline
// fails fast instead of hanging past the API server's timeout.
func TestCheck_WaitsForASlotUntilTheDeadline(t *testing.T) {
	c := newChecker(&fakeValidator{}, endpoint("a", "/a"))
	c.slots <- struct{}{} // another check holds the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.CheckRoot(ctx, Root{Gateway: gateway(v1alpha1.EditionCE)}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("waited %s past the deadline", time.Since(start))
	}
}

func TestCheck_ReleasesTheSlotWhenDone(t *testing.T) {
	c := newChecker(&fakeValidator{}, endpoint("a", "/a"))
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		verdict, err := c.CheckRoot(ctx, Root{Gateway: gateway(v1alpha1.EditionCE)}, nil)
		cancel()
		if err != nil || !verdict.OK {
			t.Fatalf("check %d: verdict = %+v, err = %v; want OK", i, verdict, err)
		}
	}
}

func TestCheckGroup_RendersThePoliciesItsEndpointsReference(t *testing.T) {
	v := &fakeValidator{}
	c := newChecker(v, policy("breaker"))

	if _, err := c.CheckGroup(context.Background(), Group{
		Gateway: gateway(v1alpha1.EditionCE), Endpoints: []v1alpha1.KrakenDEndpoint{*withPolicy(endpoint("a", "/a"), "breaker")},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.seen[0], "qos/circuit-breaker") {
		t.Errorf("config lacks the policy's circuit breaker: %s", v.seen[0])
	}
}

// A missing policy is not a validator failure: the renderer leaves the
// endpoint out and the check goes on.
func TestCheckGroup_MissingPolicyLeavesTheEndpointOut(t *testing.T) {
	v := &fakeValidator{}
	c := newChecker(v)

	verdict, err := c.CheckGroup(context.Background(), Group{Gateway: gateway(v1alpha1.EditionCE),
		Endpoints: []v1alpha1.KrakenDEndpoint{*withPolicy(endpoint("a", "/a"), "gone"), *endpoint("b", "/b")}}, nil)

	if err != nil || !verdict.OK {
		t.Fatalf("verdict = %+v, err = %v; want OK", verdict, err)
	}
	if strings.Contains(v.seen[0], `"/a"`) || !strings.Contains(v.seen[0], `"/b"`) {
		t.Errorf("linted %s, want /b only", v.seen[0])
	}
}

func TestCheckRendered_RunsTheFullCheckAsTheRendersEdition(t *testing.T) {
	v := &fakeValidator{}
	c := newChecker(v, endpoint("a", "/a"))
	ee := gateway(v1alpha1.EditionEE)
	in, err := c.Gather(context.Background(), ee, nil)
	if err != nil {
		t.Fatal(err)
	}
	in.CEFallback = true // the controller's fresh license verdict
	out, err := renderer.New(renderer.Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if verdict, err := c.CheckRendered(context.Background(), in, out, nil); err != nil || !verdict.OK {
		t.Fatalf("verdict = %+v, err = %v", verdict, err)
	}
	if len(v.calls) != 1 || v.calls[0] != "validate" || v.editions[0] != v1alpha1.EditionCE {
		t.Errorf("calls = %v %v, want one CE validate for an EE gateway in fallback", v.calls, v.editions)
	}
}

func TestCheckGroup_HealthPathClash(t *testing.T) {
	gw := gateway(v1alpha1.EditionCE)
	gw.Spec.Config.Router = &v1alpha1.RouterConfig{HealthPath: "/healthz"}
	c := newChecker(realValidator())

	verdict, err := c.CheckGroup(context.Background(), Group{
		Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{*endpoint("a", "/healthz")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.OK || !strings.Contains(verdict.Output, "the gateway's own route") {
		t.Errorf("verdict = %+v, want the entry refused next to the health route", verdict)
	}
}

// panicValidator panics in either mode.
type panicValidator struct{}

func (panicValidator) Validate(context.Context, []byte, v1alpha1.Edition) error {
	panic("validator exploded")
}
func (panicValidator) Lint(context.Context, []byte, v1alpha1.Edition) error {
	panic("validator exploded")
}

// controller-runtime recovers a panicking webhook or reconciler, so a check
// that panics must still give its slot back.
func TestCheck_ReleasesTheSlotWhenTheValidatorPanics(t *testing.T) {
	c := newChecker(panicValidator{}, endpoint("a", "/a"))
	func() {
		defer func() { _ = recover() }()
		_, _ = c.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, nil)
	}()
	c.validator = &fakeValidator{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	verdict, err := c.CheckRoot(ctx, Root{Gateway: gateway(v1alpha1.EditionCE)}, nil)
	if err != nil || !verdict.OK {
		t.Fatalf("verdict = %+v, err = %v; want OK after a panicked check", verdict, err)
	}
}

func TestCheckRoot_AnEEGatewayInLicenseFallbackIsLintedAsCE(t *testing.T) {
	v := &fakeValidator{}
	c := newChecker(v)
	gw := degraded(gateway(v1alpha1.EditionEE))

	if _, err := c.CheckRoot(context.Background(), Root{Gateway: gw, CEFallback: CEFallback(gw)}, nil); err != nil {
		t.Fatal(err)
	}
	if len(v.editions) != 1 || v.editions[0] != v1alpha1.EditionCE {
		t.Errorf("editions = %v, want one CE lint", v.editions)
	}
}

// The gateway controller reports CE fallback for an EE gateway only, yet a
// stale LicenseDegraded condition outlives an EE to CE switch until its next
// reconcile. Admission must not strip a CE gateway's wildcard on that account.
func TestCheckGroup_AStaleFallbackConditionDoesNotAffectACEGateway(t *testing.T) {
	c := newChecker(realValidator())
	gw := degraded(gateway(v1alpha1.EditionCE))

	verdict, err := c.CheckGroup(context.Background(), Group{
		Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{*endpoint("a", "/files/*")}, CEFallback: CEFallback(gw),
	}, nil)

	if err != nil || verdict.OK {
		t.Errorf("verdict = %+v, err = %v; want the CE wildcard entry linted and rejected", verdict, err)
	}
}

// gateValidator blocks every lint on gate and tracks how many run at once.
type gateValidator struct {
	gate     chan struct{}
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (g *gateValidator) Validate(ctx context.Context, jsonData []byte, e v1alpha1.Edition) error {
	return g.Lint(ctx, jsonData, e)
}

func (g *gateValidator) Lint(context.Context, []byte, v1alpha1.Edition) error {
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for p := g.peak.Load(); n > p && !g.peak.CompareAndSwap(p, n); p = g.peak.Load() {
	}
	<-g.gate
	return nil
}

func TestCheck_RunsAtMostTheConfiguredNumberOfValidationsAtOnce(t *testing.T) {
	v := &gateValidator{gate: make(chan struct{})}
	c := New(newReader(endpoint("a", "/a")), renderer.New(renderer.Options{}), v, 2, nil)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for v.inFlight.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // time for a third validation to start, if the limit leaks
	close(v.gate)
	wg.Wait()

	if got := v.peak.Load(); got != 2 {
		t.Errorf("peak concurrent validations = %d, want 2", got)
	}
}

func TestNew_FewerThanOneSlotMeansOne(t *testing.T) {
	c := New(newReader(endpoint("a", "/a")), renderer.New(renderer.Options{}), &fakeValidator{}, 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	verdict, err := c.CheckRoot(ctx, Root{Gateway: gateway(v1alpha1.EditionCE)}, nil)

	if err != nil || !verdict.OK {
		t.Fatalf("verdict = %+v, err = %v; want OK", verdict, err)
	}
}

// cacheReader answers List the way the manager's cache does: with the shared
// stored object itself when UnsafeDisableDeepCopy is set, otherwise with a
// deep copy.
func cacheReader(shared *v1alpha1.KrakenDEndpoint) client.Reader {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var o client.ListOptions
			o.ApplyOptions(opts)
			item := shared.DeepCopy()
			if o.UnsafeDisableDeepCopy != nil && *o.UnsafeDisableDeepCopy {
				cp := *shared // shares the slices and maps, like a cached object
				item = &cp
			}
			list.(*v1alpha1.KrakenDEndpointList).Items = []v1alpha1.KrakenDEndpoint{*item}
			return nil
		},
	}).Build()
}

// The gateway controller writes to the endpoints it gathered, so what Gather
// returns must not be the cache's own objects.
func TestGather_ReturnsCopiesTheCallerMayMutate(t *testing.T) {
	shared := endpoint("a", "/a")
	c := New(cacheReader(shared), renderer.New(renderer.Options{}), &fakeValidator{}, 1, nil)

	in, err := c.Gather(context.Background(), gateway(v1alpha1.EditionCE), nil)
	if err != nil {
		t.Fatal(err)
	}
	in.Endpoints[0].Spec.Endpoints[0].Endpoint = "/mutated"

	if got := shared.Spec.Endpoints[0].Endpoint; got != "/a" {
		t.Errorf("the shared object now holds %q, want /a", got)
	}
}

func TestCheckPolicy_RendersThePolicyOnASyntheticBackend(t *testing.T) {
	v := &fakeValidator{}
	c := newChecker(v)
	p := policy("p")
	p.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"qos/circuit-breakr":{}}`)}

	if _, err := c.CheckPolicy(context.Background(), p, nil); err != nil {
		t.Fatal(err)
	}
	if len(v.seen) != 1 || !strings.Contains(v.seen[0], "qos/circuit-breakr") {
		t.Errorf("linted %v, want the policy's raw block", v.seen)
	}
	if v.calls[0] != "lint" || v.editions[0] != v1alpha1.EditionCE {
		t.Errorf("ran %s for %s, want lint for CE", v.calls[0], v.editions[0])
	}
}

func TestCheckGroup_TheOverrideKeepsTheOtherStoredPolicies(t *testing.T) {
	p, q := policy("p"), policy("q")
	p.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"stored-p/ns":{}}`)}
	q.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"stored-q/ns":{}}`)}
	v := &fakeValidator{}
	c := newChecker(v, p, q)
	candidate := p.DeepCopy()
	candidate.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"candidate/ns":{}}`)}

	if _, err := c.CheckGroup(context.Background(), Group{Gateway: gateway(v1alpha1.EditionCE),
		Endpoints: []v1alpha1.KrakenDEndpoint{*withPolicy(endpoint("a", "/a"), "p"), *withPolicy(endpoint("b", "/b"), "q")},
		Override:  candidate}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.seen[0], "stored-q/ns") || !strings.Contains(v.seen[0], "candidate/ns") {
		t.Errorf("linted %s, want the candidate p and the stored q", v.seen[0])
	}
}

func TestCheckGroup_TheOverrideIsKeyedByNamespace(t *testing.T) {
	stored := policy("p")
	stored.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"stored/ns":{}}`)}
	v := &fakeValidator{}
	c := newChecker(v, stored)
	candidate := stored.DeepCopy()
	candidate.Namespace = "other"
	candidate.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"candidate/ns":{}}`)}

	if _, err := c.CheckGroup(context.Background(), Group{Gateway: gateway(v1alpha1.EditionCE),
		Endpoints: []v1alpha1.KrakenDEndpoint{*withPolicy(endpoint("a", "/a"), "p")}, Override: candidate}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.seen[0], "stored/ns") || strings.Contains(v.seen[0], "candidate/ns") {
		t.Errorf("linted %s, want ns/p untouched by other/p", v.seen[0])
	}
}

func TestCheckGroup_TheOverrideFillsAMissingPolicy(t *testing.T) {
	v := &fakeValidator{}
	c := newChecker(v)
	candidate := policy("p")
	candidate.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"candidate/ns":{}}`)}

	if _, err := c.CheckGroup(context.Background(), Group{Gateway: gateway(v1alpha1.EditionCE),
		Endpoints: []v1alpha1.KrakenDEndpoint{*withPolicy(endpoint("a", "/a"), "p")}, Override: candidate}, nil); err != nil {
		t.Fatal(err)
	}
	if len(v.seen) != 1 || !strings.Contains(v.seen[0], "candidate/ns") {
		t.Errorf("linted %v, want the endpoint rendered with the created policy", v.seen)
	}
}

func TestCheckPolicy_ARejectionCarriesItsOutput(t *testing.T) {
	v := &fakeValidator{err: &renderer.ValidationError{
		Output: "ERROR linting the configuration file:\tjsonschema validation failed with 'file:///etc/krakend/schema.json#'\n" +
			"- at '/endpoints/0/backend/0/extra_config': additional properties 'qos/circuit-breakr' not allowed\n",
		Err: errors.New("exit status 1"),
	}}
	c := newChecker(v)

	verdict, err := c.CheckPolicy(context.Background(), policy("p"), nil)

	if err != nil || verdict.OK || verdict.Excerpt(1024) !=
		"- at '/endpoints/0/backend/0/extra_config': additional properties 'qos/circuit-breakr' not allowed" {
		t.Errorf("verdict = %+v, %v; want the rejection with its finding line", verdict, err)
	}
}
