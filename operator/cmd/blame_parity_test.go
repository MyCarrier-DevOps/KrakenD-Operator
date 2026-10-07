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

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	licenseutil "github.com/mycarrier-devops/krakend-operator/internal/util/license"
	"github.com/mycarrier-devops/krakend-operator/internal/webhook"
)

// judgingExecutor stands in for the krakend binary: it reads the config named
// by -c and, when judge refuses it, exits 1 printing judge's output, as
// krakend check does.
type judgingExecutor struct {
	judge func(config string) (output string, refused bool)
}

func (e judgingExecutor) Execute(ctx context.Context, _ string, args ...string) ([]byte, error) {
	config, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		return nil, err
	}
	if output, refused := e.judge(string(config)); refused {
		return []byte(output), exec.CommandContext(ctx, "sh", "-c", "exit 1").Run()
	}
	return []byte("Syntax OK!"), nil
}

// verdictRecorder is the pod's one checker, recording each endpoint verdict
// any caller reaches, by endpoint.
type verdictRecorder struct {
	*configcheck.Checker
	mu       sync.Mutex
	verdicts map[string][]configcheck.EndpointVerdict
}

func (v *verdictRecorder) CheckEndpoint(ctx context.Context, u configcheck.EndpointUnit,
	memo configcheck.Memo) (configcheck.EndpointVerdict, error) {
	verdict, err := v.Checker.CheckEndpoint(ctx, u, memo)
	v.mu.Lock()
	defer v.mu.Unlock()
	key := u.Endpoint.Namespace + "/" + u.Endpoint.Name
	v.verdicts[key] = append(v.verdicts[key], verdict)
	return verdict, err
}

// blameWorld is gateway infra/gw (CE) with its tenants' objects, written past
// admission as stored objects arrive, and the pod's one checker over a krakend
// stand-in that refuses what judge refuses. The gateway controller and the
// admission webhooks share that checker.
type blameWorld struct {
	c         client.Client
	gw        *v1alpha1.KrakenDGateway
	checker   *verdictRecorder
	gateways  *controller.KrakenDGatewayReconciler
	endpoints *webhook.EndpointValidator
}

func newBlameWorld(t *testing.T, judge func(config string) (string, bool), objs ...client.Object) *blameWorld {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: v1alpha1.EditionCE}}
	objs = append([]client.Object{gw}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(objs...).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointGateway, fieldindex.EndpointGatewayKeys).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointPolicy, fieldindex.EndpointPolicyKeys).
		Build()
	checker := &verdictRecorder{verdicts: map[string][]configcheck.EndpointVerdict{},
		Checker: configcheck.New(c, renderer.New(renderer.Options{}), renderer.NewValidator(
			renderer.ValidatorOptions{Executor: judgingExecutor{judge: judge}, BinaryPath: "krakend"}), 1)}
	return &blameWorld{c: c, gw: gw, checker: checker,
		gateways: &controller.KrakenDGatewayReconciler{Client: c, APIReader: c, Scheme: scheme,
			Recorder: record.NewFakeRecorder(100), Renderer: renderer.New(renderer.Options{}), Checker: checker,
			Clock: clock.RealClock{}, LicenseParser: licenseutil.NewX509LicenseParser()},
		endpoints: &webhook.EndpointValidator{Client: c, Checker: checker},
	}
}

// reconcile runs one pass of the gateway controller.
func (w *blameWorld) reconcile(t *testing.T) {
	t.Helper()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(w.gw)}
	if _, err := w.gateways.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// stored reads ep as it is stored now.
func (w *blameWorld) stored(t *testing.T, ep *v1alpha1.KrakenDEndpoint) *v1alpha1.KrakenDEndpoint {
	t.Helper()
	var got v1alpha1.KrakenDEndpoint
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(ep), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

// accepted reads ep's stored Accepted condition.
func (w *blameWorld) accepted(t *testing.T, ep *v1alpha1.KrakenDEndpoint) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(w.stored(t, ep).Status.Conditions, v1alpha1.ConditionAccepted)
}

// tenantEndpoint is an endpoint of namespace ns on gateway infra/gw serving
// GET path from host.
func tenantEndpoint(ns, name, path, host string) *v1alpha1.KrakenDEndpoint {
	return &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw", Namespace: "infra"},
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: path, Method: "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{host}, URLPattern: "/x"}}}},
		},
	}
}

