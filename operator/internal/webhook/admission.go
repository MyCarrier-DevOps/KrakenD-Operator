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

package webhook

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

// admissionBudget is how long a validator works on one request. The API
// server cuts a webhook call off at 15 s with a generic timeout; stopping
// earlier lets the validator answer with a clear, transient error.
const admissionBudget = 12 * time.Second

// warningLimit bounds, in bytes, a verdict's summary quoted in a warning or a
// denial.
const warningLimit = 1024

// ConfigChecker renders a gateway's config with a proposed change and
// validates it. *configcheck.Checker implements it.
type ConfigChecker interface {
	CheckGateway(ctx context.Context, gw *v1alpha1.KrakenDGateway,
		replace []v1alpha1.KrakenDEndpoint) (configcheck.Verdict, error)
	CheckIsolated(ctx context.Context, gw *v1alpha1.KrakenDGateway,
		eps []v1alpha1.KrakenDEndpoint) (configcheck.Verdict, error)
	CheckGatewayPolicy(ctx context.Context, gw *v1alpha1.KrakenDGateway,
		policy *v1alpha1.KrakenDBackendPolicy) (configcheck.Verdict, error)
	LintPolicy(ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy) (configcheck.Verdict, error)
	SameConfig(ctx context.Context, old, gw *v1alpha1.KrakenDGateway) (bool, error)
	// Conflicts renders gw's endpoints with replace substituted in process
	// and returns what the render leaves out (configcheck.Checker).
	Conflicts(ctx context.Context, gw *v1alpha1.KrakenDGateway,
		replace []v1alpha1.KrakenDEndpoint) (configcheck.RouteConflicts, error)
}

// invalid is the admission error for field errors: 422 Invalid with one
// status cause per error, so kubectl and API clients see each rejected field.
// It is nil when errs is empty.
func invalid(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: kind}, name, errs)
}

// unavailable is the admission error for a failed lookup or an unavailable
// validator: 500, a transient server error. Controllers and GitOps tools retry
// on their own; kubectl prints it and exits.
func unavailable(err error) error {
	return apierrors.NewInternalError(err)
}

// checkErr is the admission error for a checker failure: the validator could
// not run, so the request is not judged and the failure is transient. nil
// stays nil.
func checkErr(err error) error {
	if err == nil {
		return nil
	}
	return unavailable(fmt.Errorf("validating the gateway config: %w", err))
}

// truncate cuts s to at most limit bytes, on a rune boundary, and marks the
// cut.
func truncate(s string, limit int) string {
	return configcheck.TruncateEllipsis(s, limit)
}

// newErrors returns the errors in errs that old does not already have, matched
// on their full text (field, type, value and detail), so an update is rejected
// only for problems it introduces.
func newErrors(errs, old field.ErrorList) field.ErrorList {
	existing := make(map[string]struct{}, len(old))
	for _, e := range old {
		existing[e.Error()] = struct{}{}
	}
	var fresh field.ErrorList
	for _, e := range errs {
		if _, ok := existing[e.Error()]; !ok {
			fresh = append(fresh, e)
		}
	}
	return fresh
}

// echoLimit bounds, in bytes, a user-supplied name a message quotes, such as a
// spec.version or an operationId: the CRDs do not bound them.
const echoLimit = 64
