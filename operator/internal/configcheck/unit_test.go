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
	"reflect"
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
