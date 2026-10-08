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
)

// maxVerifiedPerGateway bounds what verifiedConfigMaps keeps for one gateway:
// the applied config, the render being published and the revision history.
const maxVerifiedPerGateway = 8

// configMapVersion identifies one stored version of a ConfigMap. A copy of
// the same name that is deleted and created again has another UID, and a copy
// that is edited has another resource version.
type configMapVersion struct {
	uid             types.UID
	resourceVersion string
}

// verifiedConfigMaps remembers, per gateway, the version of each config
// ConfigMap whose payload was hashed against its checksum. Hashing reads the
// whole rendered config, so a pass that finds the same version again does not
// repeat it. The memo is in memory: after a restart each ConfigMap is
// verified once more.
type verifiedConfigMaps struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]map[string]configMapVersion
}

// has reports whether version of the ConfigMap name was verified.
func (m *verifiedConfigMaps) has(gw types.NamespacedName, name string, version configMapVersion) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.entries[gw][name]
	return ok && v == version
}

// remember records that version of the ConfigMap name was verified.
func (m *verifiedConfigMaps) remember(gw types.NamespacedName, name string, version configMapVersion) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[types.NamespacedName]map[string]configMapVersion)
	}
	if len(m.entries[gw]) >= maxVerifiedPerGateway {
		delete(m.entries, gw)
	}
	if m.entries[gw] == nil {
		m.entries[gw] = make(map[string]configMapVersion)
	}
	m.entries[gw][name] = version
}

// forget drops what is remembered about the ConfigMap name.
func (m *verifiedConfigMaps) forget(gw types.NamespacedName, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries[gw], name)
}

// forgetGateway drops everything remembered about gw's ConfigMaps.
func (m *verifiedConfigMaps) forgetGateway(gw types.NamespacedName) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, gw)
}
