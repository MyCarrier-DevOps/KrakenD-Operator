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

// Package renderer transforms CRD state into a deterministic krakend.json byte slice.
package renderer

import (
	"context"
	"fmt"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Renderer builds the krakend.json configuration from CRD state.
type Renderer interface {
	Render(input RenderInput) (*RenderOutput, error)
}

// RenderInput holds all inputs needed to render the KrakenD configuration.
type RenderInput struct {
	Gateway *v1alpha1.KrakenDGateway
	// Endpoints' order decides which definition of a component schema wins
	// (first seen); callers pass it sorted by namespace/name.
	Endpoints        []v1alpha1.KrakenDEndpoint
	Policies         map[string]*v1alpha1.KrakenDBackendPolicy
	CEFallback       bool
	Dragonfly        *DragonflyState
	PluginConfigMaps []corev1.ConfigMap
}

// DragonflyState holds the runtime state of the Dragonfly cache.
type DragonflyState struct {
	Enabled    bool
	ServiceDNS string
}

// FeatureWildcardEndpoint is the StrippedEEFeature.Feature of an EE wildcard
// entry, which a CE-fallback render removes whole.
const FeatureWildcardEndpoint = "wildcard endpoint"

// StrippedEEFeature is one Enterprise-only feature removed from a CE-fallback
// render.
type StrippedEEFeature struct {
	// Source is the KrakenDEndpoint the feature came from; zero for a
	// gateway-level feature.
	Source types.NamespacedName
	// Method and Endpoint name the entry; both are empty for a gateway-level
	// feature.
	Method   string
	Endpoint string
	// Feature is "wildcard endpoint", "extra_config <namespace>" or
	// "backend[<i>] extra_config <namespace>".
	Feature string
}

// String renders f for status messages.
func (f StrippedEEFeature) String() string {
	if f.Source == (types.NamespacedName{}) {
		return "gateway: " + f.Feature
	}
	return fmt.Sprintf("%s %s %s: %s", f.Source, f.Method, f.Endpoint, f.Feature)
}

// RenderOutput holds the results of a rendering pass.
type RenderOutput struct {
	JSON                []byte
	Checksum            string
	PluginChecksum      string
	ConflictedEndpoints []types.NamespacedName
	InvalidEndpoints    []types.NamespacedName
	// EntryConflicts maps each KrakenDEndpoint that lost at least one entry
	// to the entries it lost, sorted by endpoint, then method, then winner. An
	// entry that clashes with several older entries is listed once for each
	// KrakenDEndpoint they belong to. Its keys are ConflictedEndpoints.
	EntryConflicts map[types.NamespacedName][]EntryConflict
	// StrippedEEFeatures lists what a CE-fallback render (RenderInput.CEFallback)
	// removed because only KrakenD Enterprise supports it: EE wildcard entries
	// and EE-only extra_config namespaces, or only the EE-only keys of a block
	// CE partly honors (backend/http/client keeps send_body_on_redirect; a
	// block with nothing to drop stays and is not listed). It lists rendered
	// entries in order, then the gateway level. The entries' docs-only namespaces
	// (documentation/openapi) are dropped without being listed.
	StrippedEEFeatures []StrippedEEFeature
	// SchemaConflicts lists the component schemas an endpoint defines
	// differently from the endpoint the rendered documentation takes that
	// schema name from (first seen, in endpoint order). It is empty unless the
	// render publishes docs.
	SchemaConflicts []SchemaConflict
	// RouteResolutionCapped says the render stopped resolving router clashes
	// between endpoints at MaxRouteRefusals: entries after that point are
	// rendered whether or not they clash, and EntryConflicts misses those
	// clashes.
	RouteResolutionCapped bool
}

// SchemaConflict is a component schema Endpoint defines under a name whose
// rendered definition comes from Winner, with a different body. Endpoint's
// documentation then shows Winner's schema.
type SchemaConflict struct {
	Endpoint types.NamespacedName
	Schema   string
	Winner   types.NamespacedName
}

// Options configures the renderer (reserved for future use).
type Options struct{}

// Validator validates a rendered krakend.json configuration.
type Validator interface {
	// Validate checks jsonData the way KrakenD of the given edition would load
	// it, using the embedded CE binary, including krakend's router test. An
	// invalid config returns *ValidationError; any other error means the
	// validator could not run.
	Validate(ctx context.Context, jsonData []byte, edition v1alpha1.Edition) error
	// Lint is Validate without krakend's router test.
	Lint(ctx context.Context, jsonData []byte, edition v1alpha1.Edition) error
}

// CommandExecutor executes external commands.
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
}

// RejectionStage names the check that rejected a config.
type RejectionStage int

const (
	// StageUnknown is a rejection that does not say which check made it.
	StageUnknown RejectionStage = iota
	// StageEEWildcard is the EE router's wildcard rule, applied in Go.
	StageEEWildcard
	// StageRoute is the route check, run in an in-process gin engine.
	StageRoute
	// StageCheck is krakend check itself.
	StageCheck
)

// RouteRefusal is one registration the route check refused: its lint lines
// joined with newlines.
type RouteRefusal struct {
	Message string
}

// ValidationError wraps a failed krakend check output. Stage says which check
// rejected the config.
type ValidationError struct {
	Output string
	Err    error
	Stage  RejectionStage
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("krakend config validation failed: %s: %s", e.Err, e.Output)
}

func (e *ValidationError) Unwrap() error {
	return e.Err
}

// EntryConflict is one entry of a KrakenDEndpoint that the render left out
// because an older KrakenDEndpoint, or an earlier entry of the same one, has
// an entry with the same method and route shape (paths that differ only in
// parameter names are the same route), or an entry KrakenD's router cannot
// serve next to it. An entry that clashes with the entries of several older
// KrakenDEndpoints is recorded once for each of them, every older clashing
// entry, served or left out; each record has its own Detail.
type EntryConflict struct {
	Endpoint string
	Method   string
	// Winner is the KrakenDEndpoint whose older entry this one shares a route
	// shape with or clashes with in the router. That entry may itself be left
	// out of the render. It is the losing KrakenDEndpoint itself when an
	// earlier entry of its own is the one.
	Winner types.NamespacedName
	// Detail is the router's refusal when the two entries do not share a
	// route shape but KrakenD cannot serve both: gin refuses to register one
	// next to the other, or one lies under the other's EE wildcard. It names
	// methods and paths only. It is empty for entries of the same route shape.
	Detail string
}
