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

// Package hash provides deterministic hashing utilities for the KrakenD operator.
package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// SHA256Hex returns the hex-encoded SHA-256 hash of the given data.
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// CombineHex deterministically and unambiguously combines two or more
// already-hex-encoded digests into a single hex-encoded SHA-256 digest.
// Each input is length-prefixed (as a decimal length + ":" separator,
// netstring-style framing — decimal ASCII length, ":", payload, NUL) before
// hashing, so two distinct digest pairs can
// never collide into the same byte stream regardless of the length of
// either input — unlike a bare byte concatenation (e.g.
// append([]byte(a), b...)), which is only unambiguous under an unstated
// fixed-length precondition on its inputs. Safe to use even if callers'
// digest formats or lengths change in the future.
func CombineHex(digests ...string) string {
	h := sha256.New()
	for _, d := range digests {
		// hash.Hash.Write is documented to never return an error; the
		// check exists only to satisfy errcheck and to fail loudly instead
		// of silently truncating the digest input if that contract were
		// ever violated.
		if _, err := fmt.Fprintf(h, "%d:%s\x00", len(d), d); err != nil {
			panic(fmt.Sprintf("writing to sha256 hash (should never fail): %v", err))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
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
