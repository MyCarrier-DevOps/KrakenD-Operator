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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// EditionFor is the edition a render of gw is validated as: CE for a CE
// gateway or for an EE gateway in license fallback, otherwise EE.
func EditionFor(gw *v1alpha1.KrakenDGateway, ceFallback bool) v1alpha1.Edition {
	if gw.Spec.Edition == v1alpha1.EditionEE && !ceFallback {
		return v1alpha1.EditionEE
	}
	return v1alpha1.EditionCE
}

// DefaultValidateTimeout bounds one krakend check run when no Timeout is
// configured. A healthy run takes about a second.
const DefaultValidateTimeout = 30 * time.Second

// ValidatorOptions configures the KrakenD config validator.
type ValidatorOptions struct {
	Executor   CommandExecutor
	BinaryPath string
	// Timeout bounds one krakend check run. Zero means DefaultValidateTimeout.
	Timeout time.Duration
}

// KrakenDValidator validates rendered KrakenD JSON via krakend check.
type KrakenDValidator struct {
	Executor   CommandExecutor
	BinaryPath string
	Timeout    time.Duration
}

// NewValidator creates a KrakenDValidator with the given options.
func NewValidator(opts ValidatorOptions) *KrakenDValidator {
	return &KrakenDValidator{
		Executor:   opts.Executor,
		BinaryPath: opts.BinaryPath,
		Timeout:    opts.Timeout,
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

// Validate checks jsonData as the given edition would load it: the edition's
// validation copy, the EE router rules, then krakend check -t -n.
func (v *KrakenDValidator) Validate(ctx context.Context, jsonData []byte, edition v1alpha1.Edition) error {
	return v.validate(ctx, jsonData, edition, "-t", "-n")
}

// Lint is Validate without krakend's one-second router test (check -n). It
// is the admission check; the route check (routecheck.go) covers what -t
// would catch.
func (v *KrakenDValidator) Lint(ctx context.Context, jsonData []byte, edition v1alpha1.Edition) error {
	return v.validate(ctx, jsonData, edition, "-n")
}

// validate prepares the edition's copy of jsonData, applies the rules checked
// in Go (the EE router rule, then the route check) and runs krakend check with
// flags on the copy.
func (v *KrakenDValidator) validate(
	ctx context.Context, jsonData []byte, edition v1alpha1.Edition, flags ...string,
) error {
	doc, findings, err := validationCopy(ctx, jsonData, edition)
	if err != nil {
		return fmt.Errorf("preparing validation copy: %w", err)
	}
	if len(findings) > 0 {
		return &ValidationError{Output: strings.Join(findings, "\n"), Err: errEEWildcardRule}
	}
	conflicts, err := routeConflicts(ctx, doc)
	if err != nil {
		return fmt.Errorf("checking routes: %w", err)
	}
	if len(conflicts) > 0 {
		return &ValidationError{Output: strings.Join(conflicts, "\n"), Err: errRouteConflict}
	}
	return v.check(ctx, doc, flags...)
}

// errEEWildcardRule is the verdict for an EE wildcard endpoint that EE
// would refuse, found before krakend check runs.
var errEEWildcardRule = errors.New("EE wildcard rule")

// check writes jsonData to a temp file and runs `krakend check <flags> -c` on
// it. -n lints against the JSON schema built into the binary, so validation
// never needs network access; -t also tests the router.
//
// It returns a *ValidationError only when krakend check ran to completion and
// rejected the config (a non-zero exit status before the deadline): that is a
// verdict on the config. Every other failure (binary missing, temp-file I/O,
// deadline exceeded, process killed by a signal) comes back as a plain error:
// the config was not judged and the caller should retry.
func (v *KrakenDValidator) check(ctx context.Context, jsonData []byte, flags ...string) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, v.timeout())
	defer cancel()

	tmpFile, err := os.CreateTemp("", "krakend-config-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", withoutPath(err))
	}
	tmpName := tmpFile.Name()
	defer func() {
		if err := os.Remove(tmpName); err != nil && retErr == nil {
			retErr = fmt.Errorf("removing temp file: %w", withoutPath(err))
		}
	}()

	if _, writeErr := tmpFile.Write(jsonData); writeErr != nil {
		if closeErr := tmpFile.Close(); closeErr != nil {
			return fmt.Errorf("writing config to temp file: %w, close error: %w",
				withoutPath(writeErr), withoutPath(closeErr))
		}
		return fmt.Errorf("writing config to temp file: %w", withoutPath(writeErr))
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", withoutPath(err))
	}

	args := append(append([]string{"check"}, flags...), "-c", tmpName)
	output, err := v.Executor.Execute(ctx, v.BinaryPath, args...)
	if err != nil {
		return classifyCheckError(ctx, bytes.ReplaceAll(output, []byte(tmpName), []byte(checkedConfigName)), err)
	}
	return nil
}

// checkedConfigName stands in for the random temp file name in krakend's
// output, so a verdict reads the same on every run.
const checkedConfigName = "krakend.json"

// withoutPath drops the file name from a *fs.PathError and keeps the
// operation and the underlying error. The temp file name is random, so an
// error that carries it reads differently on every call; callers that
// record the message in a status would then write it on every retry. The
// underlying error stays wrapped, so errors.Is still matches the errno.
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s: %w", pathErr.Op, pathErr.Err)
	}
	return err
}

// classifyCheckError separates a verdict from a validator that could not
// run. A process killed because ctx ended also surfaces as an
// *exec.ExitError (exit code -1), so the context is checked first.
func classifyCheckError(ctx context.Context, output []byte, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("krakend check did not finish: %w: %w", ctxErr, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return &ValidationError{Output: string(output), Err: err}
	}
	return fmt.Errorf("running krakend check: %w", err)
}

// validationCopy creates the document the embedded CE krakend binary checks
// for a render of the given edition. The CE and EE 2.13 binaries embed the same
// schemas, so nothing is stripped: for an EE render, each wildcard endpoint is
// rewritten to a parameter route and every endpoint stays at its index.
// Non-nil findings are a verdict reached without running krakend check.
func validationCopy(ctx context.Context, jsonData []byte, edition v1alpha1.Edition) (doc []byte, findings []string, err error) {
	var config map[string]any
	if err := json.Unmarshal(jsonData, &config); err != nil {
		return nil, nil, fmt.Errorf("unmarshaling config for validation copy: %w", err)
	}

	endpoints, ok := config["endpoints"].([]any)
	if !ok || edition != v1alpha1.EditionEE {
		return jsonData, nil, nil
	}
	findings, err = eeWildcardFindings(ctx, endpoints)
	if err != nil {
		return nil, nil, err
	}
	if len(findings) > 0 {
		return nil, findings, nil
	}
	if !rewriteEEWildcards(endpoints) {
		return jsonData, nil, nil
	}
	doc, err = serializeJSON(config)
	return doc, nil, err
}

func (v *KrakenDValidator) timeout() time.Duration {
	if v.Timeout > 0 {
		return v.Timeout
	}
	return DefaultValidateTimeout
}
