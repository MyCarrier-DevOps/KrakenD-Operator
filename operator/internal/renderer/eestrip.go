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
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
)

// The extra_config namespaces KrakenD 2.13 documents as Enterprise-only, by
// the level they appear at. Source: the "Only applies to KrakenD Enterprise"
// box on each page under https://www.krakend.io/docs/enterprise/ (read
// 2026-09-28), cross-checked with "Enterprise only." in the v2.13 schema
// (https://www.krakend.io/schema/v2.13/krakend.json). The schema alone cannot
// separate them: the CE and EE 2.13 binaries embed the same schema, so CE
// lint accepts every namespace in eeonly_namespaces.json and the CE runtime
// silently ignores it.
//
// Not listed, because CE honors part of them: telemetry/opentelemetry at
// endpoint and backend level (its proxy section is CE), and endpoint-level
// security/cors (undocumented). backend/http/client is listed although CE
// honors its send_body_on_redirect: its EE settings (client_tls,
// proxy_address, no_redirect) change where and how a backend is reached, and
// dropping them must be visible.
//
//go:embed eeonly_namespaces.json
var eeOnlyNamespacesFile []byte

// eeOnlyData is eeonly_namespaces.json: per NamespaceLevel, the namespaces
// only KrakenD Enterprise implements, and those of them every CE render drops
// without listing them. hack/audit-admission-rules.sh reads the same file, so
// the audit, admission and both CE renders judge one list.
type eeOnlyData struct {
	EnterpriseOnly map[NamespaceLevel][]string `json:"enterpriseOnly"`
	CERenderDrops  map[NamespaceLevel][]string `json:"ceRenderDrops"`
}

var eeOnlyLists = func() eeOnlyData {
	var lists eeOnlyData
	if err := json.Unmarshal(eeOnlyNamespacesFile, &lists); err != nil {
		panic("renderer: eeonly_namespaces.json: " + err.Error())
	}
	return lists
}()

var (
	eeOnlyServiceNamespaces  = namespaceSet(eeOnlyLists.EnterpriseOnly[LevelService]...)
	eeOnlyEndpointNamespaces = namespaceSet(eeOnlyLists.EnterpriseOnly[LevelEndpoint]...)
	eeOnlyBackendNamespaces  = namespaceSet(eeOnlyLists.EnterpriseOnly[LevelBackend]...)
	// ceDroppedEndpointNamespaces are the Enterprise-only entry namespaces
	// every CE render (CE edition or CE fallback) drops without listing them:
	// documentation/openapi, which AutoConfig generates on every endpoint and
	// only KrakenD Enterprise publishes. Dropping it changes nothing the
	// gateway serves, so EEOnlyNamespaces leaves it out, admission does not
	// reject it, and a CE fallback does not report it per endpoint.
	ceDroppedEndpointNamespaces = namespaceSet(eeOnlyLists.CERenderDrops[LevelEndpoint]...)
)

func namespaceSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// dropCEInertNamespaces removes, in place, the namespaces every CE render (CE
// edition or CE fallback) drops from a rendered entry.
func dropCEInertNamespaces(ep map[string]any) {
	ec, ok := ep["extra_config"].(map[string]any)
	if !ok {
		return
	}
	for ns := range ceDroppedEndpointNamespaces {
		delete(ec, ns)
	}
	if len(ec) == 0 {
		delete(ep, "extra_config")
	}
}

// stripNamespaces deletes, in place, ec's keys that are in eeOnly, and returns
// one feature per deleted key, labelled "<where> <key>" and sorted.
func stripNamespaces(
	ec map[string]any, eeOnly map[string]struct{}, base StrippedEEFeature, where string,
) []StrippedEEFeature {
	var out []StrippedEEFeature
	for k := range ec {
		if _, ok := eeOnly[k]; !ok {
			continue
		}
		delete(ec, k)
		f := base
		f.Feature = where + " " + k
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Feature < out[j].Feature })
	return out
}

// stripEndpointEEFeatures removes, in place, the Enterprise-only namespaces of
// a rendered endpoint entry and of its backends.
func stripEndpointEEFeatures(ep map[string]any, fe flatEndpoint) []StrippedEEFeature {
	base := StrippedEEFeature{Source: fe.Source, Method: fe.Entry.Method, Endpoint: fe.Entry.Endpoint}
	var out []StrippedEEFeature
	if ec, ok := ep["extra_config"].(map[string]any); ok {
		out = append(out, stripNamespaces(ec, eeOnlyEndpointNamespaces, base, "extra_config")...)
		if len(ec) == 0 {
			delete(ep, "extra_config")
		}
	}
	backends, ok := ep["backend"].([]any)
	if !ok {
		return out
	}
	for i, b := range backends {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		ec, ok := bm["extra_config"].(map[string]any)
		if !ok {
			continue
		}
		where := fmt.Sprintf("backend[%d] extra_config", i)
		out = append(out, stripNamespaces(ec, eeOnlyBackendNamespaces, base, where)...)
		if len(ec) == 0 {
			delete(bm, "extra_config")
		}
	}
	return out
}

// NamespaceLevel is where in a KrakenD config an extra_config namespace appears.
type NamespaceLevel string

// The levels EEOnlyNamespaces knows.
const (
	LevelService  NamespaceLevel = "service"
	LevelEndpoint NamespaceLevel = "endpoint"
	LevelBackend  NamespaceLevel = "backend"
)

// EEOnlyNamespaces returns the extra_config namespaces that only KrakenD
// Enterprise implements at level, sorted, or nil for an unknown level. A
// CE-fallback render strips them; admission rejects them on CE gateways,
// where KrakenD CE would accept and then silently ignore them. At
// LevelEndpoint it leaves out the namespaces every CE render drops
// (ceDroppedEndpointNamespaces).
func EEOnlyNamespaces(level NamespaceLevel) []string {
	set, ok := map[NamespaceLevel]map[string]struct{}{
		LevelService:  eeOnlyServiceNamespaces,
		LevelEndpoint: eeOnlyEndpointNamespaces,
		LevelBackend:  eeOnlyBackendNamespaces,
	}[level]
	if !ok {
		return nil
	}
	names := slices.Sorted(maps.Keys(set))
	if level == LevelEndpoint {
		names = slices.DeleteFunc(names, func(n string) bool {
			_, dropped := ceDroppedEndpointNamespaces[n]
			return dropped
		})
	}
	return names
}

// CEDrop is an Enterprise-only use in an extra_config that a CE render drops.
// Keys is empty when the whole namespace goes, and lists the dropped keys when
// CE honors the rest of the block.
type CEDrop struct {
	Namespace string
	Keys      []string
}

// CEDrops returns what a CE render drops from the extra_config ec at level, in
// namespace order.
func CEDrops(level NamespaceLevel, ec map[string]json.RawMessage) []CEDrop {
	var drops []CEDrop
	for _, ns := range EEOnlyNamespaces(level) {
		if _, ok := ec[ns]; ok {
			drops = append(drops, CEDrop{Namespace: ns})
		}
	}
	return drops
}
