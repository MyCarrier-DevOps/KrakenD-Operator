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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
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
	s.judge(ctx, chk, memo, now, was, suspects)
	return s
}

// judge adds to s what judging suspects finds (failingEndpoints). The cap of
// maxEntryCauses broken endpoints counts those s holds already, and suspects
// left unjudged add to the count s holds.
func (s *scan) judge(ctx context.Context, chk ConfigChecker, memo configcheck.Memo,
	now configcheck.EndpointUnit, was *configcheck.EndpointUnit, suspects []v1alpha1.KrakenDEndpoint) {
	for i := range suspects {
		if len(s.broken) == maxEntryCauses {
			s.unchecked += len(suspects) - i
			return
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
			s.unchecked, s.stopped = s.unchecked+len(suspects)-i, err
			return
		}
		if !v.OK {
			s.broken = append(s.broken, ep.Namespace+"/"+ep.Name)
		}
	}
}

// failingEndpointsDecidingFirst is failingEndpoints for a write whose group
// check failed, given the check of the same endpoints as a group with the
// stored config (before). When that passes, an endpoint it did not mask passes
// on its own with the stored config unless it references a policy that fails
// krakend check alone, which no group check judges. So only the endpoints it
// masked and those that use such a policy (checked with the stored policies;
// baseline stands for the stored policy being written, nil for a gateway
// write) can have failed before the write: they are judged first. If none of
// them failed both ways, the write is the cause of the failure, whichever
// endpoints the rest of the scan reaches, and decided is true: a scan the
// admission deadline cuts off then changes no verdict, only how many
// endpoints are named. The decision is withdrawn when any endpoint turns out
// to fail both ways. When before fails, nothing is decided, and the suspects
// are judged in their order.
func failingEndpointsDecidingFirst(ctx context.Context, c client.Reader, chk ConfigChecker, memo configcheck.Memo,
	now configcheck.EndpointUnit, was *configcheck.EndpointUnit, baseline *v1alpha1.KrakenDBackendPolicy,
	before configcheck.Verdict, suspects []v1alpha1.KrakenDEndpoint) (s scan, decided bool, err error) {
	if !before.OK {
		return failingEndpoints(ctx, chk, memo, now, was, suspects), false, nil
	}
	users, err := usersOfFailingPolicies(ctx, c, chk, memo, baseline, suspects)
	if err != nil {
		return scan{}, false, err
	}
	var first, rest []v1alpha1.KrakenDEndpoint
	for _, ep := range suspects {
		name := types.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
		if before.Suspect(name) || users[name] {
			first = append(first, ep)
		} else {
			rest = append(rest, ep)
		}
	}
	s.judge(ctx, chk, memo, now, was, first)
	decided = s.stopped == nil
	if decided {
		s.judge(ctx, chk, memo, now, was, rest)
	} else {
		s.unchecked += len(rest)
	}
	return s, decided && !s.already, nil
}

// usersOfFailingPolicies returns the endpoints of eps that reference a policy
// failing krakend check on its own: each distinct policy is checked once, as
// stored, or as baseline when it is the policy a write replaces. A policy that
// does not exist fails nothing: no render includes its endpoints, and the
// endpoint controller reports it missing (PolicyNotFound).
func usersOfFailingPolicies(ctx context.Context, c client.Reader, chk ConfigChecker, memo configcheck.Memo,
	baseline *v1alpha1.KrakenDBackendPolicy, eps []v1alpha1.KrakenDEndpoint) (map[types.NamespacedName]bool, error) {
	fails := make(map[string]bool)
	users := make(map[types.NamespacedName]bool)
	for i := range eps {
		for _, key := range fieldindex.EndpointPolicyKeys(&eps[i]) {
			failing, known := fails[key]
			if !known {
				var err error
				if failing, err = policyFailsAlone(ctx, c, chk, memo, baseline, key); err != nil {
					return nil, err
				}
				fails[key] = failing
			}
			if failing {
				users[types.NamespacedName{Namespace: eps[i].Namespace, Name: eps[i].Name}] = true
			}
		}
	}
	return users, nil
}

// policyFailsAlone checks the policy of key ("namespace/name"), read through
// c, unless it is baseline's own.
func policyFailsAlone(ctx context.Context, c client.Reader, chk ConfigChecker, memo configcheck.Memo,
	baseline *v1alpha1.KrakenDBackendPolicy, key string) (bool, error) {
	policy := baseline
	if policy == nil || policy.Namespace+"/"+policy.Name != key {
		namespace, name, _ := strings.Cut(key, "/")
		policy = &v1alpha1.KrakenDBackendPolicy{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, policy); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("getting policy %s: %w", key, err)
		}
	}
	alone, err := chk.CheckPolicy(ctx, policy, memo)
	return !alone.OK, err
}

// brokenList names the endpoints a write breaks, quoting nothing of them, in
// at most room bytes. It counts the suspects the scan left unjudged: past
// maxEntryCauses, or once a check could not run. That count always survives
// whole; names that do not fit are folded into a count of their own, which
// says they were checked.
func brokenList(s scan, subject string, room int) string {
	header := "with this change these KrakenDEndpoints fail validation: "
	if len(s.broken) == 0 {
		header = "with this change " + subject + " fail validation"
	}
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
