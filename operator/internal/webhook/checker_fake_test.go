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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

// scriptedChecker answers each check with the next verdict of its script (OK
// once the script runs out), or with err, and records which checks ran and
// how much of its deadline each call had left and which endpoint entries it
// was handed ("ns/name[METHOD /path ...]"; "-" for none). err fails the call
// numbered failCall (1-based; 0 means the first). gateways records, for each
// gateway or isolated check, the gateway it was handed as "edition/timeout".
type scriptedChecker struct {
	verdicts  []configcheck.Verdict
	err       error
	failCall  int
	calls     []string
	deadlines []time.Duration
	args      []string
	gateways  []string
	// same and sameErr are the answer of SameConfig: whether the two gateway
	// versions render the same config, or why that could not be told.
	same    bool
	sameErr error
	// conflicts answers Conflicts from the gateway and the replace set it is
	// handed; nil answers none. conflictErr fails every Conflicts call.
	// conflictCalls records each call's replace set. Conflicts is not a check:
	// it is recorded neither in calls nor in deadlines.
	conflicts     func(gw *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint) configcheck.RouteConflicts
	conflictErr   error
	conflictCalls [][]v1alpha1.KrakenDEndpoint
	// endpointVerdicts answer CheckEndpoint in order (OK once they run out).
	endpointVerdicts []configcheck.EndpointVerdict
	// memos records, for each root, policy, group and endpoint check, whether it was handed
	// a memo.
	memos []bool
	// overrides records, for each endpoint check, the namespace/name of the
	// policy override it was handed, "-" for none.
	overrides []string
	// delay holds every check that long, giving up when its context ends.
	delay time.Duration
	// failOnly makes err fail call failCall alone, not every call after it.
	failOnly bool
}

func (s *scriptedChecker) Conflicts(
	_ context.Context, gw *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint,
) (configcheck.RouteConflicts, error) {
	s.conflictCalls = append(s.conflictCalls, replace)
	if s.conflictErr != nil {
		return configcheck.RouteConflicts{}, s.conflictErr
	}
	if s.conflicts == nil {
		return configcheck.RouteConflicts{}, nil
	}
	return s.conflicts(gw, replace), nil
}

// SameConfig answers from same and sameErr and records nothing: it runs no check.
func (s *scriptedChecker) SameConfig(context.Context, *v1alpha1.KrakenDGateway, *v1alpha1.KrakenDGateway) (bool, error) {
	return s.same, s.sameErr
}

// record notes a check and returns the error it fails with, if any.
func (s *scriptedChecker) record(ctx context.Context, call string, eps []v1alpha1.KrakenDEndpoint) error {
	s.calls = append(s.calls, call)
	s.args = append(s.args, describe(eps))
	if d, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, time.Until(d))
	} else {
		s.deadlines = append(s.deadlines, 0)
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if n, at := len(s.calls), max(s.failCall, 1); s.err != nil && (n == at || n > at && !s.failOnly) {
		return s.err
	}
	return nil
}

func (s *scriptedChecker) next(ctx context.Context, call string, eps []v1alpha1.KrakenDEndpoint) (configcheck.Verdict, error) {
	if err := s.record(ctx, call, eps); err != nil {
		return configcheck.Verdict{}, err
	}
	if len(s.verdicts) == 0 {
		return configcheck.Verdict{OK: true}, nil
	}
	v := s.verdicts[0]
	s.verdicts = s.verdicts[1:]
	return v, nil
}

func (s *scriptedChecker) CheckRoot(
	ctx context.Context, r configcheck.Root, memo configcheck.Memo,
) (configcheck.Verdict, error) {
	s.gateways = append(s.gateways, string(r.Gateway.Spec.Edition)+"/"+r.Gateway.Spec.Config.Timeout)
	s.memos = append(s.memos, memo != nil)
	return s.next(ctx, "root", nil)
}

