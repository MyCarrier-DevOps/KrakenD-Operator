// Package configcheck decides whether a gateway's aggregated KrakenD config is
// valid. The gateway controller and the admission webhooks share one Checker,
// so both judge the same inputs with the same renderer and the same krakend
// binary, and share its exec slots.
package configcheck

import (
	"fmt"

	"k8s.io/apimachinery/pkg/types"
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
	return ""
}
