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
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

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
	// Memo remembers recent config verdicts across requests. Nil remembers
	// nothing.
	Memo configcheck.Memo
}

// ValidateCreate validates a new KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	policy, ok := obj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", obj)
	}
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()
	return checkPolicyRender(ctx, v.Client, v.Checker, v.Memo, nil, policy)
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
	return checkPolicyRender(ctx, v.Client, v.Checker, v.Memo, old, policy)
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

// checkPolicyRender validates policy on its own (lintPolicyAlone) and against
// the endpoints of every gateway that use it. Every gateway is screened first
// (screenPolicyUse), and only then are the endpoints a change breaks named,
// gateway by gateway (judgePolicyUse), so naming the endpoints of one gateway
// cannot spend the time another gateway's checks need. A denial names
// endpoints and quotes none of them. A gateway that could not be checked makes
// the request a 500 unless another gateway already refuses it.
func checkPolicyRender(ctx context.Context, c client.Reader, chk ConfigChecker, memo configcheck.Memo,
	old, policy *v1alpha1.KrakenDBackendPolicy) (admission.Warnings, error) {
	if err := lintPolicyAlone(ctx, chk, memo, old, policy); err != nil {
		return nil, err
	}
	uses, err := gatewaysUsing(ctx, c, policy)
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
	omitted, unwarned, unchecked := 0, 0, 0
	var stopped error
	// cause records a gateway the policy cannot go to; past maxEntryCauses the
	// rest are only counted.
	cause := func(e *field.Error) {
		if len(errs) < maxEntryCauses {
			errs = append(errs, e)
			return
		}
		omitted++
	}
	screened := make([]policyUse, 0, len(uses))
	for i := range uses {
		gw := &uses[i].gateway
		if gw.Spec.Edition == v1alpha1.EditionCE && len(drops) > 0 {
			cause(field.Invalid(field.NewPath("spec", "raw"), describeDrops(drops),
				fmt.Sprintf("Enterprise-only extra_config: gateway %s/%s runs CE, which ignores it silently",
					gw.Namespace, gw.Name)))
			continue
		}
		screened = append(screened, screenPolicyUse(ctx, chk, memo, gw, uses[i].endpoints, policy))
	}
	for _, use := range screened {
		broken, warning, err := judgePolicyUse(ctx, chk, memo, use, old, policy)
		if err != nil {
			unchecked++
			if stopped == nil {
				stopped = err
			}
			continue
		}
		if broken != "" {
			cause(field.Invalid(field.NewPath("spec"), field.OmitValueType{}, truncate(broken, warningLimit)))
		}
		switch {
		case warning == "":
		case len(warnings) < maxPolicyWarnings:
			warnings = append(warnings, truncate(warning, policyWarningLimit))
		default:
			unwarned++
		}
	}
	if len(errs) == 0 && stopped != nil {
		return nil, checkErr(stopped)
	}
	if unchecked > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec"), field.OmitValueType{},
			fmt.Sprintf("%d more gateways that use the policy could not be checked", unchecked)))
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

// policyUse is a gateway's screening of a policy write (screenPolicyUse).
type policyUse struct {
	gateway *v1alpha1.KrakenDGateway
	// served are the gateway's endpoints that use the policy and that it
	// serves (servedEndpoints).
	served []v1alpha1.KrakenDEndpoint
	// failed says the group check of served with the write failed, and
	// suspects are those of served it did not judge (suspectsOf).
	failed   bool
	suspects []v1alpha1.KrakenDEndpoint
	// warning says the gateway's root fails on its own, so nothing is judged.
	warning string
	// err is a check that could not run: the gateway is not judged.
	err error
}

// screenPolicyUse runs the checks of a policy write on gw that do not depend
// on how many endpoints it breaks: gw's root alone, then the root with the
// endpoints that use the policy and that gw serves, with policy in place of
// the stored one.
func screenPolicyUse(ctx context.Context, chk ConfigChecker, memo configcheck.Memo, gw *v1alpha1.KrakenDGateway,
	endpoints []v1alpha1.KrakenDEndpoint, policy *v1alpha1.KrakenDBackendPolicy) policyUse {
	use := policyUse{gateway: gw}
	ceFallback := configcheck.CEFallback(gw)
	root, err := chk.CheckRoot(ctx, configcheck.Root{Gateway: gw, CEFallback: ceFallback}, memo)
	switch {
	case err != nil:
		use.err = err
		return use
	case !root.OK:
		use.warning = fmt.Sprintf("gateway %s/%s fails validation on its own, so this policy was not "+
			"checked against it", gw.Namespace, gw.Name)
		return use
	}
	use.served = servedEndpoints(endpoints)
	if len(use.served) == 0 {
		return use
	}
	group, err := chk.CheckGroup(ctx, configcheck.Group{
		Gateway: gw, Endpoints: use.served, Override: policy, CEFallback: ceFallback,
	}, memo)
	if err != nil {
		use.err = err
		return use
	}
	use.failed, use.suspects = !group.OK, suspectsOf(group, use.served)
	return use
}

