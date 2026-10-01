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
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestStatusPatch_MergePatchReplacesConditionsUnlessLocked pins the API
// server behavior both endpoint status writers rely on. Even with
// status.conditions declared as a list map, a JSON merge patch replaces the
// whole list: a patch computed from a stale read drops a condition another
// writer added since, and the same patch with an optimistic lock is rejected.
func TestStatusPatch_MergePatchReplacesConditionsUnlessLocked(t *testing.T) {
	ns := testNamespace(t)
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-premise", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "no-such-gateway"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	key := client.ObjectKeyFromObject(ep)
	// Let the endpoint controller's first status write land so it cannot race
	// the patches below; nothing else re-triggers it for this endpoint.
	eventually(t, func() error {
		var cur v1alpha1.KrakenDEndpoint
		if err := k8sClient.Get(ctx, key, &cur); err != nil {
			return err
		}
		if cur.Status.ObservedGeneration != cur.Generation {
			return fmt.Errorf("endpoint status not written yet")
		}
		return nil
	})

	var stale v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &stale); err != nil {
		t.Fatal(err)
	}
	var fresh v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type: "ExampleWriterA", Status: metav1.ConditionTrue, Reason: "Example", Message: "written after the stale read",
	})
	if err := k8sClient.Status().Update(ctx, &fresh); err != nil {
		t.Fatalf("writer A: %v", err)
	}

	writerB := metav1.Condition{
		Type: "ExampleWriterB", Status: metav1.ConditionTrue, Reason: "Example", Message: "computed from the stale read",
	}
	locked := stale.DeepCopy()
	meta.SetStatusCondition(&locked.Status.Conditions, writerB)
	err := k8sClient.Status().Patch(ctx, locked,
		client.MergeFromWithOptions(&stale, client.MergeFromWithOptimisticLock{}))
	if !apierrors.IsConflict(err) {
		t.Fatalf("locked merge patch from a stale read: got %v, want a Conflict", err)
	}

	unlocked := stale.DeepCopy()
	meta.SetStatusCondition(&unlocked.Status.Conditions, writerB)
	if err := k8sClient.Status().Patch(ctx, unlocked, client.MergeFrom(&stale)); err != nil {
		t.Fatalf("unlocked merge patch: %v", err)
	}
	var after v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(after.Status.Conditions, "ExampleWriterA") != nil {
		t.Fatal("an unlocked merge patch kept ExampleWriterA: the list was merged by key, " +
			"so the premise behind the optimistic lock no longer holds")
	}
}

// createGateway creates a CE KrakenDGateway named name in ns.
func createGateway(t *testing.T, ns, name string) client.ObjectKey {
	t.Helper()
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.9", Edition: v1alpha1.EditionCE, Config: v1alpha1.GatewayConfig{}},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway %s: %v", name, err)
	}
	return client.ObjectKeyFromObject(gw)
}

// createEndpoint creates a KrakenDEndpoint named name in ns on gateway, with
// one GET entry per path.
func createEndpoint(t *testing.T, ns, name, gateway string, paths ...string) client.ObjectKey {
	t.Helper()
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: gateway}},
	}
	for _, p := range paths {
		ep.Spec.Endpoints = append(ep.Spec.Endpoints, v1alpha1.EndpointEntry{
			Endpoint: p,
			Method:   "GET",
			Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc:8080"}, URLPattern: p}},
		})
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatalf("create endpoint %s: %v", name, err)
	}
	return client.ObjectKeyFromObject(ep)
}

