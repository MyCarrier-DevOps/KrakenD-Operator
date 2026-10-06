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
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// PolicyValidator validates KrakenDBackendPolicy resources.
type PolicyValidator struct {
	client.Client
	// Checker renders the policy alone and in every gateway that uses it.
	Checker ConfigChecker
}

// ValidateCreate validates a new KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	policy, ok := obj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", obj)
	}
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()
	return checkPolicyRender(ctx, v.Client, v.Checker, nil, policy)
}

// ValidateUpdate validates an updated KrakenDBackendPolicy. An update that
// leaves the spec alone, such as the protection finalizer, is never validated.
func (v *PolicyValidator) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	if terminatingWithUnchangedSpec(oldObj, newObj) {
		return nil, nil
	}
	policy, ok := newObj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", newObj)
	}
	old, ok := oldObj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", oldObj)
	}
	if equality.Semantic.DeepEqual(old.Spec, policy.Spec) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()
	return checkPolicyRender(ctx, v.Client, v.Checker, old, policy)
}

// ValidateDelete is required by admission.CustomValidator. The policy webhook is
// not registered for DELETE: the policy-protection finalizer keeps a referenced
// policy until nothing references it.
func (v *PolicyValidator) ValidateDelete(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// maxPolicyWarnings is how many gateways that already fail a policy change
// names in warnings; the rest are counted in one more.
const maxPolicyWarnings = 5

// policyWarningBytes bounds the warnings of one policy write together. Past
// 4096 characters in all the API server cuts every warning of the response to
// 256, which drops each one's count of findings left out.
const policyWarningBytes = 4096

// countWarningBytes is room kept for the closing warning that counts the
// gateways left out.
const countWarningBytes = 96

// policyWarningLimit bounds one gateway's warning, so that maxPolicyWarnings of
// them and the closing count fit in policyWarningBytes.
const policyWarningLimit = (policyWarningBytes - countWarningBytes) / maxPolicyWarnings

// policySummaryLimit bounds the findings a gateway's warning quotes: the
// rest of the warning, the gateway's name and the cut marker, fits in what
// policyWarningLimit leaves.
const policySummaryLimit = policyWarningLimit / 2

// checkPolicyRender validates policy on its own and in every gateway that
// renders it. It rejects a request only for a pass-to-fail change: a policy
// that already failed alone (old) is judged by its gateways, and a gateway
// already failing without the change gets a warning instead.
func checkPolicyRender(
	ctx context.Context, c client.Reader, chk ConfigChecker, old, policy *v1alpha1.KrakenDBackendPolicy,
) (admission.Warnings, error) {
	if err := lintPolicyAlone(ctx, chk, old, policy); err != nil {
		return nil, err
	}
	gateways, err := gatewaysUsing(ctx, c, policy)
	if err != nil {
		return nil, unavailable(err)
	}
	// krakend check accepts Enterprise-only namespaces, and KrakenD CE then
	// ignores them silently; only a new or changed raw is judged.
	var drops []renderer.CEDrop
	if old == nil || !equality.Semantic.DeepEqual(old.Spec.Raw, policy.Spec.Raw) {
		drops = renderer.EEOnlyNamespacesIn(policy.Spec.Raw, renderer.LevelBackend)
	}
	var errs field.ErrorList
	var warnings admission.Warnings
	omitted, unwarned := 0, 0
	// cause records a gateway the policy cannot go to; past maxEntryCauses the
	// rest are only counted.
	cause := func(e *field.Error) {
		if len(errs) < maxEntryCauses {
			errs = append(errs, e)
			return
		}
		omitted++
	}
	for i := range gateways {
		gw := &gateways[i]
		if gw.Spec.Edition == v1alpha1.EditionCE && len(drops) > 0 {
			cause(field.Invalid(field.NewPath("spec", "raw"), describeDrops(drops),
				fmt.Sprintf("Enterprise-only extra_config: gateway %s/%s runs CE, which ignores it silently",
					gw.Namespace, gw.Name)))
			continue
		}
		w, err := ratchetRender(ctx, renderChecks{
			after:  bindPolicyCheck(chk.CheckGatewayPolicy, gw, policy),
			before: policyBaseline(chk, gw, old),
		},
			func(after configcheck.Verdict) error {
				cause(field.Invalid(field.NewPath("spec"), field.OmitValueType{},
					fmt.Sprintf("breaks gateway %s/%s: %s", gw.Namespace, gw.Name, after.Summary(warningLimit))))
				return errPolicyBreaksGateway
			},
			func(before configcheck.Verdict) string {
				summary := shownSummary(before, policy.Namespace, policySummaryLimit)
				return truncate(fmt.Sprintf("gateway %s/%s already fails validation: %s",
					gw.Namespace, gw.Name, summary), policyWarningLimit)
			})
		if err != nil && !errors.Is(err, errPolicyBreaksGateway) {
			return nil, err
		}
		switch {
		case len(w) == 0:
		case len(warnings) < maxPolicyWarnings:
			warnings = append(warnings, w...)
		default:
			unwarned++
		}
	}
	if omitted > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec"), field.OmitValueType{},
			fmt.Sprintf("the policy is also refused on %d more gateways", omitted)))
	}
	if unwarned > 0 {
		warnings = append(warnings, fmt.Sprintf("%d more gateways already fail validation", unwarned))
	}
	return warnings, invalid("KrakenDBackendPolicy", policy.Name, errs)
}