// CheckEndpoint records the endpoint, and ":raw" of the policy override when
// there is one, in args.
func (s *scriptedChecker) CheckEndpoint(
	ctx context.Context, u configcheck.EndpointUnit, memo configcheck.Memo,
) (configcheck.EndpointVerdict, error) {
	s.memos = append(s.memos, memo != nil)
	err := s.record(ctx, "endpoint", []v1alpha1.KrakenDEndpoint{*u.Endpoint})
	override := "-"
	if u.Override != nil {
		s.args[len(s.args)-1] += ":" + rawOf(u.Override)
		override = u.Override.Namespace + "/" + u.Override.Name
	}
	s.overrides = append(s.overrides, override)
	if err != nil {
		return configcheck.EndpointVerdict{}, err
	}
	if len(s.endpointVerdicts) == 0 {
		return configcheck.EndpointVerdict{OK: true}, nil
	}
	v := s.endpointVerdicts[0]
	s.endpointVerdicts = s.endpointVerdicts[1:]
	return v, nil
}

func (s *scriptedChecker) CheckGateway(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint,
) (configcheck.Verdict, error) {
	s.gateways = append(s.gateways, string(gw.Spec.Edition)+"/"+gw.Spec.Config.Timeout)
	if len(replace) > 0 {
		return s.next(ctx, "gateway+candidate", replace)
	}
	return s.next(ctx, "gateway", replace)
}

func (s *scriptedChecker) CheckIsolated(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, eps []v1alpha1.KrakenDEndpoint,
) (configcheck.Verdict, error) {
	s.gateways = append(s.gateways, string(gw.Spec.Edition)+"/"+gw.Spec.Config.Timeout)
	return s.next(ctx, "isolated", eps)
}

// CheckGatewayPolicy records "ns/gateway:raw" in args: the gateway and the raw
// of the policy it was handed.
func (s *scriptedChecker) CheckGatewayPolicy(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, policy *v1alpha1.KrakenDBackendPolicy,
) (configcheck.Verdict, error) {
	v, err := s.next(ctx, "gateway+policy", nil)
	s.args[len(s.args)-1] = gw.Namespace + "/" + gw.Name + ":" + rawOf(policy)
	return v, err
}

// LintPolicy records "policy:raw" in args.
func (s *scriptedChecker) LintPolicy(
	ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy,
) (configcheck.Verdict, error) {
	v, err := s.next(ctx, "policy", nil)
	s.args[len(s.args)-1] = "policy:" + rawOf(policy)
	return v, err
}

// CheckPolicy records "policy:raw" in args.
func (s *scriptedChecker) CheckPolicy(
	ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy, memo configcheck.Memo,
) (configcheck.Verdict, error) {
	s.memos = append(s.memos, memo != nil)
	v, err := s.next(ctx, "policy", nil)
	s.args[len(s.args)-1] = "policy:" + rawOf(policy)
	return v, err
}

// CheckGroup records "ns/gateway:raw" in args: the gateway and the raw of the
// policy override, "-" for none.
func (s *scriptedChecker) CheckGroup(
	ctx context.Context, g configcheck.Group, memo configcheck.Memo,
) (configcheck.Verdict, error) {
	s.gateways = append(s.gateways, string(g.Gateway.Spec.Edition)+"/"+g.Gateway.Spec.Config.Timeout)
	s.memos = append(s.memos, memo != nil)
	v, err := s.next(ctx, "group", g.Endpoints)
	raw := "-"
	if g.Override != nil {
		raw = rawOf(g.Override)
	}
	s.args[len(s.args)-1] = g.Gateway.Namespace + "/" + g.Gateway.Name + ":" + raw
	return v, err
}

// rawOf is the raw of policy as text, "-" for none.
func rawOf(policy *v1alpha1.KrakenDBackendPolicy) string {
	if policy.Spec.Raw == nil {
		return "-"
	}
	return string(policy.Spec.Raw.Raw)
}

// failing is a verdict that blames entry index of default/ep.
func failing(ep string, index int, msg string) configcheck.Verdict {
	return configcheck.Verdict{Findings: []configcheck.Finding{{
		Endpoint: types.NamespacedName{Namespace: "default", Name: ep}, Index: index, Message: msg,
	}}}
}

// describe lists the entries of eps as "ns/name[METHOD /path ...]", "-" for none.
func describe(eps []v1alpha1.KrakenDEndpoint) string {
	if len(eps) == 0 {
		return "-"
	}
	var out []string
	for _, ep := range eps {
		var entries []string
		for _, e := range ep.Spec.Endpoints {
			entries = append(entries, e.Method+" "+e.Endpoint)
		}
		out = append(out, ep.Namespace+"/"+ep.Name+"["+strings.Join(entries, " ")+"]")
	}
	return strings.Join(out, ",")
}