// getEndpoint reads the endpoint key from the API server.
func getEndpoint(key client.ObjectKey) (*v1alpha1.KrakenDEndpoint, error) {
	var ep v1alpha1.KrakenDEndpoint
	if err := k8sClient.Get(ctx, key, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// expectCondition returns an error unless ep carries condition typ with the
// given status and reason, observed at ep's current generation.
func expectCondition(ep *v1alpha1.KrakenDEndpoint, typ string, status metav1.ConditionStatus, reason string) error {
	c := meta.FindStatusCondition(ep.Status.Conditions, typ)
	switch {
	case c == nil:
		return fmt.Errorf("%s: no %s condition (conditions %+v)", ep.Name, typ, ep.Status.Conditions)
	case c.Status != status || c.Reason != reason:
		return fmt.Errorf("%s: %s = %s/%s, want %s/%s", ep.Name, typ, c.Status, c.Reason, status, reason)
	case c.ObservedGeneration != ep.Generation:
		return fmt.Errorf("%s: %s observed generation %d, endpoint is at %d",
			ep.Name, typ, c.ObservedGeneration, ep.Generation)
	}
	return nil
}

// touchGateway sets a test annotation on the gateway, which re-runs its
// reconcile without changing its spec.
func touchGateway(t *testing.T, key client.ObjectKey, value string) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var gw v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, key, &gw); err != nil {
			return err
		}
		if gw.Annotations == nil {
			gw.Annotations = map[string]string{}
		}
		gw.Annotations["test.krakend.io/touch"] = value
		return k8sClient.Update(ctx, &gw)
	})
	if err != nil {
		t.Fatalf("annotating gateway %s: %v", key, err)
	}
}

// eventCount sums the counts of the events with the given reason recorded on
// the object named name in ns.
func eventCount(ns, name, reason string) (int32, error) {
	var list corev1.EventList
	if err := k8sClient.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return 0, err
	}
	var total int32
	for i := range list.Items {
		ev := &list.Items[i]
		if ev.InvolvedObject.Name == name && ev.Reason == reason {
			total += max(ev.Count, 1)
		}
	}
	return total, nil
}

func TestGatewayAcceptance_MarksIncludedAndConflictedEndpoints(t *testing.T) {
	ns := testNamespace(t)
	gw := createGateway(t, ns, "gw-accept")
	// ep-a is created first and sorts first, so it wins GET /users whether or
	// not both creation timestamps fall in the same second.
	older := createEndpoint(t, ns, "ep-a-older", gw.Name, "/users")
	eventually(t, func() error {
		ep, err := getEndpoint(older)
		if err != nil {
			return err
		}
		return expectCondition(ep, "Accepted", metav1.ConditionTrue, "Accepted")
	})
	newer := createEndpoint(t, ns, "ep-b-newer", gw.Name, "/users")
	eventually(t, func() error {
		ep, err := getEndpoint(newer)
		if err != nil {
			return err
		}
		return expectCondition(ep, "Accepted", metav1.ConditionFalse, "EndpointConflict")
	})
	oneConflictEvent := func() error {
		n, err := eventCount(ns, "ep-b-newer", "EndpointConflict")
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("EndpointConflict events on ep-b-newer = %d, want 1", n)
		}
		return nil
	}
	eventually(t, oneConflictEvent)

	// Each gateway reconcile re-evaluates acceptance; an unchanged verdict is
	// neither rewritten nor announced again.
	for i := range 3 {
		touchGateway(t, gw, strconv.Itoa(i))
	}
	consistently(t, 5*time.Second, func() error {
		if err := oneConflictEvent(); err != nil {
			return err
		}
		ep, err := getEndpoint(newer)
		if err != nil {
			return err
		}
		return expectCondition(ep, "Accepted", metav1.ConditionFalse, "EndpointConflict")
	})

	partial := createEndpoint(t, ns, "ep-c-partial", gw.Name, "/users", "/only-c")
	eventually(t, func() error {
		ep, err := getEndpoint(partial)
		if err != nil {
			return err
		}
		if err := expectCondition(ep, "Accepted", metav1.ConditionTrue, "PartiallyAccepted"); err != nil {
			return err
		}
		want := v1alpha1.EndpointConflict{Endpoint: "/users", Method: "GET", Winner: ns + "/ep-a-older"}
		if len(ep.Status.Conflicts) != 1 || ep.Status.Conflicts[0] != want {
			return fmt.Errorf("status.conflicts = %+v, want [%+v]", ep.Status.Conflicts, want)
		}
		return nil
	})
}

