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

package renderer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// ValidatorOptions configures the KrakenD config validator.
type ValidatorOptions struct {
	Executor   CommandExecutor
	BinaryPath string
}

// KrakenDValidator validates rendered KrakenD JSON via krakend check.
type KrakenDValidator struct {
	Executor   CommandExecutor
	BinaryPath string
}

// NewValidator creates a KrakenDValidator with the given options.
func NewValidator(opts ValidatorOptions) *KrakenDValidator {
	return &KrakenDValidator{
		Executor:   opts.Executor,
		BinaryPath: opts.BinaryPath,
	}
}

// KrakenDExecutor runs krakend CLI commands.
type KrakenDExecutor struct {
	BinaryPath string
}

// NewKrakenDExecutor creates a command executor for the krakend binary.
func NewKrakenDExecutor(binaryPath string) *KrakenDExecutor {
	return &KrakenDExecutor{BinaryPath: binaryPath}
}

// Execute runs a command and returns its combined output.
func (e *KrakenDExecutor) Execute(
	ctx context.Context, name string, args ...string,
) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// Validate writes jsonData to a temp file and runs krakend check -tlc.
func (v *KrakenDValidator) Validate(ctx context.Context, jsonData []byte) (retErr error) {
	tmpFile, err := os.CreateTemp("", "krakend-config-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer func() {
		if err := os.Remove(tmpName); err != nil && retErr == nil {
			retErr = fmt.Errorf("removing temp file: %w", err)
		}
	}()

	if _, writeErr := tmpFile.Write(jsonData); writeErr != nil {
		if closeErr := tmpFile.Close(); closeErr != nil {
			return fmt.Errorf("writing config to temp file: %w, close error: %w", writeErr, closeErr)
		}
		return fmt.Errorf("writing config to temp file: %w", writeErr)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	output, err := v.Executor.Execute(ctx, v.BinaryPath, "check", "-tlc", "-c", tmpName)
	if err != nil {
		return &ValidationError{
			Output: string(output),
			Err:    err,
		}
	}
	return nil
}

// PrepareValidationCopy strips wildcard endpoints for EE configs that use
// the CE validator (which rejects /* patterns).
func (v *KrakenDValidator) PrepareValidationCopy(jsonData []byte, eeWithoutFallback bool) ([]byte, error) {
	if !eeWithoutFallback {
		return jsonData, nil
	}
	var config map[string]any
	if err := json.Unmarshal(jsonData, &config); err != nil {
		return nil, fmt.Errorf("unmarshaling config for validation copy: %w", err)
	}
	endpoints, ok := config["endpoints"].([]any)
	if !ok {
		return jsonData, nil
	}
	var filtered []any
	for _, ep := range endpoints {
		epMap, ok := ep.(map[string]any)
		if !ok {
			continue
		}
		if path, ok := epMap["endpoint"].(string); ok && path == "/*" {
			continue
		}
		filtered = append(filtered, ep)
	}
	config["endpoints"] = filtered
	return serializeJSON(config)
}