// TestBlame_AdmissionAndTheControllerAgree runs the review rounds'
// attribution attacks through both paths over one checker. An attacker's
// stored endpoint makes its own check print text naming the victim's route.
// Each object is judged on its own, so the text reaches only the attacker's
// own verdict. The victim is served and can be written, and admission reaches
// the same verdict on the attacker as the controller.
func TestBlame_AdmissionAndTheControllerAgree(t *testing.T) {
	forgeries := map[string]string{
		"krakend's endpoint print": "ERROR parsing the configuration file: 'endpoint: GET /orders-b, backend: 0'",
		"a placeholder list":       "undefined output param 'x'! endpoint: GET /orders-a, backend: 0. input: [GET /orders-b], output: [x]",
		"a {x} and :x shape twin":  "endpoint: GET /orders-b:cancel, backend: 0. input: [], output: [cancel]",
		"a forged lint pointer":    "bad host\n- at '/endpoints/0/endpoint': owned by tenant-b",
	}
	for name, forged := range forgeries {
		t.Run(name, func(t *testing.T) {
			attacker := tenantEndpoint("tenant-a", "orders-a", "/orders-a", "http://attacker.invalid")
			victim := tenantEndpoint("tenant-b", "orders-b", "/orders-b", "http://svc")
			w := newBlameWorld(t, func(config string) (string, bool) {
				return forged, strings.Contains(config, "attacker.invalid")
			}, attacker, victim)
			ctx := context.Background()

			// The controller excludes the attacker alone.
			w.reconcile(t)
			if got := w.accepted(t, victim); got == nil || got.Reason != v1alpha1.ReasonAccepted {
				t.Errorf("victim Accepted = %+v, want Accepted", got)
			}
			if got := w.accepted(t, attacker); got == nil || got.Reason != v1alpha1.ReasonEndpointInvalid {
				t.Errorf("attacker Accepted = %+v, want EndpointInvalid", got)
			}

			// Admission lets the victim's tenant write while the attacker is
			// stored, and judges the attacker as the controller did.
			if _, err := w.endpoints.ValidateCreate(ctx,
				tenantEndpoint("tenant-b", "orders-c", "/orders-c", "http://svc")); err != nil {
				t.Errorf("the victim's tenant cannot create an endpoint: %v", err)
			}
			stored := w.stored(t, attacker)
			updated := stored.DeepCopy()
			updated.Spec.Endpoints[0].Backends[0].URLPattern = "/y"
			warnings, err := w.endpoints.ValidateUpdate(ctx, stored, updated)
			if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "fails krakend check on its own") {
				t.Errorf("attacker update: %v, %v; want admitted with a warning quoting its own output", warnings, err)
			}
			got := w.checker.verdicts["tenant-a/orders-a"]
			// The controller's verdict, then admission's on the update, then
			// admission's on the stored version, which is the controller's
			// endpoint as it is stored.
			if len(got) != 3 || !reflect.DeepEqual(got[0], got[2]) {
				t.Errorf("attacker verdicts = %+v, want the controller's and admission's on the stored version equal", got)
			}
		})
	}
}

