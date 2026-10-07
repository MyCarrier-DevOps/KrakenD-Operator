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

// Package configcheck judges KrakenD configs: a gateway's root, each of its
// endpoints and policies on its own, a group of them, and the whole render the
// gateway controller applies. The gateway controller, the AutoConfig
// controller and the admission webhooks share one Checker, so all judge the
// same inputs with the same renderer and the same krakend binary, and share
// its exec slots.
package configcheck

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/types"

	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// Verdict is the outcome of a check. OK is false when the config is invalid;
// Output then says why, in the words of the check that rejected it.
type Verdict struct {
	OK bool
	// Output is the rejection's text when OK is false: what krakend check,
	// the route check or the EE wildcard rule printed for the config that was
	// checked. Only the owner of that config may be shown it.
	Output string
	// Masked are the endpoints that lost an entry in the checked render
	// (MaskedEndpoints). They come from this check's render, never from a
	// memo: the same content can be rendered from inputs that mask different
	// endpoints.
	Masked []types.NamespacedName
	// Stage is the check that rejected the config.
	Stage renderer.RejectionStage
}

// Suspect reports whether the check of a group of endpoints, this verdict, did
// not judge the endpoint name of that group: every one when the check failed,
// otherwise those that lost an entry in its render (Masked), whose lost
// entries it never checked. The gateway controller, the AutoConfig controller
// and admission judge exactly these endpoints on their own, so each blames
// the same ones.
func (v Verdict) Suspect(name types.NamespacedName) bool {
	return !v.OK || slices.Contains(v.Masked, name)
}

// Excerpt is the rejection's output on one line: its lines that carry a
// finding, joined with "; " and cut at a line boundary (joinBounded). An
// empty rejection reads "rejected with no output", so an excerpt always
// carries a reason.
func (v Verdict) Excerpt(limit int) string {
	var lines []string
	for _, l := range strings.Split(v.Output, "\n") {
		if l = strings.TrimSpace(l); !noFinding(l) {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 && !v.OK {
		lines = []string{"rejected with no output"}
	}
	return joinBounded(lines, limit)
}

// noFinding reports the lines krakend check prints around its findings.
func noFinding(line string) bool {
	return line == "" || line == "Syntax OK!" ||
		strings.HasPrefix(line, "Parsing configuration file") ||
		strings.HasPrefix(line, "ERROR linting the configuration file")
}

// joinBounded joins parts with "; ", cut at a part boundary so the parts take
// at most limit bytes, followed by " (+N more)" for the parts left out, or
// " (truncated)" when only a cut first part is kept. When the first part
// alone exceeds limit, a prefix of it is kept, cut on a rune boundary.
func joinBounded(parts []string, limit int) string {
	var b strings.Builder
	for i, s := range parts {
		if i > 0 {
			s = "; " + s
		}
		if b.Len()+len(s) > limit {
			left := len(parts) - i
			if i == 0 {
				b.WriteString(Truncate(s, limit))
				left--
			}
			if left == 0 {
				b.WriteString(" (truncated)")
			} else {
				fmt.Fprintf(&b, " (+%d more)", left)
			}
			break
		}
		b.WriteString(s)
	}
	return b.String()
}

// Truncate returns the longest prefix of s that is at most limit bytes and
// ends on a rune boundary, so a cut never leaves half a character.
func Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// TruncateEllipsis returns s when it fits in limit bytes. Otherwise it cuts s
// on a rune boundary and ends it with "...", so the result is at most limit
// bytes; a limit under 3 leaves no room for the marker, so it only cuts.
func TruncateEllipsis(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit < len("...") {
		return Truncate(s, limit)
	}
	return Truncate(s, limit-len("...")) + "..."
}
