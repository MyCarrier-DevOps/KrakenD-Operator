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

package controller

import (
	"maps"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

// verdictMemo remembers, per owner (a gateway or an AutoConfig), the verdicts
// of the config checks the owner's last pass ran, keyed by content
// (configcheck.Memo). A pass that ends normally keeps only what it used, so
// the memo never holds more than one pass's checks. A pass that fails keeps
// what it used on top of what the last pass kept, so the verdicts judged
// before the failure are not run again. It lives in memory on purpose: after
// a restart each check runs once more, possibly with a newer validator.
type verdictMemo struct {
	mu     sync.Mutex
	owners map[types.NamespacedName]map[string]configcheck.Verdict
}

// passMemo is the configcheck.Memo of one pass. It answers from what the
// owner's last pass kept and records what this pass uses. One pass runs on
// one goroutine: an owner is reconciled by one worker at a time.
type passMemo struct {
	kept map[string]configcheck.Verdict
	used map[string]configcheck.Verdict
}

// Lookup answers from this pass, then from what the last pass kept.
func (p *passMemo) Lookup(key string) (configcheck.Verdict, bool) {
	if v, ok := p.used[key]; ok {
		return v, true
	}
	v, ok := p.kept[key]
	if ok {
		p.used[key] = v
	}
	return v, ok
}

// Store records a verdict this pass judged.
func (p *passMemo) Store(key string, v configcheck.Verdict) {
	p.used[key] = v
}

// begin starts a pass for owner.
func (m *verdictMemo) begin(owner types.NamespacedName) *passMemo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &passMemo{kept: m.owners[owner], used: map[string]configcheck.Verdict{}}
}

// end keeps what p used for owner's next pass, and, when the pass failed,
// what the last pass kept too.
func (m *verdictMemo) end(owner types.NamespacedName, p *passMemo, failed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owners == nil {
		m.owners = map[types.NamespacedName]map[string]configcheck.Verdict{}
	}
	keep := p.used
	if failed {
		keep = maps.Clone(p.kept)
		if keep == nil {
			keep = map[string]configcheck.Verdict{}
		}
		maps.Copy(keep, p.used)
	}
	m.owners[owner] = keep
}

// forget drops what the memo keeps for owner.
func (m *verdictMemo) forget(owner types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.owners, owner)
}

// countedPass is a gateway pass's memo. Every verdict stored in it was just
// judged, so each rejection among them is counted once in
// config_validation_failures_total.
type countedPass struct{ *passMemo }

// Store counts a fresh rejection and records the verdict.
func (p countedPass) Store(key string, v configcheck.Verdict) {
	if !v.OK {
		configValidationFailures.Inc()
	}
	p.passMemo.Store(key, v)
}
