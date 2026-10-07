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
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// mapMemo is a Memo that keeps every verdict, for tests.
type mapMemo map[string]Verdict

func (m mapMemo) Lookup(key string) (Verdict, bool) { v, ok := m[key]; return v, ok }
func (m mapMemo) Store(key string, v Verdict)       { m[key] = v }

// renderedPaths returns the endpoint paths of a rendered config.
func renderedPaths(t *testing.T, rendered string) []string {
	t.Helper()
	var doc struct {
		Endpoints []struct {
			Endpoint string `json:"endpoint"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("parsing the checked config: %v", err)
	}
	paths := []string{}
	for _, e := range doc.Endpoints {
		paths = append(paths, e.Endpoint)
	}
	return paths
}

func TestCheckRoot_LintsTheGatewayWithNoEndpoint(t *testing.T) {
	val := &fakeValidator{}
	chk := newChecker(val, endpoint("stored", "/stored"))

	v, err := chk.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, nil)

	if err != nil || !v.OK {
		t.Fatalf("CheckRoot = %+v, %v; want OK", v, err)
	}
	if !reflect.DeepEqual(val.calls, []string{"lint"}) {
		t.Fatalf("calls = %v, want one lint", val.calls)
	}
	if paths := renderedPaths(t, val.seen[0]); len(paths) != 0 {
		t.Errorf("checked endpoints %v, want the root with none", paths)
	}
}

func TestCheckRoot_RemembersEachContentAndEdition(t *testing.T) {
	val := &fakeValidator{}
	chk := newChecker(val)
	memo := mapMemo{}
	gw := gateway(v1alpha1.EditionEE)

	for _, fallback := range []bool{false, false, true} {
		if _, err := chk.CheckRoot(context.Background(), Root{Gateway: gw, CEFallback: fallback}, memo); err != nil {
			t.Fatal(err)
		}
	}

	if want := []v1alpha1.Edition{v1alpha1.EditionEE, v1alpha1.EditionCE}; !reflect.DeepEqual(val.editions, want) {
		t.Errorf("checked as %v, want each edition once: the second EE check is remembered", val.editions)
	}
}

func TestCheckRoot_AnUnjudgedCheckIsNotRemembered(t *testing.T) {
	val := &fakeValidator{err: errors.New("fork/exec krakend: no such file or directory")}
	chk := newChecker(val)
	memo := mapMemo{}

	for range 2 {
		if _, err := chk.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, memo); err == nil {
			t.Fatal("CheckRoot succeeded with a validator that cannot run")
		}
	}

	if len(val.calls) != 2 || len(memo) != 0 {
		t.Errorf("ran %d times and remembered %d verdicts, want 2 runs and nothing remembered", len(val.calls), len(memo))
	}
}

func TestCheckGroup_ChecksOnlyItsEndpointsWithTheOverride(t *testing.T) {
	val := &fakeValidator{}
	stored := policy("p")
	chk := newChecker(val, stored, endpoint("other", "/other"))
	changed := policy("p")
	changed.Spec.CircuitBreaker.MaxErrors = 99

	_, err := chk.CheckGroup(context.Background(), Group{
		Gateway: gateway(v1alpha1.EditionCE), Endpoints: []v1alpha1.KrakenDEndpoint{*withPolicy(endpoint("mine", "/mine"), "p")},
		Override: changed,
	}, nil)

	if err != nil {
		t.Fatal(err)
	}
	if paths := renderedPaths(t, val.seen[0]); !reflect.DeepEqual(paths, []string{"/mine"}) {
		t.Errorf("checked endpoints %v, want only the group's", paths)
	}
	if !strings.Contains(val.seen[0], `"max_errors": 99`) {
		t.Errorf("checked config does not carry the override:\n%s", val.seen[0])
	}
}

func TestCheckPolicy_LintsThePolicyOnItsOwn(t *testing.T) {
	val := &fakeValidator{}
	chk := newChecker(val, endpoint("other", "/other"))
	memo := mapMemo{}

	for range 2 {
		v, err := chk.CheckPolicy(context.Background(), policy("p"), memo)
		if err != nil || !v.OK {
			t.Fatalf("CheckPolicy = %+v, %v", v, err)
		}
	}

	if len(val.calls) != 1 {
		t.Fatalf("ran %d times, want 1: the second check is remembered", len(val.calls))
	}
	if paths := renderedPaths(t, val.seen[0]); !reflect.DeepEqual(paths, []string{"/policy-lint"}) {
		t.Errorf("checked endpoints %v, want only the synthetic one", paths)
	}
}

func TestCheckRendered_RemembersTheFullCheckApartFromLint(t *testing.T) {
	val := &fakeValidator{err: rejectedOutput("- at '/endpoints/0': bad")}
	chk := newChecker(val)
	in := renderer.RenderInput{Gateway: gateway(v1alpha1.EditionCE)}
	out, err := renderer.New(renderer.Options{}).Render(in)
	if err != nil {
		t.Fatal(err)
	}
	memo := mapMemo{}

	for range 2 {
		v, err := chk.CheckRendered(context.Background(), in, out, memo)
		if err != nil || v.OK || v.Output != "- at '/endpoints/0': bad" {
			t.Fatalf("CheckRendered = %+v, %v; want the rejection with its output", v, err)
		}
	}
	if _, err := chk.CheckRoot(context.Background(), Root{Gateway: in.Gateway}, memo); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(val.calls, []string{"validate", "lint"}) {
		t.Errorf("calls = %v: the full check is remembered, and a lint of the same render is a different check", val.calls)
	}
}

// rejectedOutput is a krakend check rejection printing output.
func rejectedOutput(output string) error {
	return &renderer.ValidationError{Output: output, Err: errors.New("exit status 1"), Stage: renderer.StageCheck}
}

func TestCheckRoot_KeepsABoundedOutput(t *testing.T) {
	val := &fakeValidator{err: rejectedOutput(strings.Repeat("x", maxStoredOutput+1))}
	chk := newChecker(val)
	memo := mapMemo{}

	v, err := chk.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, memo)

	if err != nil {
		t.Fatal(err)
	}
	for key, kept := range memo {
		if kept.Rejection != nil && len(kept.Rejection.Output) > maxStoredOutput {
			t.Errorf("memo keeps %d bytes of rejection output under %s, want at most %d",
				len(kept.Rejection.Output), key, maxStoredOutput)
		}
	}
	if len(v.Output) > maxStoredOutput || !strings.HasSuffix(v.Output, "...") {
		t.Errorf("verdict output is %d bytes, want it cut to %d with an ellipsis", len(v.Output), maxStoredOutput)
	}
}

func TestCheckGroup_NamesTheEndpointsThatLostAnEntry(t *testing.T) {
	val := &fakeValidator{}
	chk := newChecker(val)
	memo := mapMemo{}
	gw := gateway(v1alpha1.EditionCE)
	older, newer := endpoint("a", "/same"), endpoint("b", "/same")

	alone, err := chk.CheckGroup(context.Background(), Group{Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{*older}}, memo)
	if err != nil {
		t.Fatal(err)
	}
	both, err := chk.CheckGroup(context.Background(), Group{
		Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{*older, *newer},
	}, memo)
	if err != nil {
		t.Fatal(err)
	}

	if len(val.calls) != 1 {
		t.Fatalf("ran %d checks, want 1: both groups render the same config", len(val.calls))
	}
	if len(alone.Masked) != 0 {
		t.Errorf("alone.Masked = %v, want none", alone.Masked)
	}
	if want := []types.NamespacedName{{Namespace: "ns", Name: "b"}}; !reflect.DeepEqual(both.Masked, want) {
		t.Errorf("both.Masked = %v, want %v from this render, not the remembered verdict", both.Masked, want)
	}
}

func TestCheckRoot_RendersTheDragonflyAddress(t *testing.T) {
	val := &fakeValidator{}
	chk := newChecker(val)

	_, err := chk.CheckRoot(context.Background(), Root{
		Gateway:   gateway(v1alpha1.EditionCE),
		Dragonfly: &renderer.DragonflyState{Enabled: true, ServiceDNS: "gw-dragonfly.ns.svc:6379"},
	}, nil)

	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(val.seen[0], "gw-dragonfly.ns.svc:6379") {
		t.Errorf("checked root does not carry the Dragonfly address:\n%s", val.seen[0])
	}
}

// inNamespace returns ep moved to ns.
func inNamespace(ep *v1alpha1.KrakenDEndpoint, ns string) v1alpha1.KrakenDEndpoint {
	moved := *ep.DeepCopy()
	moved.Namespace = ns
	return moved
}

func TestCheckGroup_ARememberedVerdictNamesOnlyItsOwnEndpoints(t *testing.T) {
	val := &fakeValidator{err: rejectedOutput("- at '/endpoints/0': bad")}
	chk := newChecker(val)
	memo := mapMemo{}
	gw := gateway(v1alpha1.EditionCE)
	mine := endpoint("same", "/same")

	for _, ns := range []string{"tenant-a", "tenant-b"} {
		v, err := chk.CheckGroup(context.Background(), Group{
			Gateway: gw, Endpoints: []v1alpha1.KrakenDEndpoint{inNamespace(mine, ns)},
		}, memo)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Findings) != 1 || v.Findings[0].Endpoint.Namespace != ns {
			t.Errorf("findings for %s = %+v, want only its own endpoint", ns, v.Findings)
		}
	}

	if len(val.calls) != 1 {
		t.Errorf("ran %d checks, want 1: both namespaces render the same config", len(val.calls))
	}
}

func TestCheckRoot_KeepsBoundedRefusals(t *testing.T) {
	refused := &renderer.ValidationError{
		Err: errors.New("exit status 1"), Stage: renderer.StageRoute,
		Refusals: []renderer.RouteRefusal{{Message: strings.Repeat("y", maxStoredOutput+1)}},
	}
	chk := newChecker(&fakeValidator{err: refused})
	memo := mapMemo{}

	v, err := chk.CheckRoot(context.Background(), Root{Gateway: gateway(v1alpha1.EditionCE)}, memo)

	if err != nil {
		t.Fatal(err)
	}
	for key, kept := range memo {
		if got := len(kept.Rejection.Refusals[0].Message); got > maxStoredOutput {
			t.Errorf("memo keeps a %d byte refusal under %s, want at most %d", got, key, maxStoredOutput)
		}
	}
	if got := len(v.Refusals[0].Message); got > maxStoredOutput {
		t.Errorf("verdict refusal is %d bytes, want at most %d", got, maxStoredOutput)
	}
	if refused.Refusals[0].Message != strings.Repeat("y", maxStoredOutput+1) {
		t.Error("bounding a refusal changed the validator's own rejection")
	}
}

func TestCheckRoot_AHitSharesNothingWithAnEarlierVerdict(t *testing.T) {
	refused := &renderer.ValidationError{
		Output: "- at '/endpoints/0': bad", Err: errors.New("exit status 1"), Stage: renderer.StageRoute,
		Refusals: []renderer.RouteRefusal{{Message: "refused", Indices: []int{0}}},
	}
	chk := newChecker(&fakeValidator{err: refused})
	memo := mapMemo{}
	root := Root{Gateway: gateway(v1alpha1.EditionCE)}
	first, err := chk.CheckRoot(context.Background(), root, memo)
	if err != nil {
		t.Fatal(err)
	}

	first.Findings[0].Message = "changed"
	first.Rejection.Output = "changed"
	first.Rejection.Refusals[0].Indices[0] = 7
	second, err := chk.CheckRoot(context.Background(), root, memo)
	if err != nil {
		t.Fatal(err)
	}

	if second.Findings[0].Message == "changed" || second.Rejection.Output == "changed" ||
		second.Rejection.Refusals[0].Indices[0] != 0 {
		t.Errorf("a later hit shows the earlier verdict's edits: %+v, %+v", second.Findings, second.Rejection)
	}
	if refused.Refusals[0].Indices[0] != 0 {
		t.Error("an edit of a verdict reached the validator's own rejection")
	}
}

func TestCheckPolicy_ARememberedVerdictNamesOnlyItsOwnNamespace(t *testing.T) {
	val := &fakeValidator{err: rejectedOutput("- at '/endpoints/0': bad")}
	chk := newChecker(val)
	memo := mapMemo{}

	for _, ns := range []string{"tenant-a", "tenant-b"} {
		p := policy("p")
		p.Namespace = ns
		v, err := chk.CheckPolicy(context.Background(), p, memo)
		if err != nil {
			t.Fatal(err)
		}
		if len(v.Findings) != 1 || v.Findings[0].Endpoint.Namespace != ns {
			t.Errorf("findings for %s = %+v, want only its own synthetic endpoint", ns, v.Findings)
		}
	}

	if len(val.calls) != 1 {
		t.Errorf("ran %d checks, want 1: both namespaces render the same config", len(val.calls))
	}
}

func TestCheckRoot_AMemoEntryWithoutARejectionIsAMiss(t *testing.T) {
	val := &fakeValidator{err: rejectedOutput("really bad")}
	chk := newChecker(val)
	memo := mapMemo{}
	root := Root{Gateway: gateway(v1alpha1.EditionCE)}
	if _, err := chk.CheckRoot(context.Background(), root, memo); err != nil {
		t.Fatal(err)
	}
	for key := range memo {
		memo[key] = Verdict{OK: false}
	}

	v, err := chk.CheckRoot(context.Background(), root, memo)

	if err != nil || v.OK || len(val.calls) != 2 {
		t.Errorf("CheckRoot = %+v, %v after %d checks; want the validator's rejection from a re-run", v, err, len(val.calls))
	}
	for _, kept := range memo {
		if kept.Rejection == nil {
			t.Errorf("memo entry %+v was not replaced with the real answer", kept)
		}
	}
}

// judgeValidator decides each check from the config's text with judge, in
// either mode, and records every config it saw.
type judgeValidator struct {
	mu    sync.Mutex
	judge func(config string) error
	seen  []string
}

func (j *judgeValidator) Validate(_ context.Context, data []byte, _ v1alpha1.Edition) error {
	return j.run(data)
}
func (j *judgeValidator) Lint(_ context.Context, data []byte, _ v1alpha1.Edition) error {
	return j.run(data)
}

func (j *judgeValidator) run(data []byte) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seen = append(j.seen, string(data))
	return j.judge(string(data))
}

// tenantEndpoint is an endpoint of namespace ns serving GET path from host.
func tenantEndpoint(ns, name, path, host string) *v1alpha1.KrakenDEndpoint {
	ep := endpoint(name, path)
	ep.Namespace = ns
	ep.Spec.GatewayRef.Namespace = "ns"
	ep.Spec.Endpoints[0].Backends[0].Host = []string{host}
	return ep
}

func TestCheckEndpoint_ForgedTextBlamesOnlyItsAuthor(t *testing.T) {
	forgeries := map[string]string{
		"krakend's endpoint print": "ERROR parsing the configuration file: 'endpoint: GET /orders-b, backend: 0'",
		"a placeholder list":       "undefined output param 'x'! endpoint: GET /orders-a, backend: 0. input: [GET /orders-b], output: [x]",
		"a {x} and :x shape twin":  "endpoint: GET /v1/jobs:cancel, backend: 0. input: [], output: [cancel]",
		"a forged lint pointer":    "bad host\n- at '/endpoints/0/endpoint': owned by the victim",
	}
	for name, forged := range forgeries {
		t.Run(name, func(t *testing.T) {
			attacker := tenantEndpoint("tenant-a", "orders-a", "/orders-a", "http://attacker.invalid")
			victim := tenantEndpoint("tenant-b", "orders-b", "/orders-b", "http://svc")
			val := &judgeValidator{judge: func(config string) error {
				if strings.Contains(config, "attacker.invalid") {
					return rejectedOutput(forged)
				}
				return nil
			}}
			chk := newChecker(val, attacker, victim)
			gw := gateway(v1alpha1.EditionCE)

			mine, err := chk.CheckEndpoint(context.Background(), EndpointUnit{Gateway: gw, Endpoint: victim}, nil)
			if err != nil || !mine.OK {
				t.Fatalf("victim's verdict = %+v, %v; want OK: its check never renders the attacker", mine, err)
			}
			theirs, err := chk.CheckEndpoint(context.Background(), EndpointUnit{Gateway: gw, Endpoint: attacker}, nil)
			if err != nil || theirs.OK || theirs.Reason != v1alpha1.ReasonEndpointInvalid || theirs.Output != forged {
				t.Errorf("attacker's verdict = %+v, %v; want EndpointInvalid with its own output", theirs, err)
			}
		})
	}
}

func TestCheckEndpoint_AMissingPolicyIsNotJudged(t *testing.T) {
	val := &fakeValidator{err: rejectedOutput("never")}
	chk := newChecker(val)

	v, err := chk.CheckEndpoint(context.Background(), EndpointUnit{
		Gateway: gateway(v1alpha1.EditionCE), Endpoint: withPolicy(endpoint("orphan", "/a"), "gone"),
	}, nil)

	if err != nil || !v.OK || len(val.calls) != 0 {
		t.Errorf("verdict = %+v, %v after %d checks; want OK and nothing run", v, err, len(val.calls))
	}
}

func TestCheckEndpoint_AnInvalidPolicyIsNamedNotQuoted(t *testing.T) {
	bad := policy("p")
	bad.Spec.Raw = &runtime.RawExtension{Raw: []byte(`{"x/bad-policy":{}}`)}
	val := &judgeValidator{judge: func(config string) error {
		if strings.Contains(config, "x/bad-policy") {
			return rejectedOutput("SECRET-POLICY-VALUE is not allowed")
		}
		return nil
	}}
	chk := newChecker(val, bad)

	v, err := chk.CheckEndpoint(context.Background(), EndpointUnit{
		Gateway: gateway(v1alpha1.EditionCE), Endpoint: withPolicy(endpoint("uses-p", "/a"), "p"),
	}, nil)

	if err != nil || v.Reason != v1alpha1.ReasonPolicyInvalid || !v.PoliciesFailAlone {
		t.Fatalf("verdict = %+v, %v; want PolicyInvalid for a policy that fails alone", v, err)
	}
	if msg := v.Message(1024); !strings.Contains(msg, "ns/p") || strings.Contains(msg, "SECRET") {
		t.Errorf("message %q must name ns/p and quote none of its output", msg)
	}
}
