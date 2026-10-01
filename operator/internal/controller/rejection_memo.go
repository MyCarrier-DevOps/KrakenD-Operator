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
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// rejectionMemo remembers, per gateway, the validation input krakend check
// last rejected and the rejection it produced. Validation is deterministic
// for identical input, so a reconcile that renders the same bytes again
// reuses the verdict instead of running krakend check again. The memo is in
// memory on purpose: after an operator restart, possibly with a newer
// validator, each rejected gateway is validated once more.
type rejectionMemo struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]rejectedInput
}

type rejectedInput struct {
	checksum string
	err      *renderer.ValidationError
}

// lookup returns the remembered rejection for key when it was issued for
// the same validation input, and nil otherwise.
func (m *rejectionMemo) lookup(key types.NamespacedName, checksum string) *renderer.ValidationError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[key]; ok && e.checksum == checksum {
		return e.err
	}
	return nil
}

func (m *rejectionMemo) remember(key types.NamespacedName, checksum string, err *renderer.ValidationError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[types.NamespacedName]rejectedInput)
	}
	m.entries[key] = rejectedInput{checksum: checksum, err: err}
}

func (m *rejectionMemo) forget(key types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
}
