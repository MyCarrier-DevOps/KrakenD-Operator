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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// describeDrops lists what a CE render drops: a namespace, with the keys CE
// does not honor when it honors the rest of the block.
func describeDrops(drops []renderer.CEDrop) string {
	parts := make([]string, len(drops))
	for i, d := range drops {
		parts[i] = d.String()
	}
	return strings.Join(parts, ", ")
}

// clashErrors reports router clashes on p: the first maxEntryCauses as causes
// of their own, cut to the warning limit, the rest counted in one more. A
// clash names endpoints, methods and paths only. When capped, the render
// stopped resolving clashes, so a new one cannot be told apart from those it
// left unresolved: that is the one cause.
func clashErrors(p *field.Path, clashes []configcheck.Clash, capped bool) field.ErrorList {
	if capped {
		return field.ErrorList{field.Invalid(p, field.OmitValueType{}, configcheck.ClashesCapped)}
	}
	var errs field.ErrorList
	for i, c := range clashes {
		if i == maxEntryCauses {
			errs = append(errs, field.Invalid(p, field.OmitValueType{},
				fmt.Sprintf("%d more entries clash the same way", len(clashes)-i)))
			break
		}
		errs = append(errs, field.Invalid(p, field.OmitValueType{},
			truncate("KrakenD's router cannot serve both: "+c.String(), warningLimit)))
	}
	return errs
}

// suspectsOf returns the endpoints of served that a group check of them
// (group) did not judge (Verdict.Suspect).
func suspectsOf(group configcheck.Verdict, served []v1alpha1.KrakenDEndpoint) []v1alpha1.KrakenDEndpoint {
	return slices.DeleteFunc(slices.Clone(served), func(ep v1alpha1.KrakenDEndpoint) bool {
		return !group.Suspect(types.NamespacedName{Namespace: ep.Namespace, Name: ep.Name})
	})
}

// servedEndpoints returns eps less those their gateway leaves out for failing
// validation on their own (Accepted False with reason EndpointInvalid or
// PolicyInvalid for their current generation). An endpoint not judged yet,
// or changed since its verdict, counts as served, so a change is judged
// against it.
func servedEndpoints(eps []v1alpha1.KrakenDEndpoint) []v1alpha1.KrakenDEndpoint {
	return slices.DeleteFunc(slices.Clone(eps), func(ep v1alpha1.KrakenDEndpoint) bool {
		c := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted)
		return c != nil && c.Status == metav1.ConditionFalse && c.ObservedGeneration == ep.Generation &&
			(c.Reason == v1alpha1.ReasonEndpointInvalid || c.Reason == v1alpha1.ReasonPolicyInvalid)
	})
}

// servedByLastConfig reports whether the gateway's last applied config serves
// ep, going by what the gateway reported on its current generation: Accepted
// is True, which includes a reason of PartiallyAccepted.
func servedByLastConfig(ep v1alpha1.KrakenDEndpoint) bool {
	c := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted)
	return c != nil && c.ObservedGeneration == ep.Generation &&
		(c.Status == metav1.ConditionTrue || c.Reason == v1alpha1.ReasonPartiallyAccepted)
}

// scan is what failingEndpoints found among the suspects of a write.
type scan struct {
	// broken are the suspects the write breaks, by namespace/name: each fails
	// on its own with the write and did not fail without it (or was judged
	// with nothing to compare it with). At most maxEntryCauses.
	broken []string
	// already says a suspect fails on its own both with and without the write.
	already bool
	// unchecked counts the suspects left unjudged.
	unchecked int
	// stopped is why the scan ended before judging every suspect, when it was
	// not maxEntryCauses: a check that could not run, such as one cut off by
	// the admission deadline.
	stopped error
}

// failingEndpoints judges each suspect on its own with a write (now, with the
// suspect as its endpoint) and, when that fails, without it (was; nil when
// there is nothing to compare with: nothing was stored before, or the stored
// root fails and the suspect was served by the last applied config, so that
// every failure is the write's). It stops
// at maxEntryCauses broken endpoints, or at the first check that cannot run,
// which includes one the admission deadline cuts off, and counts the suspects
// left. A caller keeps a denial the scan found however the scan ended.
func failingEndpoints(ctx context.Context, chk ConfigChecker, memo configcheck.Memo,
	now configcheck.EndpointUnit, was *configcheck.EndpointUnit, suspects []v1alpha1.KrakenDEndpoint) scan {
	var s scan
	for i := range suspects {
		if len(s.broken) == maxEntryCauses {
			s.unchecked = len(suspects) - i
			return s
		}
		ep := &suspects[i]
		now.Endpoint = ep
		v, err := chk.CheckEndpoint(ctx, now, memo)
		if err == nil && !v.OK && was != nil {
			before := *was
			before.Endpoint = ep
			var stored configcheck.EndpointVerdict
			stored, err = chk.CheckEndpoint(ctx, before, memo)
			if err == nil && !stored.OK {
				s.already = true
				continue
			}
		}
		if err != nil {
			s.unchecked, s.stopped = len(suspects)-i, err
			return s
		}
		if !v.OK {
			s.broken = append(s.broken, ep.Namespace+"/"+ep.Name)
		}
	}
	return s
}

// brokenList names the endpoints a write breaks, quoting nothing of them, in
// at most room bytes. It counts the suspects the scan left unjudged: past
// maxEntryCauses, or once a check could not run. That count always survives
// whole; names that do not fit are folded into a count of their own, which
// says they were checked.
func brokenList(s scan, room int) string {
	const header = "with this change these KrakenDEndpoints fail validation: "
	var unchecked string
	switch {
	case s.unchecked == 0:
	case s.stopped != nil:
		unchecked = fmt.Sprintf(" (%d not checked within the admission time)", s.unchecked)
	default:
		unchecked = fmt.Sprintf(" (+%d more not checked)", s.unchecked)
	}
	listed := func(n int) string {
		text := header + strings.Join(s.broken[:n], ", ")
		if n < len(s.broken) {
			text += fmt.Sprintf(" (+%d more)", len(s.broken)-n)
		}
		return text + unchecked
	}
	n := len(s.broken)
	for n > 0 && len(listed(n)) > room {
		n--
	}
	return listed(n)
}
