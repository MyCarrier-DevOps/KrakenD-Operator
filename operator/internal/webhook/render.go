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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// renderChecks are the checks of the verdict ratchet, each over a proposed
// change: the gateway with it, and without it, then, optionally, the same two
// on the isolated baseline used when the gateway already fails (a change with
// no isolated form leaves isoAfter and isoBefore nil). newFailure, when set,
// decides from the two verdicts whether a gateway that already fails is made
// newly worse by the change, which is then denied without the isolated checks.
type renderChecks struct {
	after, before, isoAfter, isoBefore func(context.Context) (configcheck.Verdict, error)
	newFailure                         func(before, after configcheck.Verdict) bool
}

// bindCheck fixes the gateway and endpoints a check runs on.
func bindCheck(
	run func(context.Context, *v1alpha1.KrakenDGateway, []v1alpha1.KrakenDEndpoint) (configcheck.Verdict, error),
	gw *v1alpha1.KrakenDGateway, eps []v1alpha1.KrakenDEndpoint,
) func(context.Context) (configcheck.Verdict, error) {
	return func(ctx context.Context) (configcheck.Verdict, error) { return run(ctx, gw, eps) }
}

// ratchetRender rejects a change only when it turns a passing config into a
// failing one. It runs after, then before; when before fails too the failure
// is a warning (preexisting words it from before's verdict) unless newFailure
// finds the change newly to blame, or the change fails on the isolated
// baseline where its own baseline passed. Without an
// isolated baseline (isoAfter nil) a preexisting failure is only a warning.
// A check that cannot run is a 500 with no warning: the request is not
// judged. deny builds the rejection from a failing verdict.
func ratchetRender(
	ctx context.Context, c renderChecks, deny func(configcheck.Verdict) error,
	preexisting func(before configcheck.Verdict) string,
) (admission.Warnings, error) {
	after, err := c.after(ctx)
	if err != nil || after.OK {
		return nil, checkErr(err)
	}
	before, err := c.before(ctx)
	if err != nil {
		return nil, checkErr(err)
	}
	if before.OK {
		return nil, deny(after)
	}
	if c.newFailure != nil && c.newFailure(before, after) {
		return nil, deny(after)
	}
	warning := admission.Warnings{preexisting(before)}
	if c.isoAfter == nil {
		return warning, nil
	}
	isoAfter, err := c.isoAfter(ctx)
	if err != nil {
		return nil, checkErr(err)
	}
	if isoAfter.OK {
		return warning, nil
	}
	isoBefore, err := c.isoBefore(ctx)
	if err != nil {
		return nil, checkErr(err)
	}
	if isoBefore.OK {
		return nil, deny(isoAfter)
	}
	return warning, nil
}

// describeDrops lists what a CE render drops: a namespace, with the keys CE
// does not honor when it honors the rest of the block.
func describeDrops(drops []renderer.CEDrop) string {
	parts := make([]string, len(drops))
	for i, d := range drops {
		parts[i] = d.String()
	}
	return strings.Join(parts, ", ")
}

// blames reports whether v names an entry of endpoint.
func blames(v configcheck.Verdict, endpoint types.NamespacedName) bool {
	return slices.ContainsFunc(v.Findings, func(f configcheck.Finding) bool { return f.Endpoint == endpoint })
}

// newlyBlamed is the endpoint rule of the ratchet on a failing gateway: the
// candidate's own KrakenDEndpoint is blamed after the change and was not
// before it. An isolated check cannot see a clash with another endpoint, and a
// change reaches a later check stage than before only by removing a failure of
// its own object, which was then blamed before.
func newlyBlamed(self types.NamespacedName) func(before, after configcheck.Verdict) bool {
	return func(before, after configcheck.Verdict) bool {
		return blames(after, self) && !blames(before, self)
	}
}
