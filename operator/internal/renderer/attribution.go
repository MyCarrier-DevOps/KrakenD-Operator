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

package renderer

import (
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// Attribution ties one krakend check finding to the rendered endpoint entry
// it names. Index is the entry's position in the rendered "endpoints" array,
// or -1 when the finding names no endpoint (root settings, plugins); Endpoint
// is then the zero value. Endpoint is also zero when Index is past the
// Sources it was attributed against.
type Attribution struct {
	Endpoint types.NamespacedName
	Index    int
	Message  string
}

var lintPointerRe = regexp.MustCompile(`^- at '/endpoints/(\d+)[/']`)

// Attribute maps krakend check output back to the KrakenDEndpoints that
// produced the entries it names. renderedJSON is the rendered config, whose
// endpoints array is index-aligned with sources (RenderOutput.Sources). Each
// attributable output line yields one Attribution per entry it names; a line
// that names none yields one with Index -1.
func Attribute(_ []byte, sources []types.NamespacedName, checkOutput string) []Attribution {
	var out []Attribution
	for _, raw := range strings.Split(checkOutput, "\n") {
		line := strings.TrimSpace(raw)
		if skipCheckLine(line) {
			continue
		}
		indices := matchLine(line)
		if len(indices) == 0 {
			out = append(out, Attribution{Index: -1, Message: line})
			continue
		}
		for _, i := range indices {
			a := Attribution{Index: i, Message: line}
			if i < len(sources) {
				a.Endpoint = sources[i]
			}
			out = append(out, a)
		}
	}
	return out
}

// skipCheckLine reports lines that carry no finding.
func skipCheckLine(line string) bool {
	return line == "" || line == "Syntax OK!" ||
		strings.HasPrefix(line, "Parsing configuration file") ||
		strings.HasPrefix(line, "ERROR linting the configuration file")
}

// matchLine returns the indices of the rendered entries line names: a lint
// pointer's index.
func matchLine(line string) []int {
	if m := lintPointerRe.FindStringSubmatch(line); m != nil {
		i, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		return []int{i}
	}
	return nil
}