// served lists "METHOD path" for every entry of the config the gateway
// applies.
func (w *blameWorld) served(t *testing.T) []string {
	t.Helper()
	var gw v1alpha1.KrakenDGateway
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(w.gw), &gw); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: gw.Namespace, Name: resources.ConfigMapName(&gw, gw.Status.ConfigChecksum)}
	if err := w.c.Get(context.Background(), key, &cm); err != nil {
		t.Fatalf("reading the applied config: %v", err)
	}
	var doc struct {
		Endpoints []struct {
			Endpoint string `json:"endpoint"`
			Method   string `json:"method"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal([]byte(cm.Data[resources.ConfigKey]), &doc); err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, e := range doc.Endpoints {
		routes = append(routes, e.Method+" "+e.Endpoint)
	}
	return routes
}

// aged sets ep's creationTimestamp i seconds after a fixed instant: of two
// clashing entries, the gateway serves the older endpoint's.
func aged(ep *v1alpha1.KrakenDEndpoint, i int) *v1alpha1.KrakenDEndpoint {
	ep.CreationTimestamp = metav1.NewTime(time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC))
	return ep
}

// TestBlame_RemovingAnEndpointEvictsNoOtherEntry: in each scenario the oldest
// endpoint serves GET /a/{id}, and a newer tenant's entry loses to an entry
// that is not served itself:
//   - x beats z's GET /a/{name}/x; z beats v's GET /a/{id}/y; u's GET /b
//     clashes with nobody;
//   - with router.auto_options, s beats d's GET /a/{name}/x, and d beats e's
//     POST /a/{id}: the OPTIONS /a/{id} route of e's path clashes with the
//     OPTIONS route of d's, whether or not s's entry already added it;
//   - a's GET /a/{id} beats b's GET /a/{key}, the same route shape, and b
//     beats c's GET /a/{id}/y, which fits a's route but not b's.
//
// The oldest endpoint's owner may delete it or move it off /a/{id};
// afterwards the gateway serves every entry it served before apart from that
// endpoint's own, and the loser's verdict does not change. No tenant's
// removal evicts another tenant's entry.
func TestBlame_RemovingAnEndpointEvictsNoOtherEntry(t *testing.T) {
	scenarios := map[string]struct {
		router *v1alpha1.RouterConfig
		build  func() (oldest, loser *v1alpha1.KrakenDEndpoint, all []client.Object)
	}{
		"x, z, v and u": {nil, func() (*v1alpha1.KrakenDEndpoint, *v1alpha1.KrakenDEndpoint, []client.Object) {
			x := aged(tenantEndpoint("tenant-x", "x", "/a/{id}", "http://svc"), 0)
			z := aged(tenantEndpoint("tenant-z", "z", "/a/{name}/x", "http://svc"), 1)
			v := aged(tenantEndpoint("tenant-v", "v", "/a/{id}/y", "http://svc"), 2)
			u := aged(tenantEndpoint("tenant-u", "u", "/b", "http://svc"), 3)
			return x, v, []client.Object{x, z, v, u}
		}},
		"a, b and c": {nil, func() (*v1alpha1.KrakenDEndpoint, *v1alpha1.KrakenDEndpoint, []client.Object) {
			a := aged(tenantEndpoint("tenant-a", "a", "/a/{id}", "http://svc"), 0)
			b := aged(tenantEndpoint("tenant-b", "b", "/a/{key}", "http://svc"), 1)
			c := aged(tenantEndpoint("tenant-c", "c", "/a/{id}/y", "http://svc"), 2)
			return a, c, []client.Object{a, b, c}
		}},
		"s, d and e with auto_options": {&v1alpha1.RouterConfig{AutoOptions: true},
			func() (*v1alpha1.KrakenDEndpoint, *v1alpha1.KrakenDEndpoint, []client.Object) {
				s := aged(tenantEndpoint("tenant-s", "s", "/a/{id}", "http://svc"), 0)
				d := aged(tenantEndpoint("tenant-d", "d", "/a/{name}/x", "http://svc"), 1)
				e := aged(tenantEndpoint("tenant-e", "e", "/a/{id}", "http://svc"), 2)
				e.Spec.Endpoints[0].Method = "POST"
				return s, e, []client.Object{s, d, e}
			}},
	}
	removals := map[string]func(ctx context.Context, w *blameWorld, oldest *v1alpha1.KrakenDEndpoint) error{
		"deleting it": func(ctx context.Context, w *blameWorld, oldest *v1alpha1.KrakenDEndpoint) error {
			if _, err := w.endpoints.ValidateDelete(ctx, oldest); err != nil {
				return err
			}
			return w.c.Delete(ctx, oldest)
		},
		"moving it off /a/{id}": func(ctx context.Context, w *blameWorld, oldest *v1alpha1.KrakenDEndpoint) error {
			moved := oldest.DeepCopy()
			moved.Spec.Endpoints[0].Endpoint = "/c/{id}"
			if _, err := w.endpoints.ValidateUpdate(ctx, oldest, moved); err != nil {
				return err
			}
			return w.c.Update(ctx, moved)
		},
	}
	accept := func(string) (string, bool) { return "", false }
	for sname, sc := range scenarios {
		for rname, remove := range removals {
			t.Run(sname+", "+rname, func(t *testing.T) {
				oldest, loser, all := sc.build()
				w := newBlameWorld(t, accept, all...)
				w.setRouter(t, sc.router)
				w.reconcile(t)
				before, loserBefore := w.served(t), w.accepted(t, loser)

				if err := remove(context.Background(), w, w.stored(t, oldest)); err != nil {
					t.Fatalf("the oldest endpoint's owner cannot do it: %v", err)
				}
				w.reconcile(t)

				after := w.served(t)
				var evicted []string
				for _, route := range before {
					if route != "GET /a/{id}" && !slices.Contains(after, route) {
						evicted = append(evicted, route)
					}
				}
				if len(evicted) != 0 {
					t.Errorf("served before and not after: %v (before %v, after %v)", evicted, before, after)
				}
				loserAfter := w.accepted(t, loser)
				if loserBefore == nil || loserAfter == nil || loserAfter.Reason != loserBefore.Reason ||
					loserAfter.Message != loserBefore.Message {
					t.Errorf("%s's Accepted went from %+v to %+v, want it unchanged", loser.Name, loserBefore, loserAfter)
				}
			})
		}
	}
}

// setRouter sets the gateway's router block, as its owner would.
func (w *blameWorld) setRouter(t *testing.T, router *v1alpha1.RouterConfig) {
	t.Helper()
	var gw v1alpha1.KrakenDGateway
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(w.gw), &gw); err != nil {
		t.Fatal(err)
	}
	gw.Spec.Config.Router = router
	if err := w.c.Update(context.Background(), &gw); err != nil {
		t.Fatal(err)
	}
}
