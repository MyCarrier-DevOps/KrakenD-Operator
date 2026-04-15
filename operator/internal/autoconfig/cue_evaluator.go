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
	"sort"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
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
	DefaultHost  string
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

	if input.DefaultHost != "" {
		hostCUE := fmt.Sprintf("_defaultHost: %q", input.DefaultHost)
		hostFill := cueCtx.CompileString(hostCUE, cue.Filename("host-inject.cue"))
		unified = unified.Unify(hostFill)
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
	output, err := exportEndpointEntries(endpointsValue)
	if err != nil {
		return nil, err
	}

	applyDefaults(output, input.Defaults)

	if input.URLTransform != nil {
		applyURLTransform(output, input.URLTransform)
	}

	applyFieldOverrides(output, input.Overrides)

	return output, nil
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

// applyDefaults applies CR-level EndpointDefaults to all entries. These replace
// the CUE-generated defaults (e.g. _defaultTimeout) and give the user
// control over baseline values without custom CUE definitions.
func applyDefaults(output *CUEOutput, defaults *v1alpha1.EndpointDefaults) {
	if defaults == nil {
		return
	}
	for i := range output.Entries {
		entry := &output.Entries[i]
		if defaults.Timeout != nil {
			entry.Timeout = defaults.Timeout
		}
		if defaults.CacheTTL != nil {
			entry.CacheTTL = defaults.CacheTTL
		}
		if defaults.OutputEncoding != "" {
			entry.OutputEncoding = defaults.OutputEncoding
		}
		if defaults.ConcurrentCalls != nil {
			entry.ConcurrentCalls = defaults.ConcurrentCalls
		}
		if defaults.InputHeaders != nil {
			entry.InputHeaders = defaults.InputHeaders
		}
		if defaults.InputQueryStrings != nil {
			entry.InputQueryStrings = defaults.InputQueryStrings
		}
		if defaults.PolicyRef != nil {
			for j := range entry.Backends {
				entry.Backends[j].PolicyRef = defaults.PolicyRef
			}
		}
		if defaults.ExtraConfig != nil {
			entry.ExtraConfig = mergeExtraConfig(entry.ExtraConfig, defaults.ExtraConfig)
		}
	}
}

// applyURLTransform applies host mapping, path stripping, and path prefixing
// to the evaluated endpoint entries. This runs as a post-processing step after
// CUE evaluation, allowing the CR's URLTransformSpec to override hosts and
// transform paths without requiring custom CUE definitions.
func applyURLTransform(output *CUEOutput, transform *v1alpha1.URLTransformSpec) {
	hostMap := make(map[string]string, len(transform.HostMapping))
	for _, m := range transform.HostMapping {
		hostMap[m.From] = m.To
	}

	for i := range output.Entries {
		entry := &output.Entries[i]

		// Host mapping: replace matching backend hosts
		for j := range entry.Backends {
			for k, host := range entry.Backends[j].Host {
				if to, ok := hostMap[host]; ok {
					entry.Backends[j].Host[k] = to
				}
			}
		}

		oldKey := entry.Endpoint + ":" + entry.Method

		// Strip path prefix
		if transform.StripPathPrefix != "" {
			entry.Endpoint = strings.TrimPrefix(entry.Endpoint, transform.StripPathPrefix)
			if entry.Endpoint == "" {
				entry.Endpoint = "/"
			}
		}

		// Add path prefix
		if transform.AddPathPrefix != "" {
			entry.Endpoint = transform.AddPathPrefix + entry.Endpoint
		}

		// Update keys in OperationIDs and Tags maps if endpoint changed
		newKey := entry.Endpoint + ":" + entry.Method
		if newKey != oldKey {
			if opID, ok := output.OperationIDs[oldKey]; ok {
				delete(output.OperationIDs, oldKey)
				output.OperationIDs[newKey] = opID
			}
			if tags, ok := output.Tags[oldKey]; ok {
				delete(output.Tags, oldKey)
				output.Tags[newKey] = tags
			}
		}
	}
}

// applyFieldOverrides applies per-operation override fields (Timeout,
// CacheTTL, OutputEncoding, ConcurrentCalls, InputHeaders, InputQueryStrings,
// PolicyRef, Endpoint, Method, ExtraConfig, Backends) to the evaluated
// endpoint entries. ExtraConfig is merged here via mergeExtraConfig;
// applyOverrides separately injects override data into the CUE tree for
// custom CUE definitions that reference _overrides.
func applyFieldOverrides(output *CUEOutput, overrides []v1alpha1.OperationOverride) {
	if len(overrides) == 0 {
		return
	}

	// Build operationID → entry index lookup
	opIDIndex := make(map[string]int, len(output.Entries))
	for _, key := range sortedKeys(output.OperationIDs) {
		opID := output.OperationIDs[key]
		for i := range output.Entries {
			entryKey := output.Entries[i].Endpoint + ":" + output.Entries[i].Method
			if entryKey == key {
				opIDIndex[opID] = i
				break
			}
		}
	}

	for _, ov := range overrides {
		idx, ok := opIDIndex[ov.OperationID]
		if !ok {
			continue
		}
		entry := &output.Entries[idx]
		oldKey := entry.Endpoint + ":" + entry.Method

		if ov.Timeout != nil {
			entry.Timeout = ov.Timeout
		}
		if ov.CacheTTL != nil {
			entry.CacheTTL = ov.CacheTTL
		}
		if ov.OutputEncoding != "" {
			entry.OutputEncoding = ov.OutputEncoding
		}
		if ov.ConcurrentCalls != nil {
			entry.ConcurrentCalls = ov.ConcurrentCalls
		}
		if ov.InputHeaders != nil {
			entry.InputHeaders = ov.InputHeaders
		}
		if ov.InputQueryStrings != nil {
			entry.InputQueryStrings = ov.InputQueryStrings
		}
		if ov.Endpoint != "" {
			entry.Endpoint = ov.Endpoint
		}
		if ov.Method != "" {
			entry.Method = ov.Method
		}
		if ov.ExtraConfig != nil {
			entry.ExtraConfig = mergeExtraConfig(entry.ExtraConfig, ov.ExtraConfig)
		}
		if ov.PolicyRef != nil {
			for i := range entry.Backends {
				entry.Backends[i].PolicyRef = ov.PolicyRef
			}
		}
		for _, bo := range ov.Backends {
			if bo.Index >= 0 && bo.Index < len(entry.Backends) && bo.ExtraConfig != nil {
				entry.Backends[bo.Index].ExtraConfig = bo.ExtraConfig
			}
		}

		// Update OperationIDs and Tags maps if endpoint/method changed
		newKey := entry.Endpoint + ":" + entry.Method
		if newKey != oldKey {
			if opID, ok := output.OperationIDs[oldKey]; ok {
				delete(output.OperationIDs, oldKey)
				output.OperationIDs[newKey] = opID
			}
			if tags, ok := output.Tags[oldKey]; ok {
				delete(output.Tags, oldKey)
				output.Tags[newKey] = tags
			}
		}
	}
}

// mergeExtraConfig performs a deep merge of override keys into existing
// ExtraConfig. When both base and override contain JSON objects for the same
// key, the objects are merged recursively so that only the specified sub-fields
// are overwritten. Keys not present in the override are preserved at every
// level. If unmarshalling fails, the override replaces entirely.
func mergeExtraConfig(existing, override *runtime.RawExtension) *runtime.RawExtension {
	if existing == nil || len(existing.Raw) == 0 {
		return override
	}
	if override == nil || len(override.Raw) == 0 {
		return existing
	}

	var base map[string]json.RawMessage
	if err := json.Unmarshal(existing.Raw, &base); err != nil {
		return override
	}

	var patch map[string]json.RawMessage
	if err := json.Unmarshal(override.Raw, &patch); err != nil {
		return override
	}

	for k, v := range patch {
		if orig, ok := base[k]; ok {
			base[k] = deepMergeJSON(orig, v)
		} else {
			base[k] = v
		}
	}

	merged, err := json.Marshal(base)
	if err != nil {
		return override
	}
	return &runtime.RawExtension{Raw: merged}
}

// deepMergeJSON recursively merges two JSON values. If both are objects, their
// keys are merged recursively. Otherwise the patch value wins.
func deepMergeJSON(base, patch json.RawMessage) json.RawMessage {
	var baseMap map[string]json.RawMessage
	var patchMap map[string]json.RawMessage

	if err := json.Unmarshal(base, &baseMap); err != nil {
		return patch
	}
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return patch
	}

	for k, v := range patchMap {
		if orig, ok := baseMap[k]; ok {
			baseMap[k] = deepMergeJSON(orig, v)
		} else {
			baseMap[k] = v
		}
	}

	merged, err := json.Marshal(baseMap)
	if err != nil {
		return patch
	}
	return merged
}

// sortedKeys returns the keys of a map in stable order for deterministic processing.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
