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

package webhook

import (
	"k8s.io/utils/lru"

	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

// admissionMemoSize is how many verdicts the admission webhooks remember, the
// most recent first, shared by every request the pod admits. A verdict
// depends only on its content key, so a remembered one is as good as a run.
// Each verdict keeps at most 16 KiB of output (configcheck caps it before it
// is stored), so the memo stays within a few MiB.
const admissionMemoSize = 256

// lruMemo is the admission webhooks' configcheck.Memo: the most recent
// verdicts, safe for concurrent requests.
type lruMemo struct {
	cache *lru.Cache
}

func newAdmissionMemo() *lruMemo {
	return &lruMemo{cache: lru.New(admissionMemoSize)}
}

// Lookup returns the remembered verdict for key.
func (m *lruMemo) Lookup(key string) (configcheck.Verdict, bool) {
	v, ok := m.cache.Get(key)
	if !ok {
		return configcheck.Verdict{}, false
	}
	verdict, ok := v.(configcheck.Verdict)
	return verdict, ok
}

// Store remembers v for key, forgetting the least recent verdict past the
// memo's size.
func (m *lruMemo) Store(key string, v configcheck.Verdict) {
	m.cache.Add(key, v)
}
