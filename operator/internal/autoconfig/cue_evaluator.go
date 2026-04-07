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

package autoconfig

import (
	"context"
	"encoding/json"
	"fmt"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// CUEInput holds data for CUE evaluation.
type CUEInput struct {
	SpecData     []byte
	SpecFormat   v1alpha1.SpecFormat
	DefaultDefs  map[string]string
	CustomDefs   map[string]string
	Defaults     *v1alpha1.EndpointDefaults
	Overrides    []v1alpha1.OperationOverride
	URLTransform *v1alpha1.URLTransformSpec
	Environment  string
	ServiceName  string
}

// CUEOutput holds the result of CUE evaluation.
type CUEOutput struct {
	Entries      []v1alpha1.EndpointEntry
	OperationIDs map[string]string
	Tags         map[string][]string
	Warnings     []string
}

// CUEEvaluator evaluates CUE definitions against OpenAPI spec data.
type CUEEvaluator interface {
	Evaluate(ctx context.Context, input CUEInput) (*CUEOutput, error)
}

// NewCUEEvaluator returns a CUEEvaluator implementation.
func NewCUEEvaluator() CUEEvaluator {
	return &cueEvaluator{}
}

type cueEvaluator struct{}

func (e *cueEvaluator) Evaluate(_ context.Context, input CUEInput) (*CUEOutput, error) {
	cueCtx := cuecontext.New()

	specJSON, err := normalizeToJSON(input.SpecData, input.SpecFormat)
	if err != nil {
		return nil, fmt.Errorf("normalizing spec to JSON: %w", err)
	}

	unified := loadDefinitions(cueCtx, input.DefaultDefs)
	if !unified.Exists() {
		return nil, fmt.Errorf("no CUE definitions loaded")
	}

	// Inject spec data using Unify (FillPath rejects hidden labels like _spec).
	specCUE := fmt.Sprintf("%s: _\n%s: %s", input.ServiceName, input.ServiceName, specJSON)
	specFill := cueCtx.CompileString(specCUE, cue.Filename("spec-inject.cue"))
	unified = unified.Unify(specFill)

	if input.Environment != "" {
		envCUE := fmt.Sprintf("_env: %q", input.Environment)
		envFill := cueCtx.CompileString(envCUE, cue.Filename("env-inject.cue"))
		unified = unified.Unify(envFill)
	}

	if len(input.CustomDefs) > 0 {
		customValue := loadDefinitions(cueCtx, input.CustomDefs)
		unified = unified.Unify(customValue)
	}

	unified = applyOverrides(cueCtx, unified, input)

	if err := unified.Validate(cue.Concrete(true)); err != nil {
		return nil, fmt.Errorf("CUE evaluation failed: %w", err)
	}

	endpointsValue := unified.LookupPath(cue.ParsePath("endpoint"))
	return exportEndpointEntries(endpointsValue)
}

func loadDefinitions(cueCtx *cue.Context, defs map[string]string) cue.Value {
	var unified cue.Value
	for filename, content := range defs {
		val := cueCtx.CompileString(content, cue.Filename(filename))
		if !unified.Exists() {
			unified = val
		} else {
			unified = unified.Unify(val)
		}
	}
	return unified
}

func normalizeToJSON(data []byte, format v1alpha1.SpecFormat) ([]byte, error) {
	switch format {
	case v1alpha1.SpecFormatJSON:
		return data, nil
	case v1alpha1.SpecFormatYAML:
		return yaml.YAMLToJSON(data)
	default:
		var js json.RawMessage
		if json.Unmarshal(data, &js) == nil {
			return data, nil
		}
		return yaml.YAMLToJSON(data)
	}
}

func applyOverrides(cueCtx *cue.Context, unified cue.Value, input CUEInput) cue.Value {
	for _, override := range input.Overrides {
		if override.ExtraConfig != nil && override.ExtraConfig.Raw != nil {
			key := sanitizeName(override.OperationID)
			overrideCUE := fmt.Sprintf("_overrides: %s: _\n_overrides: %s: %s", key, key, override.ExtraConfig.Raw)
			val := cueCtx.CompileString(overrideCUE, cue.Filename("override-"+key+".cue"))
			unified = unified.Unify(val)
		}
	}
	return unified
}

func exportEndpointEntries(endpointsValue cue.Value) (*CUEOutput, error) {
	output := &CUEOutput{
		OperationIDs: make(map[string]string),
		Tags:         make(map[string][]string),
	}

	iter, err := endpointsValue.Fields(cue.Optional(true))
	if err != nil {
		return nil, fmt.Errorf("iterating endpoint fields: %w", err)
	}

	for iter.Next() {
		key := iter.Selector().String()
		val := iter.Value()

		var entry v1alpha1.EndpointEntry
		jsonBytes, err := val.MarshalJSON()
		if err != nil {
			output.Warnings = append(output.Warnings, fmt.Sprintf("skipping %s: %v", key, err))
			continue
		}
		if err := json.Unmarshal(jsonBytes, &entry); err != nil {
			output.Warnings = append(output.Warnings, fmt.Sprintf("skipping %s: %v", key, err))
			continue
		}

		output.Entries = append(output.Entries, entry)

		entryKey := entry.Endpoint + ":" + entry.Method
		opIDValue := val.LookupPath(cue.MakePath(cue.Hid("_operationId", "_")))
		if opIDValue.Exists() {
			if opID, err := opIDValue.String(); err == nil {
				output.OperationIDs[entryKey] = opID
			}
		}

		tagsValue := val.LookupPath(cue.MakePath(cue.Hid("_tags", "_")))
		if tagsValue.Exists() {
			tagsIter, err := tagsValue.List()
			if err == nil {
				var tags []string
				for tagsIter.Next() {
					if t, err := tagsIter.Value().String(); err == nil {
						tags = append(tags, t)
					}
				}
				output.Tags[entryKey] = tags
			}
		}
	}

	return output, nil
}
