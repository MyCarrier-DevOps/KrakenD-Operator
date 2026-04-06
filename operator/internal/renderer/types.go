/*
Copyright 2026.

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
	Gateway          *v1alpha1.KrakenDGateway
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

// RenderOutput holds the results of a rendering pass.
type RenderOutput struct {
	JSON                []byte
	Checksum            string
	DesiredImage        string
	PluginChecksum      string
	ConflictedEndpoints []types.NamespacedName
	InvalidEndpoints    []types.NamespacedName
}

// Options configures the renderer (reserved for future use).
type Options struct{}

// Validator validates a rendered krakend.json configuration.
type Validator interface {
	Validate(ctx context.Context, jsonData []byte) error
	PrepareValidationCopy(jsonData []byte, eeWithoutFallback bool) ([]byte, error)
}

// CommandExecutor executes external commands.
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ValidationError wraps a failed krakend check output.
type ValidationError struct {
	Output string
	Err    error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("krakend config validation failed: %s: %s", e.Err, e.Output)
}

func (e *ValidationError) Unwrap() error {
	return e.Err
}