// judgePolicyUse names the endpoints of a screened gateway that policy
// breaks: each suspect is checked on its own with policy and, when that
// fails, with the stored policy (old; nil on a create, when nothing rendered
// them with it). One that fails only with policy is broken, and any one makes
// the returned cause, which names endpoints and quotes none. When every
// suspect that fails failed with the stored policy too, the write only draws
// a warning. When the group failed but no suspect fails on its own, the group
// with the stored policy tells whether failing together is the write's doing.
// A check that cannot run is returned as err, unless a broken endpoint was
// already found: that denial stands.
func judgePolicyUse(ctx context.Context, chk ConfigChecker, memo configcheck.Memo, use policyUse,
	old, policy *v1alpha1.KrakenDBackendPolicy) (cause, warning string, err error) {
	if use.err != nil || use.warning != "" {
		return "", use.warning, use.err
	}
	gw := use.gateway
	ceFallback := configcheck.CEFallback(gw)
	var was *configcheck.EndpointUnit
	if old != nil {
		was = &configcheck.EndpointUnit{Gateway: gw, Override: old, CEFallback: ceFallback}
	}
	s := failingEndpoints(ctx, chk, memo,
		configcheck.EndpointUnit{Gateway: gw, Override: policy, CEFallback: ceFallback}, was, use.suspects)
	switch {
	case len(s.broken) > 0:
		prefix := fmt.Sprintf("breaks gateway %s/%s: ", gw.Namespace, gw.Name)
		return prefix + brokenList(s, warningLimit-len(prefix)), "", nil
	case s.stopped != nil:
		return "", "", s.stopped
	case s.already:
		return "", fmt.Sprintf("gateway %s/%s: endpoints that use this policy already fail validation with "+
			"the stored policy", gw.Namespace, gw.Name), nil
	case !use.failed:
		return "", "", nil
	}
	if old != nil {
		before, err := chk.CheckGroup(ctx, configcheck.Group{
			Gateway: gw, Endpoints: use.served, Override: old, CEFallback: ceFallback,
		}, memo)
		if err != nil {
			return "", "", err
		}
		if !before.OK {
			return "", fmt.Sprintf("gateway %s/%s: the endpoints that use this policy already fail validation "+
				"together with the stored policy", gw.Namespace, gw.Name), nil
		}
	}
	return fmt.Sprintf("breaks gateway %s/%s: with this change the endpoints that use it fail validation "+
		"together, though each passes on its own", gw.Namespace, gw.Name), "", nil
}

// lintPolicyAlone refuses policy when it fails krakend check on its own,
// quoting its own output, unless the stored policy (old, nil on a create)
// already failed too: then its endpoints decide.
func lintPolicyAlone(ctx context.Context, chk ConfigChecker, memo configcheck.Memo,
	old, policy *v1alpha1.KrakenDBackendPolicy) error {
	alone, err := chk.CheckPolicy(ctx, policy, memo)
	if err != nil || alone.OK {
		return checkErr(err)
	}
	if old != nil {
		oldAlone, err := chk.CheckPolicy(ctx, old, memo)
		if err != nil || !oldAlone.OK {
			return checkErr(err)
		}
	}
	return invalid("KrakenDBackendPolicy", policy.Name, field.ErrorList{field.Invalid(
		field.NewPath("spec"), field.OmitValueType{}, "fails krakend check on its own: "+alone.Excerpt(warningLimit))})
}

// gatewayUse is a gateway that renders a policy, and the endpoints of it that
// reference the policy.
type gatewayUse struct {
	gateway   v1alpha1.KrakenDGateway
	endpoints []v1alpha1.KrakenDEndpoint
}

// gatewaysUsing returns the gateways of the endpoints that reference policy,
// each once with those endpoints, sorted by namespace/name. Gateways that no
// longer exist are skipped.
func gatewaysUsing(
	ctx context.Context, c client.Reader, policy *v1alpha1.KrakenDBackendPolicy,
) ([]gatewayUse, error) {
	var eps v1alpha1.KrakenDEndpointList
	if err := c.List(ctx, &eps, client.UnsafeDisableDeepCopy,
		client.MatchingFields{fieldindex.EndpointPolicy: policy.Namespace + "/" + policy.Name}); err != nil {
		return nil, fmt.Errorf("listing endpoints that reference policy %s/%s: %w", policy.Namespace, policy.Name, err)
	}
	byGateway := map[types.NamespacedName][]v1alpha1.KrakenDEndpoint{}
	for i := range eps.Items {
		ep := eps.Items[i]
		key := types.NamespacedName{
			Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace), Name: ep.Spec.GatewayRef.Name,
		}
		byGateway[key] = append(byGateway[key], ep)
	}
	keys := slices.SortedFunc(maps.Keys(byGateway), func(a, b types.NamespacedName) int {
		return cmp.Compare(a.String(), b.String())
	})
	uses := make([]gatewayUse, 0, len(keys))
	for _, key := range keys {
		var gw v1alpha1.KrakenDGateway
		if err := c.Get(ctx, key, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("getting gateway %s: %w", key, err)
		}
		uses = append(uses, gatewayUse{gateway: gw, endpoints: byGateway[key]})
	}
	return uses, nil
}
