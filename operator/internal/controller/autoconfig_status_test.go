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
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate_BoundsBytesAndMarksTheCut(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short) = %q, want it unchanged", got)
	}
	// 100 three-byte runes: 300 bytes, over the cap by byte count and well
	// under it by rune count.
	got := truncate(strings.Repeat("€", 100), maxStatusMessageLen)
	if len(got) > maxStatusMessageLen {
		t.Errorf("len = %d bytes, want at most %d", len(got), maxStatusMessageLen)
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate = %q, want valid UTF-8 ending in \"...\"", got)
	}
}
