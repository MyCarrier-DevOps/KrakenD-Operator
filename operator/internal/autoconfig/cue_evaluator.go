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

package autoconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueerrors "cuelang.org/go/cue/errors"
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
	Defaults     *v1alpha1.Defaults
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
	// UnmatchedOverrides holds, in override order, an operationId from
	// spec.overrides that no operation has, or "<operationId> backends[<i>]"
	// for a backend index out of range.
	UnmatchedOverrides []string
	// AmbiguousOverrides holds the operationIds from spec.overrides that
	// more than one operation declares, in override order.
	AmbiguousOverrides []string
	// Skipped holds the operations whose method the KrakenDEndpoint API does
	// not accept (reason UnsupportedMethod), sorted by path then method, with
	// the path and method their entry has after the URL transform and the
	// overrides. They have no entry in Entries.
	Skipped []OperationIssue
	// Failed holds the operations whose entries failed CUE validation or
	// could not be decoded (reason CUEEvaluationFailed), sorted by path then
	// method. They have no entry in Entries.
	Failed []OperationIssue
}

// CUEEvaluator evaluates CUE definitions against OpenAPI spec data.
type CUEEvaluator interface {
	Evaluate(ctx context.Context, input CUEInput) (*CUEOutput, error)
}

// supportedMethods are the HTTP methods the KrakenDEndpoint API accepts. An
// entry with any other method (the default definitions also emit HEAD,
// OPTIONS and TRACE operations) is skipped, not generated.
var supportedMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

// skippedMethods are the standard HTTP methods the KrakenDEndpoint API
// rejects; the default definitions emit all but CONNECT. A failed operation
// is skipped, not held, only when its method is known to be one of these.
var skippedMethods = []string{"HEAD", "OPTIONS", "TRACE", "CONNECT"}

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

	// Errors inside one endpoint entry fail only that operation (see
	// exportEndpointEntries); any other error fails the whole evaluation.
	rootErrors := entryErrors(unified.Validate(cue.Concrete(true)))

	endpointsValue := unified.LookupPath(cue.ParsePath("endpoint"))
	output, err := exportEndpointEntries(endpointsValue, rootErrors)
	if err != nil {
		return nil, err
	}

	var endpointDefaults *v1alpha1.EndpointDefaults
	var backendDefaults *v1alpha1.BackendDefaults
	var defaultPolicyRef *v1alpha1.PolicyRef
	if input.Defaults != nil {
		endpointDefaults = input.Defaults.Endpoint
		backendDefaults = input.Defaults.Backend
		defaultPolicyRef = input.Defaults.PolicyRef
	}

	applyDefaults(output, endpointDefaults)
	applyBackendDefaults(output, backendDefaults)
	applyDefaultPolicyRef(output, defaultPolicyRef)

	if input.URLTransform != nil {
		applyURLTransform(output, input.URLTransform)
		transformIssuePaths(output.Failed, input.URLTransform)
	}

	applyFieldOverrides(output, input.Overrides)
	skipUnsupportedMethods(output)
	// Sorted once the paths and methods are final: a prefix strip or an
	// override can reorder them.
	sortIssues(output.Failed)
	sortIssues(output.Skipped)

	return output, nil
}

// loadDefinitions unifies the definition files in filename order: CUE words
// a conflict by the order of its operands, so map order would change the
// text of an error from one evaluation to the next.
func loadDefinitions(cueCtx *cue.Context, defs map[string]string) cue.Value {
	var unified cue.Value
	for _, filename := range slices.Sorted(maps.Keys(defs)) {
		val := cueCtx.CompileString(defs[filename], cue.Filename(filename))
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
			key := SanitizeName(override.OperationID)
			overrideCUE := fmt.Sprintf("_overrides: %q: _\n_overrides: %q: %s", key, key, override.ExtraConfig.Raw)
			val := cueCtx.CompileString(overrideCUE, cue.Filename("override-"+key+".cue"))
			unified = unified.Unify(val)
		}
	}
	return unified
}

