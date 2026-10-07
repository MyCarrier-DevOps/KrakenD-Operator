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
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate_CutsOnARuneBoundary(t *testing.T) {
	tests := []struct {
		name, in string
		limit    int
		want     string
	}{
		{"short is unchanged", "abc", 3, "abc"},
		{"ascii cut", "abcdef", 4, "abcd"},
		{"never splits a rune", "aéé", 2, "a"},
		{"keeps a whole rune that fits", "aéé", 3, "aé"},
		{"zero limit", "abc", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Truncate(tt.in, tt.limit); got != tt.want || !utf8.ValidString(got) {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}

func TestTruncateEllipsis_NeverExceedsTheLimit(t *testing.T) {
	tests := []struct {
		name, in string
		limit    int
		want     string
	}{
		{"fits is unchanged", "abcdef", 6, "abcdef"},
		{"cut is marked", "abcdefg", 6, "abc..."},
		{"never splits a rune", "aéééé", 7, "aé..."},
		{"limit under the marker", "abcdef", 2, "ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateEllipsis(tt.in, tt.limit)
			if got != tt.want || len(got) > tt.limit || !utf8.ValidString(got) {
				t.Errorf("TruncateEllipsis(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}

func TestVerdictExcerpt_KeepsTheFindingLinesWithinTheLimit(t *testing.T) {
	v := Verdict{Output: "Parsing configuration file: krakend.json\n" +
		"ERROR linting the configuration file:\n" +
		"- at '/endpoints/0/backend/0/host/0': first\n\n" +
		"- at '/endpoints/0/timeout': second\n"}

	if got := v.Excerpt(1024); got != "- at '/endpoints/0/backend/0/host/0': first; - at '/endpoints/0/timeout': second" {
		t.Errorf("Excerpt = %q, want the finding lines only", got)
	}
	if got := v.Excerpt(45); got != "- at '/endpoints/0/backend/0/host/0': first (+1 more)" {
		t.Errorf("Excerpt(45) = %q, want the first line and a count", got)
	}
	if got := (Verdict{}).Excerpt(64); got != "rejected with no output" {
		t.Errorf("an empty rejection's excerpt = %q", got)
	}
}

func TestVerdictExcerpt_MarksASingleCutLineTruncated(t *testing.T) {
	v := Verdict{Output: strings.Repeat("x", 100)}

	got := v.Excerpt(20)

	if !strings.HasSuffix(got, " (truncated)") || strings.Contains(got, "+0") {
		t.Errorf("excerpt = %q, want it to end with \" (truncated)\"", got)
	}
	if want := strings.Repeat("x", 20) + " (truncated)"; got != want {
		t.Errorf("excerpt = %q, want the cut line kept: %q", got, want)
	}
}

func TestVerdictExcerpt_KeepsAValidPrefixOfAnOversizedFirstLine(t *testing.T) {
	v := Verdict{Output: "- at '/a': " + strings.Repeat("é", 50) + "\n- at '/b': second"}

	got := v.Excerpt(30)

	if !utf8.ValidString(got) || !strings.HasSuffix(got, " (+1 more)") || !strings.HasPrefix(got, "- at '/a': é") {
		t.Errorf("excerpt = %q, want a rune-safe prefix of the first line and \" (+1 more)\"", got)
	}
}