// setTimeout sets every entry's timeout on the endpoint key, which bumps its
// generation, retrying on write conflicts with the controllers' status writes.
func setTimeout(key client.ObjectKey, d time.Duration) error {
	backoff := wait.Backoff{Duration: 50 * time.Millisecond, Factor: 1.5, Jitter: 0.2, Steps: 10}
	return retry.RetryOnConflict(backoff, func() error {
		ep, err := getEndpoint(key)
		if err != nil {
			return err
		}
		for i := range ep.Spec.Endpoints {
			ep.Spec.Endpoints[i].Timeout = &metav1.Duration{Duration: d}
		}
		return k8sClient.Update(ctx, ep)
	})
}

// expectEndpointStatus checks all three endpoint conditions at the current
// generation, the derived phase, and that the legacy Available is gone.
func expectEndpointStatus(key client.ObjectKey, accepted, ready metav1.ConditionStatus,
	acceptedReason, readyReason string, phase v1alpha1.EndpointPhase) error {
	ep, err := getEndpoint(key)
	if err != nil {
		return err
	}
	if err := expectCondition(ep, "ResolvedRefs", metav1.ConditionTrue, "RefsResolved"); err != nil {
		return err
	}
	if err := expectCondition(ep, "Accepted", accepted, acceptedReason); err != nil {
		return err
	}
	if err := expectCondition(ep, "Ready", ready, readyReason); err != nil {
		return err
	}
	if ep.Status.Phase != phase {
		return fmt.Errorf("%s: phase %q, want %q", ep.Name, ep.Status.Phase, phase)
	}
	if meta.FindStatusCondition(ep.Status.Conditions, "Available") != nil {
		return fmt.Errorf("%s: legacy Available condition still present", ep.Name)
	}
	return nil
}

