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
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// renderChecks are the checks of the verdict ratchet, each over a proposed
// change: the gateway with it, and without it, then, optionally, the same two
// on the isolated baseline used when the gateway already fails (a change with
// no isolated form leaves isoAfter and isoBefore nil).
type renderChecks struct {
	after, before, isoAfter, isoBefore func(context.Context) (configcheck.Verdict, error)
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
// is a warning (preexisting words it from before's verdict) unless the change
// fails on the isolated baseline where its own baseline passed. Without an
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
	parts := make([]string, 0, len(drops))
	for _, d := range drops {
		if len(d.Keys) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%s)", d.Namespace, strings.Join(d.Keys, ", ")))
			continue
		}
		parts = append(parts, d.Namespace)
	}
	return strings.Join(parts, ", ")
}
