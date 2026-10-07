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
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
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