// exportEndpointEntries decodes every entry of the endpoint struct, whatever
// its method: skipUnsupportedMethods partitions them once overrides applied.
// An entry that fails concrete validation or does not decode into an
// EndpointEntry is recorded in Failed and does not stop the other entries.
func exportEndpointEntries(endpointsValue cue.Value, rootErrors map[string][]string) (*CUEOutput, error) {
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
		rootMsgs := rootErrors[key]
		delete(rootErrors, key)
		val := iter.Value()
		op, methodKnown := entryOperation(iter.Selector().Unquoted(), val)
		entry, err := decodeEntry(val)
		// The root validation reports errors the entry alone does not, such
		// as an unresolved reference: the entry's own error comes first.
		if err == nil && len(rootMsgs) > 0 {
			err = fmt.Errorf("%s", strings.Join(sortedUnique(rootMsgs), "; "))
		}
		if err != nil {
			output.Failed = append(output.Failed, OperationIssue{
				Operation: op, Reason: v1alpha1.ReasonCUEEvaluationFailed, Message: err.Error(),
				methodKnown: methodKnown,
			})
			continue
		}
		output.Entries = append(output.Entries, entry)
		entryKey := entry.Endpoint + ":" + entry.Method
		if op.OperationID != "" {
			output.OperationIDs[entryKey] = op.OperationID
		}
		if len(op.Tags) > 0 {
			output.Tags[entryKey] = op.Tags
		}
	}
	if len(rootErrors) > 0 {
		return nil, fmt.Errorf("CUE evaluation failed: %s", strings.Join(sortedMessages(rootErrors), "; "))
	}
	return output, nil
}

// entryErrors groups the errors of err by the endpoint entry they lie in, at a
// path endpoint.<key>.<...>, keyed by the entry's selector. An error anywhere
// else is grouped under "", which no entry has.
func entryErrors(err error) map[string][]string {
	byEntry := map[string][]string{}
	for _, e := range cueerrors.Errors(err) {
		key := ""
		if p := e.Path(); len(p) >= 2 && p[0] == "endpoint" {
			key = p[1]
		}
		byEntry[key] = append(byEntry[key], e.Error())
	}
	return byEntry
}

// sortedMessages returns every message of byEntry, sorted and distinct.
func sortedMessages(byEntry map[string][]string) []string {
	var all []string
	for _, msgs := range byEntry {
		all = append(all, msgs...)
	}
	return sortedUnique(all)
}

// sortedUnique returns the distinct messages in sorted order.
func sortedUnique(msgs []string) []string {
	sorted := slices.Clone(msgs)
	slices.Sort(sorted)
	return slices.Compact(sorted)
}

// decodeEntry validates one endpoint entry as concrete and decodes it.
func decodeEntry(val cue.Value) (v1alpha1.EndpointEntry, error) {
	var entry v1alpha1.EndpointEntry
	if err := val.Validate(cue.Concrete(true)); err != nil {
		return entry, err
	}
	jsonBytes, err := val.MarshalJSON()
	if err != nil {
		return entry, err
	}
	if err := json.Unmarshal(jsonBytes, &entry); err != nil {
		return entry, err
	}
	return entry, nil
}

// entryOperation identifies the entry labelled key by its own endpoint and
// method fields, falling back to the label ("<path>:<METHOD>", the label the
// default definitions use) when a field is not concrete, and by its hidden
// _operationId and _tags, which the default definitions copy from the spec.
// It also reports whether the method came from a concrete method field.
func entryOperation(key string, val cue.Value) (Operation, bool) {
	op := Operation{Path: key}
	methodKnown := false
	if i := strings.LastIndex(key, ":"); i >= 0 {
		op.Path, op.Method = key[:i], key[i+1:]
	}
	if path, err := val.LookupPath(cue.ParsePath("endpoint")).String(); err == nil {
		op.Path = path
	}
	if method, err := val.LookupPath(cue.ParsePath("method")).String(); err == nil {
		op.Method, methodKnown = method, true
	}
	if opID, err := val.LookupPath(cue.MakePath(cue.Hid("_operationId", "_"))).String(); err == nil {
		op.OperationID = opID
	}
	tagsIter, err := val.LookupPath(cue.MakePath(cue.Hid("_tags", "_"))).List()
	if err != nil {
		return op, methodKnown
	}
	for tagsIter.Next() {
		if t, err := tagsIter.Value().String(); err == nil {
			op.Tags = append(op.Tags, t)
		}
	}
	return op, methodKnown
}

// sortIssues orders issues by path, then method.
func sortIssues(issues []OperationIssue) {
	slices.SortFunc(issues, func(a, b OperationIssue) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Method, b.Method)
	})
}