func TestEndpointStatus_ConflictedEndpointReportsBothWriters(t *testing.T) {
	ns := testNamespace(t)
	lossWatch := watchConditionLoss(t, ns)
	gw := createGateway(t, ns, "gw-both")
	older := createEndpoint(t, ns, "ep-a-older", gw.Name, "/users")
	olderReady := func() error {
		return expectEndpointStatus(older, metav1.ConditionTrue, metav1.ConditionTrue, "Accepted", "Ready",
			v1alpha1.EndpointPhaseActive)
	}
	eventually(t, olderReady)
	newer := createEndpoint(t, ns, "ep-b-newer", gw.Name, "/users", "/only-b")
	newerConflicted := func() error {
		return expectEndpointStatus(newer, metav1.ConditionFalse, metav1.ConditionFalse,
			"EndpointConflict", "EndpointConflict", v1alpha1.EndpointPhaseConflicted)
	}
	eventually(t, newerConflicted)

	// Drive both writers at once: spec changes on both endpoints re-run the
	// endpoint and gateway controllers together, and gateway annotations
	// re-run the gateway controller.
	for i := 1; i <= 3; i++ {
		if err := setTimeout(older, time.Duration(i)*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := setTimeout(newer, time.Duration(i)*time.Second); err != nil {
			t.Fatal(err)
		}
		touchGateway(t, gw, strconv.Itoa(i))
	}
	eventually(t, olderReady)
	eventually(t, newerConflicted)
	consistently(t, 5*time.Second, newerConflicted)
	lossWatch.requireNoLoss(t, older.Name, newer.Name)

	// Steady state: further gateway reconciles write nothing to the endpoint.
	before, err := getEndpoint(newer)
	if err != nil {
		t.Fatal(err)
	}
	touchGateway(t, gw, "steady-1")
	touchGateway(t, gw, "steady-2")
	consistently(t, 5*time.Second, func() error {
		cur, err := getEndpoint(newer)
		if err != nil {
			return err
		}
		if cur.ResourceVersion != before.ResourceVersion {
			return fmt.Errorf("ep-b-newer rewritten in steady state: resourceVersion %s -> %s",
				before.ResourceVersion, cur.ResourceVersion)
		}
		return nil
	})
}

func TestEndpointStatus_ConcurrentSpecChangesConverge(t *testing.T) {
	ns := testNamespace(t)
	lossWatch := watchConditionLoss(t, ns)
	gw := createGateway(t, ns, "gw-race")
	// ep-a and ep-b share GET /x; ep-a is older and sorts first, so it wins.
	keys := []client.ObjectKey{
		createEndpoint(t, ns, "ep-a", gw.Name, "/x"),
		createEndpoint(t, ns, "ep-b", gw.Name, "/x"),
		createEndpoint(t, ns, "ep-c", gw.Name, "/y"),
		createEndpoint(t, ns, "ep-d", gw.Name, "/z"),
	}
	for round := 1; round <= 5; round++ {
		var wg sync.WaitGroup
		errs := make(chan error, len(keys))
		for _, key := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- setTimeout(key, time.Duration(round)*time.Second)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
	eventuallyWithin(t, 90*time.Second, func() error {
		for _, key := range keys {
			var err error
			if key.Name == "ep-b" {
				err = expectEndpointStatus(key, metav1.ConditionFalse, metav1.ConditionFalse,
					"EndpointConflict", "EndpointConflict", v1alpha1.EndpointPhaseConflicted)
			} else {
				err = expectEndpointStatus(key, metav1.ConditionTrue, metav1.ConditionTrue,
					"Accepted", "Ready", v1alpha1.EndpointPhaseActive)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	lossWatch.requireNoLoss(t, "ep-a", "ep-b", "ep-c", "ep-d")
}

func TestEndpoint_ReattachesWhenGatewayCreatedLater(t *testing.T) {
	ns := testNamespace(t)
	ep := createEndpoint(t, ns, "ep-early", "gw-late", "/early")
	eventually(t, func() error {
		cur, err := getEndpoint(ep)
		if err != nil {
			return err
		}
		if err := expectCondition(cur, "ResolvedRefs", metav1.ConditionFalse, "GatewayNotFound"); err != nil {
			return err
		}
		if cur.Status.Phase != v1alpha1.EndpointPhaseDetached {
			return fmt.Errorf("phase %q, want Detached", cur.Status.Phase)
		}
		return nil
	})
	createGateway(t, ns, "gw-late")
	eventually(t, func() error {
		return expectEndpointStatus(ep, metav1.ConditionTrue, metav1.ConditionTrue, "Accepted", "Ready",
			v1alpha1.EndpointPhaseActive)
	})
}

func TestEndpoint_DetachedWhenGatewayDeleted(t *testing.T) {
	ns := testNamespace(t)
	gw := createGateway(t, ns, "gw-going")
	ep := createEndpoint(t, ns, "ep-staying", gw.Name, "/stay")
	eventually(t, func() error {
		return expectEndpointStatus(ep, metav1.ConditionTrue, metav1.ConditionTrue, "Accepted", "Ready",
			v1alpha1.EndpointPhaseActive)
	})
	if err := k8sClient.Delete(ctx, &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		cur, err := getEndpoint(ep)
		if err != nil {
			return err
		}
		if err := expectCondition(cur, "Ready", metav1.ConditionFalse, "GatewayNotFound"); err != nil {
			return err
		}
		if cur.Status.Phase != v1alpha1.EndpointPhaseDetached {
			return fmt.Errorf("phase %q, want Detached", cur.Status.Phase)
		}
		return nil
	})
}

// conditionLossWatch observes every version of the endpoints in a namespace
// and records a violation when a condition one version carried is missing
// from a later version. Only the endpoint-status tests use it: they reference
// no policy, so no controller may legitimately remove ResolvedRefs, Accepted
// or Ready once it has written them. A status write built from a stale read
// would drop the other writer's condition, which a final-state check can miss
// because the next reconcile writes the condition back.
type conditionLossWatch struct {
	stop       context.CancelFunc
	done       chan struct{}
	mu         sync.Mutex
	violations []string
	observed   map[string]int
}

// watchConditionLoss starts watching endpoints in ns. The watch is open
// before it returns, so callers start it before creating the endpoints.
func watchConditionLoss(t *testing.T, ns string) *conditionLossWatch {
	t.Helper()
	wc, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8sClient.Scheme()})
	if err != nil {
		t.Fatalf("creating watch client: %v", err)
	}
	wctx, stop := context.WithCancel(ctx)
	w, err := wc.Watch(wctx, &v1alpha1.KrakenDEndpointList{}, client.InNamespace(ns))
	if err != nil {
		stop()
		t.Fatalf("watching endpoints: %v", err)
	}
	cw := &conditionLossWatch{stop: stop, done: make(chan struct{}), observed: map[string]int{}}
	go cw.run(wctx, w)
	t.Cleanup(stop)
	return cw
}

func (cw *conditionLossWatch) run(wctx context.Context, w watch.Interface) {
	defer close(cw.done)
	defer w.Stop()
	seen := map[string]map[string]bool{}
	for {
		select {
		case <-wctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				if wctx.Err() == nil {
					cw.record("watch closed early")
				}
				return
			}
			ep, isEp := ev.Object.(*v1alpha1.KrakenDEndpoint)
			if !isEp || (ev.Type != watch.Added && ev.Type != watch.Modified) {
				continue
			}
			cw.observe(ep.Name)
			if seen[ep.Name] == nil {
				seen[ep.Name] = map[string]bool{}
			}
			for _, typ := range []string{"ResolvedRefs", "Accepted", "Ready"} {
				present := meta.FindStatusCondition(ep.Status.Conditions, typ) != nil
				switch {
				case present:
					seen[ep.Name][typ] = true
				case seen[ep.Name][typ]:
					cw.record(fmt.Sprintf("%s: %s disappeared in resourceVersion %s", ep.Name, typ, ep.ResourceVersion))
				}
			}
		}
	}
}

func (cw *conditionLossWatch) record(v string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.violations = append(cw.violations, v)
}

func (cw *conditionLossWatch) observe(name string) {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	cw.observed[name]++
}

// requireNoLoss stops the watch and fails the test if any condition was lost,
// the watch closed before this call, or it never saw one of the named
// endpoints: a watch that saw nothing proves nothing.
func (cw *conditionLossWatch) requireNoLoss(t *testing.T, endpoints ...string) {
	t.Helper()
	cw.stop()
	<-cw.done
	cw.mu.Lock()
	defer cw.mu.Unlock()
	for _, name := range endpoints {
		if cw.observed[name] == 0 {
			cw.violations = append(cw.violations, name+": no events observed")
		}
	}
	if len(cw.violations) > 0 {
		t.Fatalf("a status write dropped another writer's condition:\n  %s", strings.Join(cw.violations, "\n  "))
	}
}

func TestPolicy_ReferencedByFollowsPolicyRefChange(t *testing.T) {
	ns := testNamespace(t)
	createGateway(t, ns, "gw-refs")
	for _, name := range []string{"pol-a", "pol-b"} {
		if err := k8sClient.Create(ctx, &v1alpha1.KrakenDBackendPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		}); err != nil {
			t.Fatalf("create policy %s: %v", name, err)
		}
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-refs", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw-refs"},
			Endpoints: []v1alpha1.EndpointEntry{{
				Endpoint: "/refs", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{
					Host: []string{"http://svc:8080"}, URLPattern: "/refs",
					PolicyRef: &v1alpha1.PolicyRef{Name: "pol-a"},
				}},
			}},
		},
	}
	if err := k8sClient.Create(ctx, ep); err != nil {
		t.Fatal(err)
	}
	expectRefs := func(a, b int) func() error {
		return func() error {
			for name, want := range map[string]int{"pol-a": a, "pol-b": b} {
				var p v1alpha1.KrakenDBackendPolicy
				if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
					return err
				}
				if p.Status.ReferencedBy != want {
					return fmt.Errorf("%s referencedBy = %d, want %d", name, p.Status.ReferencedBy, want)
				}
				ready := meta.FindStatusCondition(p.Status.Conditions, "Ready")
				if ready == nil || ready.Status != metav1.ConditionTrue || p.Status.ObservedGeneration != p.Generation {
					return fmt.Errorf("%s: Ready %+v, observedGeneration %d, want True at %d",
						name, ready, p.Status.ObservedGeneration, p.Generation)
				}
			}
			return nil
		}
	}
	eventually(t, expectRefs(1, 0))

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cur, err := getEndpoint(client.ObjectKeyFromObject(ep))
		if err != nil {
			return err
		}
		cur.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: "pol-b"}
		return k8sClient.Update(ctx, cur)
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, expectRefs(0, 1))
}
