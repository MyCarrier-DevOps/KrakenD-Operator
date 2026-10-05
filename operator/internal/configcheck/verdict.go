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
}

// Summary joins the findings into one message of at most limit bytes, cut at
// a finding boundary and ending with the number of findings left out.
func (v Verdict) Summary(limit int) string {
	var b strings.Builder
	for i, f := range v.Findings {
		s := f.String()
		if i > 0 {
			s = "; " + s
		}
		if b.Len()+len(s) > limit {
			fmt.Fprintf(&b, " (+%d more)", len(v.Findings)-i)
			break
		}
		b.WriteString(s)
	}
	return b.String()
}

// findingsFrom converts the renderer's attributions, which name positions in the
// rendered endpoints array, into findings that name the entry's position in
// its KrakenDEndpoint's spec.endpoints. A rejection always has a finding:
// when nothing was attributed, the trimmed output is one gateway finding.
//
//nolint:unused // the Checker is the only caller and lands separately
func findingsFrom(atts []renderer.Attribution, renderedJSON []byte,
	endpoints []v1alpha1.KrakenDEndpoint, output string) []Finding {
	if len(atts) == 0 {
		return []Finding{{Index: -1, Message: strings.TrimSpace(output)}}
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
