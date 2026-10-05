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
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

// scriptedChecker answers each check with the next verdict of its script (OK
// once the script runs out), or with err, and records which checks ran and
// how much of its deadline each call had left.
type scriptedChecker struct {
	verdicts  []configcheck.Verdict
	err       error
	calls     []string
	deadlines []time.Duration
}

func (s *scriptedChecker) next(ctx context.Context, call string) (configcheck.Verdict, error) {
	s.calls = append(s.calls, call)
	if d, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, time.Until(d))
	} else {
		s.deadlines = append(s.deadlines, 0)
	}
	if s.err != nil {
		return configcheck.Verdict{}, s.err
	}
	if len(s.verdicts) == 0 {
		return configcheck.Verdict{OK: true}, nil
	}
	v := s.verdicts[0]
	s.verdicts = s.verdicts[1:]
	return v, nil
}

func (s *scriptedChecker) CheckGateway(
	ctx context.Context, _ *v1alpha1.KrakenDGateway, replace []v1alpha1.KrakenDEndpoint,
) (configcheck.Verdict, error) {
	if len(replace) > 0 {
		return s.next(ctx, "gateway+candidate")
	}
	return s.next(ctx, "gateway")
}

func (s *scriptedChecker) CheckIsolated(
	ctx context.Context, _ *v1alpha1.KrakenDGateway, _ []v1alpha1.KrakenDEndpoint,
) (configcheck.Verdict, error) {
	return s.next(ctx, "isolated")
}

// failing is a verdict that blames entry index of default/ep.
func failing(ep string, index int, msg string) configcheck.Verdict {
	return configcheck.Verdict{Findings: []configcheck.Finding{{
		Endpoint: types.NamespacedName{Namespace: "default", Name: ep}, Index: index, Message: msg,
	}}}
}