// skipUnsupportedMethods moves every entry, and every failed operation, whose
// final method the KrakenDEndpoint API does not accept to Skipped, so an
// override that changes the method decides whether the operation is
// generated. A failed operation of such a method can never publish, so it
// must not hold anything back.
func skipUnsupportedMethods(output *CUEOutput) {
	kept := output.Entries[:0]
	for _, entry := range output.Entries {
		if slices.Contains(supportedMethods, entry.Method) {
			kept = append(kept, entry)
			continue
		}
		key := entry.Endpoint + ":" + entry.Method
		output.Skipped = append(output.Skipped, unsupportedMethodIssue(Operation{
			Method: entry.Method, Path: entry.Endpoint,
			OperationID: output.OperationIDs[key], Tags: output.Tags[key],
		}))
		delete(output.OperationIDs, key)
		delete(output.Tags, key)
	}
	output.Entries = kept

	stillFailed := output.Failed[:0]
	for _, failed := range output.Failed {
		if !failed.methodKnown || !slices.Contains(skippedMethods, failed.Method) {
			stillFailed = append(stillFailed, failed)
			continue
		}
		output.Skipped = append(output.Skipped, unsupportedMethodIssue(failed.Operation))
	}
	output.Failed = stillFailed
}

// unsupportedMethodIssue reports op as skipped for its method.
func unsupportedMethodIssue(op Operation) OperationIssue {
	return OperationIssue{
		Operation: op,
		Reason:    v1alpha1.ReasonUnsupportedMethod,
		Message:   "KrakenDEndpoint supports only " + strings.Join(supportedMethods, ", "),
	}
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
			entry.InputHeaders = slices.Clone(defaults.InputHeaders)
		}
		if defaults.InputQueryStrings != nil {
			entry.InputQueryStrings = slices.Clone(defaults.InputQueryStrings)
		}
		if defaults.ExtraConfig != nil {
			entry.ExtraConfig = mergeExtraConfig(entry.ExtraConfig, defaults.ExtraConfig)
		}
	}
}

// applyBackendDefaults applies CR-level BackendDefaults to all backends in all
// entries. Scalar fields (Encoding, SD, SDScheme, DisableHostSanitize) are set
// only when the backend does not already have a value. Slice fields
// (InputHeaders, InputQueryStrings) replace the backend value. ExtraConfig is
// deep-merged into each backend's existing ExtraConfig.
func applyBackendDefaults(output *CUEOutput, defaults *v1alpha1.BackendDefaults) {
	if defaults == nil {
		return
	}
	for i := range output.Entries {
		for j := range output.Entries[i].Backends {
			applyBackendDefaultsToBackend(&output.Entries[i].Backends[j], defaults)
		}
	}
}

// applyBackendDefaultsToBackend fills unset backend fields from defaults.
// Scalar fields are set only when empty; slice fields replace when the backend
// has none; ExtraConfig is deep-merged.
func applyBackendDefaultsToBackend(backend *v1alpha1.BackendSpec, defaults *v1alpha1.BackendDefaults) {
	if defaults.Encoding != "" && backend.Encoding == "" {
		backend.Encoding = defaults.Encoding
	}
	if defaults.SD != "" && backend.SD == "" {
		backend.SD = defaults.SD
	}
	if defaults.SDScheme != "" && backend.SDScheme == "" {
		backend.SDScheme = defaults.SDScheme
	}
	if defaults.DisableHostSanitize != nil && backend.DisableHostSanitize == nil {
		val := *defaults.DisableHostSanitize
		backend.DisableHostSanitize = &val
	}
	if defaults.InputHeaders != nil {
		backend.InputHeaders = slices.Clone(defaults.InputHeaders)
	}
	if defaults.InputQueryStrings != nil {
		backend.InputQueryStrings = slices.Clone(defaults.InputQueryStrings)
	}
	if defaults.ExtraConfig != nil {
		backend.ExtraConfig = mergeExtraConfig(backend.ExtraConfig, defaults.ExtraConfig)
	}
}

