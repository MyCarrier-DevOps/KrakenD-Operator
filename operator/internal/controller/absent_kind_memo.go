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
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// absentKindWindow is how long a negative discovery answer for an optional
// kind is trusted by the delete path.
const absentKindWindow = time.Minute

// absentKindMemo remembers, per optional kind, that discovery said its CRD
// was not installed. While the CRD is absent no child of that kind can exist,
// so the delete path of a disabled feature skips the lookup until the answer
// expires. Only the delete path consults it: the create path asks discovery
// every time, and any positive answer forgets the kind. The memo holds at
// most one entry per optional kind.
type absentKindMemo struct {
	mu    sync.Mutex
	until map[schema.GroupVersionKind]time.Time
}

// absent reports whether gvk was found absent and the answer still holds at now.
func (m *absentKindMemo) absent(gvk schema.GroupVersionKind, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return now.Before(m.until[gvk])
}

// remember records that gvk is absent until the window after now ends.
func (m *absentKindMemo) remember(gvk schema.GroupVersionKind, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.until == nil {
		m.until = make(map[schema.GroupVersionKind]time.Time)
	}
	m.until[gvk] = now.Add(absentKindWindow)
}

// forget drops what is remembered about gvk.
func (m *absentKindMemo) forget(gvk schema.GroupVersionKind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.until, gvk)
}
