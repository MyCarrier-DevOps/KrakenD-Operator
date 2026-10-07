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

import "k8s.io/utils/lru"

// LRUMemo is a Memo that keeps the most recent verdicts, safe for concurrent
// use. A verdict depends only on its content key, so a remembered one is as
// good as a run. Each verdict keeps at most maxStoredOutput bytes of output,
// so a memo of a few hundred stays within a few MiB.
type LRUMemo struct {
	cache *lru.Cache
}

// NewLRUMemo returns a memo that remembers size verdicts.
func NewLRUMemo(size int) *LRUMemo {
	return &LRUMemo{cache: lru.New(size)}
}

// Lookup returns the remembered verdict for key.
func (m *LRUMemo) Lookup(key string) (Verdict, bool) {
	v, ok := m.cache.Get(key)
	if !ok {
		return Verdict{}, false
	}
	verdict, ok := v.(Verdict)
	return verdict, ok
}

// Store remembers v for key, forgetting the least recent verdict past the
// memo's size.
func (m *LRUMemo) Store(key string, v Verdict) {
	m.cache.Add(key, v)
}