// applyDefaultPolicyRef sets the default KrakenDBackendPolicy on all backends
// that don't already have one. PolicyRef is an operator-level concept (not a
// KrakenD schema field) so it lives at spec.defaults.policyRef rather than
// inside endpoint or backend defaults.
func applyDefaultPolicyRef(output *CUEOutput, policyRef *v1alpha1.PolicyRef) {
	if policyRef == nil {
		return
	}
	for i := range output.Entries {
		for j := range output.Entries[i].Backends {
			if output.Entries[i].Backends[j].PolicyRef == nil {
				output.Entries[i].Backends[j].PolicyRef = policyRef
			}
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
		oldKey := entry.Endpoint + ":" + entry.Method
		applyURLTransformToEntry(entry, transform, hostMap)
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

// transformIssuePaths gives each issue the path strip and add-prefix its
// entry would have after applyURLTransform.
func transformIssuePaths(issues []OperationIssue, transform *v1alpha1.URLTransformSpec) {
	for i := range issues {
		issues[i].Path = transformPath(issues[i].Path, transform)
	}
}

// transformPath applies the path strip and add-prefix of transform to path.
func transformPath(path string, transform *v1alpha1.URLTransformSpec) string {
	if transform.StripPathPrefix != "" {
		path = strings.TrimPrefix(path, transform.StripPathPrefix)
		if path == "" {
			path = "/"
		}
	}
	if transform.AddPathPrefix != "" {
		path = transform.AddPathPrefix + path
	}
	return path
}

// applyURLTransformToEntry applies host mapping and path strip/add-prefix to a
// single entry. hostMap is the precomputed From→To map.
func applyURLTransformToEntry(
	entry *v1alpha1.EndpointEntry,
	transform *v1alpha1.URLTransformSpec,
	hostMap map[string]string,
) {
	for j := range entry.Backends {
		for k, host := range entry.Backends[j].Host {
			if to, ok := hostMap[host]; ok {
				entry.Backends[j].Host[k] = to
			}
		}
	}
	entry.Endpoint = transformPath(entry.Endpoint, transform)
}

// ApplyURLTransformToEntries applies a URLTransformSpec (host mapping + path
// strip/add-prefix) to each entry in the slice. It is used to transform
// user-designated additional endpoints the same way spec-derived endpoints are
// transformed. The backend urlPattern is intentionally left untouched. A nil
// transform is a no-op.
func ApplyURLTransformToEntries(entries []v1alpha1.EndpointEntry, transform *v1alpha1.URLTransformSpec) {
	if transform == nil {
		return
	}
	hostMap := make(map[string]string, len(transform.HostMapping))
	for _, m := range transform.HostMapping {
		hostMap[m.From] = m.To
	}
	for i := range entries {
		applyURLTransformToEntry(&entries[i], transform, hostMap)
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

	// Build operationID → entry index lookup in Entries order, first
	// occurrence wins: the generator publishes only the first entry for a
	// duplicate operationId, so the override must land on that one.
	opIDIndex := make(map[string]int, len(output.Entries))
	count := make(map[string]int, len(output.Entries))
	for i := range output.Entries {
		key := output.Entries[i].Endpoint + ":" + output.Entries[i].Method
		if opID, ok := output.OperationIDs[key]; ok {
			if _, seen := opIDIndex[opID]; !seen {
				opIDIndex[opID] = i
			}
			if opID != "" {
				count[opID]++
			}
		}
	}
	for _, failed := range output.Failed {
		if failed.OperationID != "" {
			count[failed.OperationID]++
		}
	}

	for _, ov := range overrides {
		if count[ov.OperationID] > 1 {
			output.AmbiguousOverrides = append(output.AmbiguousOverrides, ov.OperationID)
		}
		idx, ok := opIDIndex[ov.OperationID]
		if !ok {
			// An override whose target failed evaluation is held with it,
			// not unmatched, and moves it to the route it gives the entry.
			if count[ov.OperationID] == 0 {
				output.UnmatchedOverrides = append(output.UnmatchedOverrides, ov.OperationID)
			}
			remapFailed(output.Failed, ov)
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
			entry.InputHeaders = slices.Clone(ov.InputHeaders)
		}
		if ov.InputQueryStrings != nil {
			entry.InputQueryStrings = slices.Clone(ov.InputQueryStrings)
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
			if bo.Index < 0 || bo.Index >= len(entry.Backends) {
				output.UnmatchedOverrides = append(output.UnmatchedOverrides,
					fmt.Sprintf("%s backends[%d]", ov.OperationID, bo.Index))
				continue
			}
			if bo.ExtraConfig != nil {
				entry.Backends[bo.Index].ExtraConfig = &runtime.RawExtension{
					Raw: append([]byte(nil), bo.ExtraConfig.Raw...),
				}
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

// remapFailed gives every failed operation of ov the endpoint and method the
// override sets, so the failure is judged where the operation would publish.
func remapFailed(failed []OperationIssue, ov v1alpha1.OperationOverride) {
	for i := range failed {
		if failed[i].OperationID == "" || failed[i].OperationID != ov.OperationID {
			continue
		}
		if ov.Endpoint != "" {
			failed[i].Path = ov.Endpoint
		}
		if ov.Method != "" {
			failed[i].Method, failed[i].methodKnown = ov.Method, true
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
	if baseMap == nil || patchMap == nil {
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
