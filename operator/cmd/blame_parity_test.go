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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// krakend check does. The output ends with a digest of the config, so that two
// refusals are equal only when both paths rendered the same config. That holds
// across the controller and admission only in this world (CE, no Dragonfly):
// admission renders no Dragonfly and reads the CE fallback from status, so a
// Dragonfly fixture must not reuse this equality.
type judgingExecutor struct {
	judge func(config string) (output string, refused bool)
}

func (e judgingExecutor) Execute(ctx context.Context, _ string, args ...string) ([]byte, error) {
	config, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		return nil, err
	}
	if output, refused := e.judge(string(config)); refused {
		sum := sha256.Sum256([]byte(config))
		return []byte(output + " #" + hex.EncodeToString(sum[:6])),
			exec.CommandContext(ctx, "sh", "-c", "exit 1").Run()
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

// take returns the verdicts recorded for the endpoint key (namespace/name) and
// forgets them.
func (v *verdictRecorder) take(key string) []configcheck.EndpointVerdict {
	v.mu.Lock()
	defer v.mu.Unlock()
	taken := v.verdicts[key]
	delete(v.verdicts, key)
	return taken
}

// sawVerdict reports whether verdicts holds one equal to want.
func sawVerdict(verdicts []configcheck.EndpointVerdict, want configcheck.EndpointVerdict) bool {
	return slices.ContainsFunc(verdicts, func(v configcheck.EndpointVerdict) bool { return reflect.DeepEqual(v, want) })
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
	gateway   *webhook.GatewayValidator
	endpoints *webhook.EndpointValidator
	policies  *webhook.PolicyValidator
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
	vs := webhook.NewValidators(c, c, checker, "")
	return &blameWorld{c: c, gw: gw, checker: checker,
		gateways: &controller.KrakenDGatewayReconciler{Client: c, APIReader: c, Scheme: scheme,
			Recorder: record.NewFakeRecorder(100), Renderer: renderer.New(renderer.Options{}), Checker: checker,
			Clock: clock.RealClock{}, LicenseParser: licenseutil.NewX509LicenseParser()},
		gateway:   vs.Gateway,
		endpoints: vs.Endpoint,
		policies:  vs.Policy,
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

			controllerSaw := w.checker.take("tenant-a/orders-a")

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
			// The controller's verdict carries a digest of the config it judged.
			// Admission judged the update and then the stored version, which is
			// the controller's endpoint as it is stored: its verdict is among the
			// controller's.
			admissionSaw := w.checker.take("tenant-a/orders-a")
			if len(controllerSaw) == 0 || len(admissionSaw) == 0 ||
				!sawVerdict(controllerSaw, admissionSaw[len(admissionSaw)-1]) {
				t.Errorf("attacker verdicts: controller %+v, admission %+v; want admission's on the stored version "+
					"among the controller's", controllerSaw, admissionSaw)
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

// markApplied records the gateway's current render as its applied config, as
// an earlier release leaves it: the next pass finds nothing new to apply.
func (w *blameWorld) markApplied(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	in, err := w.checker.Gather(ctx, w.gw, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderer.New(renderer.Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var gw v1alpha1.KrakenDGateway
	if err := w.c.Get(ctx, client.ObjectKeyFromObject(w.gw), &gw); err != nil {
		t.Fatal(err)
	}
	gw.Status.ConfigChecksum, gw.Status.ConfigEdition = out.Checksum, v1alpha1.EditionCE
	if err := w.c.Status().Update(ctx, &gw); err != nil {
		t.Fatal(err)
	}
}

// TestBlame_AMaskedEndpointIsJudgedAlikeOnBothPaths: e's second entry, GET
// /shared, loses to f's older GET /shared, so the gateway's render leaves it
// out, and that render passes. The entry fails krakend check on its own (its
// host). A passing render vouches only for what it rendered: the controller
// judges e on its own and excludes it, also when an earlier release already
// applied that render, and admission's update ratchet reaches the same
// verdict on the stored e.
func TestBlame_AMaskedEndpointIsJudgedAlikeOnBothPaths(t *testing.T) {
	for name, alreadyApplied := range map[string]bool{"on a new render": false, "on a render already applied": true} {
		t.Run(name, func(t *testing.T) {
			f := aged(tenantEndpoint("tenant-f", "f", "/shared", "http://svc"), 0)
			e := aged(tenantEndpoint("tenant-e", "e", "/e", "http://svc"), 1)
			e.Spec.Endpoints = append(e.Spec.Endpoints, v1alpha1.EndpointEntry{Endpoint: "/shared", Method: "GET",
				Backends: []v1alpha1.BackendSpec{{Host: []string{"http://masked.invalid"}, URLPattern: "/x"}}})
			w := newBlameWorld(t, func(config string) (string, bool) {
				return "- at '/endpoints/1/backend/0/host/0': masked.invalid is refused",
					strings.Contains(config, "masked.invalid")
			}, f, e)
			if alreadyApplied {
				w.markApplied(t)
			}

			w.reconcile(t)

			if got := w.accepted(t, f); got == nil || got.Reason != v1alpha1.ReasonAccepted {
				t.Errorf("f Accepted = %+v, want Accepted", got)
			}
			if got := w.accepted(t, e); got == nil || got.Reason != v1alpha1.ReasonEndpointInvalid {
				t.Fatalf("e Accepted = %+v, want EndpointInvalid: the render that passed left its failing entry out", got)
			}
			controllerSaw := w.checker.take("tenant-e/e")
			stored := w.stored(t, e)
			updated := stored.DeepCopy()
			updated.Spec.Endpoints[0].Backends[0].URLPattern = "/y"
			warnings, err := w.endpoints.ValidateUpdate(context.Background(), stored, updated)
			if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "fails krakend check on its own") {
				t.Errorf("e's update: %v, %v; want admitted with a warning quoting its own output", warnings, err)
			}
			// Admission judged the update and then the stored e, whose verdict is
			// among the controller's.
			admissionSaw := w.checker.take("tenant-e/e")
			if len(controllerSaw) == 0 || len(admissionSaw) == 0 ||
				!sawVerdict(controllerSaw, admissionSaw[len(admissionSaw)-1]) {
				t.Errorf("e's verdicts: controller %+v, admission %+v; want admission's on the stored e "+
					"among the controller's", controllerSaw, admissionSaw)
			}
		})
	}
}

// usingPolicy makes ep's backend reference KrakenDBackendPolicy ns/name.
func usingPolicy(ep *v1alpha1.KrakenDEndpoint, ns, name string) *v1alpha1.KrakenDEndpoint {
	ep.Spec.Endpoints[0].Backends[0].PolicyRef = &v1alpha1.PolicyRef{Name: name, Namespace: ns}
	return ep
}

// rawPolicy is KrakenDBackendPolicy ns/name whose backend extra_config is raw.
func rawPolicy(ns, name, raw string) *v1alpha1.KrakenDBackendPolicy {
	return &v1alpha1.KrakenDBackendPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.KrakenDBackendPolicySpec{Raw: &runtime.RawExtension{Raw: []byte(raw)}}}
}

// TestBlame_AForeignPolicyIsNamedAlikeOnBothPaths: tenant-e's endpoint
// references tenant-p's policy and fails krakend check only together with it.
// Both paths blame the policy by name with the same words, PolicyInvalid, and
// show none of its content.
func TestBlame_AForeignPolicyIsNamedAlikeOnBothPaths(t *testing.T) {
	p := rawPolicy("tenant-p", "p", `{"tenant-p/config":{"token":"tenant-p-secret"}}`)
	e := usingPolicy(tenantEndpoint("tenant-e", "e", "/e", "http://combo.invalid"), "tenant-p", "p")
	w := newBlameWorld(t, func(config string) (string, bool) {
		return "- at '/endpoints/0/backend/0/extra_config': tenant-p-secret refused for combo.invalid",
			strings.Contains(config, "tenant-p-secret") && strings.Contains(config, "combo.invalid")
	}, p, e)

	w.reconcile(t)
	_, denied := w.endpoints.ValidateCreate(context.Background(),
		usingPolicy(tenantEndpoint("tenant-e", "e2", "/e2", "http://combo.invalid"), "tenant-p", "p"))

	controllerSaw, admissionSaw := w.checker.verdicts["tenant-e/e"], w.checker.verdicts["tenant-e/e2"]
	if len(controllerSaw) == 0 || len(admissionSaw) != 1 || admissionSaw[0].Reason != v1alpha1.ReasonPolicyInvalid ||
		!reflect.DeepEqual(controllerSaw[len(controllerSaw)-1], admissionSaw[0]) {
		t.Fatalf("verdicts: controller %+v, admission %+v; want the same PolicyInvalid", controllerSaw, admissionSaw)
	}
	words := admissionSaw[0].Message(1024)
	got := w.accepted(t, e)
	if got == nil || got.Reason != v1alpha1.ReasonPolicyInvalid || !strings.Contains(got.Message, words) ||
		!strings.Contains(words, "tenant-p/p") || strings.Contains(got.Message, "tenant-p-secret") {
		t.Errorf("e Accepted = %+v, want PolicyInvalid saying %q, without the policy's content", got, words)
	}
	if denied == nil || !strings.Contains(denied.Error(), words) || strings.Contains(denied.Error(), "tenant-p-secret") {
		t.Errorf("the create's denial = %v, want it to say %q, without the policy's content", denied, words)
	}
}

// TestBlame_APolicyChangeIsDeniedByTheEndpointItNewlyBreaks: both endpoints
// use tenant-p's policy. tenant-a's already fails on its own and was never
// judged, so it still counts as served. A change to the policy that breaks
// tenant-b's endpoint is denied for tenant-b's endpoint, although the gateway
// with the change fails for tenant-a's too: an endpoint that already fails
// never masks one the change newly breaks.
func TestBlame_APolicyChangeIsDeniedByTheEndpointItNewlyBreaks(t *testing.T) {
	p := rawPolicy("tenant-p", "p", `{"tenant-p/config":{"mode":"old"}}`)
	a := usingPolicy(tenantEndpoint("tenant-a", "a", "/a", "http://already.invalid"), "tenant-p", "p")
	b := usingPolicy(tenantEndpoint("tenant-b", "b", "/b", "http://b.svc"), "tenant-p", "p")
	w := newBlameWorld(t, func(config string) (string, bool) {
		switch {
		case strings.Contains(config, "already.invalid"):
			return "- at '/endpoints/0/backend/0/host/0': already.invalid is refused", true
		case strings.Contains(config, "breaks-b") && strings.Contains(config, "b.svc"):
			return "- at '/endpoints/0/backend/0/extra_config': b.svc cannot take breaks-b", true
		}
		return "", false
	}, p, a, b)
	changed := p.DeepCopy()
	changed.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"tenant-p/config":{"mode":"breaks-b"}}`)}

	_, err := w.policies.ValidateUpdate(context.Background(), p, changed)

	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Code != http.StatusUnprocessableEntity ||
		!strings.Contains(err.Error(), "tenant-b/b") || strings.Contains(err.Error(), "tenant-a/a") ||
		strings.Contains(err.Error(), "b.svc") || strings.Contains(err.Error(), "breaks-b") {
		t.Errorf("policy update: %v; want a 422 naming tenant-b/b alone, quoting no krakend output", err)
	}

	// Stored past admission, the change makes the controller judge the same
	// way: b is blamed on the policy by name, a on its own failure.
	var stored v1alpha1.KrakenDBackendPolicy
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(p), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Raw = changed.Spec.Raw
	if err := w.c.Update(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	w.reconcile(t)
	if got := w.accepted(t, b); got == nil || got.Reason != v1alpha1.ReasonPolicyInvalid ||
		!strings.Contains(got.Message, "tenant-p/p") || strings.Contains(got.Message, "breaks-b") ||
		strings.Contains(got.Message, "b.svc") {
		t.Errorf("b Accepted = %+v, want PolicyInvalid naming tenant-p/p, quoting none of b's krakend output", got)
	}
	if got := w.accepted(t, a); got == nil || got.Reason != v1alpha1.ReasonEndpointInvalid {
		t.Errorf("a Accepted = %+v, want EndpointInvalid", got)
	}
}

// setTimeout sets the gateway's timeout as its owner would, past admission.
func (w *blameWorld) setTimeout(t *testing.T, timeout string) {
	t.Helper()
	var gw v1alpha1.KrakenDGateway
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(w.gw), &gw); err != nil {
		t.Fatal(err)
	}
	gw.Spec.Config.Timeout = timeout
	if err := w.c.Update(context.Background(), &gw); err != nil {
		t.Fatal(err)
	}
}

// admitTimeout is gateway admission's answer to updating the stored gateway's
// timeout.
func (w *blameWorld) admitTimeout(t *testing.T, timeout string) ([]string, error) {
	t.Helper()
	ctx := context.Background()
	var stored v1alpha1.KrakenDGateway
	if err := w.c.Get(ctx, client.ObjectKeyFromObject(w.gw), &stored); err != nil {
		t.Fatal(err)
	}
	updated := stored.DeepCopy()
	updated.Spec.Config.Timeout = timeout
	warnings, err := w.gateway.ValidateUpdate(ctx, &stored, updated)
	return warnings, err
}

// configValid reads the stored gateway's ConfigValid condition.
func (w *blameWorld) configValid(t *testing.T) *metav1.Condition {
	t.Helper()
	var gw v1alpha1.KrakenDGateway
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(w.gw), &gw); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionConfigValid)
}

// rootBrokenAt13s is a krakend stand-in whose root fails on its own at a
// timeout of 13s, and with which each of the others fails once the timeout is
// 17s (the owner's fix of the root).
func rootBrokenAt13s(failsAt17s ...string) func(string) (string, bool) {
	return func(config string) (string, bool) {
		if strings.Contains(config, `"13s"`) {
			return "root is broken", true
		}
		if !strings.Contains(config, `"17s"`) {
			return "", false
		}
		for _, host := range failsAt17s {
			if !strings.Contains(config, host) {
				return "", false
			}
		}
		return "fails with 17s", true
	}
}

// TestBlame_AFailingStoredRootDoesNotHideWhatTheOwnersFixBreaks: the gateway's
// stored root fails on its own, so the controller blames no endpoint and keeps
// serving the last applied config. The owner writes a root that passes. An
// endpoint that fails with it, and that the last applied config served, is
// broken by that write: admission denies it by name, as it does when the
// stored root passes, and the controller then excludes it.
func TestBlame_AFailingStoredRootDoesNotHideWhatTheOwnersFixBreaks(t *testing.T) {
	for name, storedTimeout := range map[string]string{"a stored root that passes": "5s", "a stored root that fails": "13s"} {
		t.Run(name, func(t *testing.T) {
			e1 := tenantEndpoint("tenant-a", "e1", "/a", "http://a-backend")
			w := newBlameWorld(t, rootBrokenAt13s("a-backend"), e1)
			w.setTimeout(t, "3s")
			w.reconcile(t)
			w.setTimeout(t, storedTimeout)
			w.reconcile(t)
			if got := w.served(t); !slices.Equal(got, []string{"GET /a"}) {
				t.Fatalf("before the write the gateway serves %v, want GET /a", got)
			}

			warnings, err := w.admitTimeout(t, "17s")

			if err == nil || !strings.Contains(err.Error(), "tenant-a/e1") {
				t.Errorf("admission of 17s = warnings %q, err %v; want a denial naming tenant-a/e1", warnings, err)
			}
			w.setTimeout(t, "17s")
			w.reconcile(t)
			if c := w.accepted(t, e1); c == nil || c.Reason != v1alpha1.ReasonEndpointInvalid {
				t.Errorf("after the write e1 Accepted = %v, want EndpointInvalid: the controller excludes what admission names", c)
			}
		})
	}
}

// TestBlame_AFailingStoredRootStillOnlyWarnsAboutAnEndpointNoConfigServed: the
// endpoint was never judged (the stored root was broken before the gateway
// applied anything), so nothing says the owner's write is what makes it fail.
// Admission warns, and the controller excludes the endpoint.
func TestBlame_AFailingStoredRootStillOnlyWarnsAboutAnEndpointNoConfigServed(t *testing.T) {
	e1 := tenantEndpoint("tenant-a", "e1", "/a", "http://a-backend")
	w := newBlameWorld(t, rootBrokenAt13s("a-backend"), e1)
	w.setTimeout(t, "13s")
	w.reconcile(t)

	warnings, err := w.admitTimeout(t, "17s")

	if err != nil || len(warnings) == 0 {
		t.Errorf("admission of 17s = warnings %q, err %v; want it admitted with a warning", warnings, err)
	}
	w.setTimeout(t, "17s")
	w.reconcile(t)
	if c := w.accepted(t, e1); c == nil || c.Reason != v1alpha1.ReasonEndpointInvalid {
		t.Errorf("after the write e1 Accepted = %v, want EndpointInvalid", c)
	}
}

// TestBlame_AFailingStoredRootDoesNotHideAFailureOnlyTogether: e1 and e2 each
// pass with the owner's fix, and fail only together. The stored root's failure
// says nothing about that, so admission denies the write, as it does when the
// stored root passes, rather than calling the failure old.
func TestBlame_AFailingStoredRootDoesNotHideAFailureOnlyTogether(t *testing.T) {
	for name, storedTimeout := range map[string]string{"a stored root that passes": "5s", "a stored root that fails": "13s"} {
		t.Run(name, func(t *testing.T) {
			e1 := tenantEndpoint("tenant-a", "e1", "/a", "http://a-backend")
			e2 := tenantEndpoint("tenant-b", "e2", "/b", "http://b-backend")
			w := newBlameWorld(t, rootBrokenAt13s("a-backend", "b-backend"), e1, e2)
			w.setTimeout(t, "3s")
			w.reconcile(t)
			w.setTimeout(t, storedTimeout)
			w.reconcile(t)

			warnings, err := w.admitTimeout(t, "17s")

			if err == nil || !strings.Contains(err.Error(), "fail validation together") {
				t.Errorf("admission of 17s = warnings %q, err %v; want a denial that they fail together", warnings, err)
			}
			w.setTimeout(t, "17s")
			w.reconcile(t)
			if c := w.configValid(t); c == nil || c.Reason != v1alpha1.ReasonCombinedConfigInvalid {
				t.Errorf("after the write ConfigValid = %v, want CombinedConfigInvalid: the controller refuses it too", c)
			}
		})
	}
}
