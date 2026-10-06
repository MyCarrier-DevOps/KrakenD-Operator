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

// foreignCheckOutput reports whether f is krakend check output that names no
// endpoint of namespace ns: another namespace's endpoint, or none, which is how
// krakend reports the gateway root and also some errors of an endpoint, such as
// an invalid backend host. krakend check prints the values it refuses, and a
// writer in ns may read neither the gateway nor another namespace's endpoint.
// The requester's own unnamed lines are quoted by the denial once the gateway
// root passes alone. The route and EE wildcard checks print only methods and
// paths, so their findings are not foreign.
func foreignCheckOutput(stage renderer.RejectionStage, f configcheck.Finding, ns string) bool {
	if stage == renderer.StageRoute || stage == renderer.StageEEWildcard {
		return false
	}
	return f.Endpoint.Name == "" || f.Endpoint.Namespace != ns
}

// withholdForeign returns the findings of v that a writer in namespace ns may
// be shown, and counts the foreignCheckOutput ones it leaves out.
func withholdForeign(v configcheck.Verdict, ns string) (shown []configcheck.Finding, withheld int) {
	for _, f := range v.Findings {
		if foreignCheckOutput(v.Stage, f, ns) {
			withheld++
			continue
		}
		shown = append(shown, f)
	}
	return shown, withheld
}

// withheldNote counts n withheld findings for a writer in namespace ns. Each
// names no endpoint of ns: it names one in another namespace, or none at all,
// which is how krakend check reports the gateway root and also some errors of
// an endpoint (an invalid backend host).
func withheldNote(n int, ns string) string {
	return fmt.Sprintf("%d findings that name no endpoint of namespace %s are not shown", n, ns)
}

// shownSummary is v's summary, cut to limit, over the findings a writer in
// namespace ns may be shown, followed by a count of those withheld.
func shownSummary(v configcheck.Verdict, ns string, limit int) string {
	shown, withheld := withholdForeign(v, ns)
	summary := configcheck.Verdict{Findings: shown}.Summary(limit)
	if withheld == 0 {
		return summary
	}
	note := withheldNote(withheld, ns)
	if summary == "" {
		return note
	}
	return summary + "; " + note
}

// onceCheck runs check at most once and returns its first result to every
// call, so the ratchet and the denial it builds share one isolated check.
func onceCheck(
	check func(context.Context) (configcheck.Verdict, error),
) func(context.Context) (configcheck.Verdict, error) {
	var (
		ran     bool
		verdict configcheck.Verdict
		err     error
	)
	return func(ctx context.Context) (configcheck.Verdict, error) {
		if !ran {
			verdict, err = check(ctx)
			ran = true
		}
		return verdict, err
	}
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

// newRouteRefusals is the gateway rule of the ratchet on a failing gateway,
// for a change after which the route check refuses routes:
//   - when the gateway failed before only at krakend check, its route check ran
//     to completion without a refusal, so any refusal is the change's;
//   - when it failed before at the route check, a refusal counts against the
//     change when every endpoint it names was unblamed before. The route check
//     leaves a refused route out of its engine, so a refusal that names an
//     endpoint blamed before may only have been hidden by an earlier one, which
//     is no fault of the change. When the check stopped at its cap before the
//     change, which refusals it hid is unknown and this does not apply.
//
// In every other case the rule does not apply.
func newRouteRefusals(before, after configcheck.Verdict) bool {
	if after.Stage != renderer.StageRoute {
		return false
	}
	switch {
	case before.Stage == renderer.StageCheck:
		return len(after.Refusals) > 0
	case before.Stage == renderer.StageRoute && !before.RefusalsCapped:
		return slices.ContainsFunc(after.Refusals, func(r configcheck.Refusal) bool {
			return len(r.Endpoints) > 0 && !slices.ContainsFunc(r.Endpoints, func(e types.NamespacedName) bool {
				return blames(before, e)
			})
		})
	}
	return false
}
