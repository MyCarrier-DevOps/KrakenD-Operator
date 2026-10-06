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

// Package configcheck decides whether a gateway's aggregated KrakenD config is
// valid. The gateway controller and the admission webhooks share one Checker,
// so both judge the same inputs with the same renderer and the same krakend
// binary, and share its exec slots.
package configcheck

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// Finding is one reason a config fails validation. Endpoint and Index name
// the KrakenDEndpoint and the position in its spec.endpoints the failure
// belongs to. A failure of the gateway root, or one that names no endpoint,
// has an empty Endpoint and Index -1.
type Finding struct {
	Endpoint types.NamespacedName
	Index    int
	Message  string
}

// String renders f for status messages and admission responses.
func (f Finding) String() string {
	if f.Endpoint.Name == "" {
		return "gateway: " + f.Message
	}
	if f.Index < 0 {
		return fmt.Sprintf("%s: %s", f.Endpoint, f.Message)
	}
	return fmt.Sprintf("%s spec.endpoints[%d]: %s", f.Endpoint, f.Index, f.Message)
}

// Verdict is the outcome of a check. OK is false when the config is invalid;
// Findings then says why.
type Verdict struct {
	OK       bool
	Findings []Finding
	// Rejection is the validator's rejection behind a verdict that is not OK.
	// A caller that remembers it can rebuild the findings with Rejected
	// against whatever the endpoints are by then.
	Rejection *renderer.ValidationError
	// Stage is the check that rejected the config.
	Stage renderer.RejectionStage
	// Refusals are the route check's refused registrations, when Stage is
	// renderer.StageRoute, and RefusalsCapped is set when the check stopped
	// at renderer.MaxRouteRefusals, so refusals past those listed are unknown.
	Refusals       []Refusal
	RefusalsCapped bool
}

// Refusal is one registration the route check refused: the KrakenDEndpoints
// it names (the refused entry's, then the accepted entry's it clashes with, if
// any; none for a refusal of the gateway's own route) and its lint lines.
type Refusal struct {
	Endpoints []types.NamespacedName
	Message   string
}

// Rejected returns the verdict for rejection of out, the render of in. The
// findings name the entries of in's endpoints as they are now.
func Rejected(rejection *renderer.ValidationError, in renderer.RenderInput, out *renderer.RenderOutput) Verdict {
	atts := renderer.Attribute(out.JSON, out.Sources, rejection.Output)
	return Verdict{Findings: findingsFrom(atts, out.JSON, in.Endpoints, rejection.Output), Rejection: rejection}
}

// Summary joins the findings into one message, cut at a finding boundary and
// ending with the number of findings left out. The findings take at most limit
// bytes; the count suffix follows them. When the first finding alone exceeds
// limit, a prefix of it is kept, cut on a rune boundary, so the message always
// carries a reason.
func (v Verdict) Summary(limit int) string {
	var b strings.Builder
	for i, f := range v.Findings {
		s := f.String()
		if i > 0 {
			s = "; " + s
		}
		if b.Len()+len(s) > limit {
			left := len(v.Findings) - i
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

// findingsFrom converts the renderer's attributions, which name positions in the
// rendered endpoints array, into findings that name the entry's position in
// its KrakenDEndpoint's spec.endpoints. A rejection always has a finding:
// when nothing was attributed, the output, one line per non-empty line joined
// with "; ", is one gateway finding.
func findingsFrom(atts []renderer.Attribution, renderedJSON []byte,
	endpoints []v1alpha1.KrakenDEndpoint, output string) []Finding {
	if len(atts) == 0 {
		var lines []string
		for _, l := range strings.Split(output, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		msg := strings.Join(lines, "; ")
		if msg == "" {
			msg = "rejected with no output"
		}
		return []Finding{{Index: -1, Message: msg}}
	}
	var doc struct {
		Endpoints []struct {
			Endpoint string `json:"endpoint"`
			Method   string `json:"method"`
		} `json:"endpoints"`
	}
	if json.Unmarshal(renderedJSON, &doc) != nil {
		doc.Endpoints = nil
	}
	specs := make(map[types.NamespacedName]*v1alpha1.KrakenDEndpoint, len(endpoints))
	for i := range endpoints {
		specs[types.NamespacedName{Namespace: endpoints[i].Namespace, Name: endpoints[i].Name}] = &endpoints[i]
	}
	out := make([]Finding, 0, len(atts))
	for _, a := range atts {
		f := Finding{Endpoint: a.Endpoint, Index: -1, Message: a.Message}
		if ep := specs[a.Endpoint]; ep != nil && a.Index >= 0 && a.Index < len(doc.Endpoints) {
			rendered := doc.Endpoints[a.Index]
			for i, e := range ep.Spec.Endpoints {
				if e.Method == rendered.Method && e.Endpoint == rendered.Endpoint {
					f.Index = i
					break
				}
			}
		}
		out = append(out, f)
	}
	return out
}