// errPolicyBreaksGateway tells checkPolicyRender's loop that a gateway's
// ratchet denied the policy; the cause itself is already collected.
var errPolicyBreaksGateway = errors.New("policy breaks gateway")

// policyBaseline is the check of gw without the change: with the stored policy
// as the update read it (old), not whatever the cache holds now. A create has
// no stored policy, so gw is rendered as it stands.
func policyBaseline(
	chk ConfigChecker, gw *v1alpha1.KrakenDGateway, old *v1alpha1.KrakenDBackendPolicy,
) func(context.Context) (configcheck.Verdict, error) {
	if old == nil {
		return bindCheck(chk.CheckGateway, gw, nil)
	}
	return bindPolicyCheck(chk.CheckGatewayPolicy, gw, old)
}

// bindPolicyCheck fixes the gateway and the policy a check runs on.
func bindPolicyCheck(
	run func(context.Context, *v1alpha1.KrakenDGateway, *v1alpha1.KrakenDBackendPolicy) (configcheck.Verdict, error),
	gw *v1alpha1.KrakenDGateway, policy *v1alpha1.KrakenDBackendPolicy,
) func(context.Context) (configcheck.Verdict, error) {
	return func(ctx context.Context) (configcheck.Verdict, error) { return run(ctx, gw, policy) }
}

// lintPolicyAlone refuses policy when it fails krakend check on its own,
// unless the stored policy (old, nil on a create) already failed too: then its
// gateways decide.
func lintPolicyAlone(
	ctx context.Context, chk ConfigChecker, old, policy *v1alpha1.KrakenDBackendPolicy,
) error {
	alone, err := chk.LintPolicy(ctx, policy)
	if err != nil || alone.OK {
		return checkErr(err)
	}
	if old != nil {
		oldAlone, err := chk.LintPolicy(ctx, old)
		if err != nil || !oldAlone.OK {
			return checkErr(err)
		}
	}
	return invalid("KrakenDBackendPolicy", policy.Name, field.ErrorList{field.Invalid(
		field.NewPath("spec"), field.OmitValueType{}, "fails krakend check on its own: "+messages(alone))})
}

// messages joins a verdict's messages without their locations: a policy's
// lint findings point into a synthetic endpoint the user never wrote.
func messages(v configcheck.Verdict) string {
	parts := make([]string, 0, len(v.Findings))
	for _, f := range v.Findings {
		parts = append(parts, f.Message)
	}
	return truncate(strings.Join(parts, "; "), warningLimit)
}

// gatewaysUsing returns the gateways of the endpoints that reference policy,
// each once, sorted by namespace/name. Gateways that no longer exist are
// skipped.
func gatewaysUsing(
	ctx context.Context, c client.Reader, policy *v1alpha1.KrakenDBackendPolicy,
) ([]v1alpha1.KrakenDGateway, error) {
	var eps v1alpha1.KrakenDEndpointList
	if err := c.List(ctx, &eps, client.UnsafeDisableDeepCopy,
		client.MatchingFields{fieldindex.EndpointPolicy: policy.Namespace + "/" + policy.Name}); err != nil {
		return nil, fmt.Errorf("listing endpoints that reference policy %s/%s: %w", policy.Namespace, policy.Name, err)
	}
	keys := map[types.NamespacedName]struct{}{}
	for i := range eps.Items {
		ep := &eps.Items[i]
		keys[types.NamespacedName{
			Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace), Name: ep.Spec.GatewayRef.Name,
		}] = struct{}{}
	}
	sorted := make([]types.NamespacedName, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	gateways := make([]v1alpha1.KrakenDGateway, 0, len(sorted))
	for _, key := range sorted {
		var gw v1alpha1.KrakenDGateway
		if err := c.Get(ctx, key, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("getting gateway %s: %w", key, err)
		}
		gateways = append(gateways, gw)
	}
	return gateways, nil
}
