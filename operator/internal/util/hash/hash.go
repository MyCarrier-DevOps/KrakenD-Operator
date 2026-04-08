/*
Copyright 2026.

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

// Package hash provides deterministic hashing utilities for the KrakenD operator.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// SHA256Hex returns the hex-encoded SHA-256 hash of the given data.
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// PluginChecksum computes a deterministic checksum from plugin ConfigMap
// binary data and OCI image tags. ConfigMaps are sorted by name, keys within
// each ConfigMap are sorted alphabetically, and OCI tags are sorted before
// hashing to guarantee deterministic output.
func PluginChecksum(configMaps []corev1.ConfigMap, ociTags []string) string {
	h := sha256.New()

	sort.Slice(configMaps, func(i, j int) bool {
		return configMaps[i].Name < configMaps[j].Name
	})
	for _, cm := range configMaps {
		keys := make([]string, 0, len(cm.BinaryData))
		for k := range cm.BinaryData {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write(cm.BinaryData[k])
		}
	}

	sort.Strings(ociTags)
	for _, tag := range ociTags {
		h.Write([]byte(tag))
	}

	return hex.EncodeToString(h.Sum(nil))
}
