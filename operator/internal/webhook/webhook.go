/*
Copyright 2026.

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

// Package webhook implements validating admission webhooks for KrakenD CRDs.
package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// GatewayValidator validates KrakenDGateway resources.
type GatewayValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDGateway. There is no "old" object on
// Create, so the runAsUser:0 ratchet (review id 3807285627, #2) never
// applies here — a brand-new CR gets the hard reject unconditionally.
func (v *GatewayValidator) ValidateCreate(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	gw, ok := obj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", obj)
	}
	warnings, err := v.validate(gw, nil)
	return warnings, err
}

// ValidateUpdate validates an updated KrakenDGateway. review id 3807285627
// (#2): the old (stored) object is now threaded through to validate so the
// runAsUser:0 reject can be RATCHETED — a CR accepted by an older operator
// version (before the round-2 reject existed) must not start failing every
// unrelated update just because ValidateUpdate re-validates the whole spec.
func (v *GatewayValidator) ValidateUpdate(
	_ context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	gw, ok := newObj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", newObj)
	}
	old, ok := oldObj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", oldObj)
	}
	warnings, err := v.validate(gw, old)
	return warnings, err
}

// ValidateDelete is a no-op for gateways.
func (v *GatewayValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

// validate runs all admission checks for gw. old is the previously-stored
// object on an Update (nil on Create) — see validatePostRestartJob's
// ratchet handling (review id 3807285627, #2).
func (v *GatewayValidator) validate(gw, old *v1alpha1.KrakenDGateway) (admission.Warnings, error) {
	var errs field.ErrorList
	var warnings admission.Warnings

	if gw.Spec.Edition == v1alpha1.EditionEE {
		if gw.Spec.License == nil ||
			(!gw.Spec.License.ExternalSecret.Enabled && gw.Spec.License.SecretRef == nil) {
			errs = append(errs, field.Required(
				field.NewPath("spec", "license"),
				"edition EE requires license.externalSecret.enabled or license.secretRef",
			))
		}
	}

	if gw.Spec.Edition == v1alpha1.EditionCE && gw.Spec.License != nil {
		if gw.Spec.License.ExternalSecret.Enabled || gw.Spec.License.SecretRef != nil {
			errs = append(errs, field.Forbidden(
				field.NewPath("spec", "license"),
				"CE edition does not require license configuration",
			))
		}
	}

	if gw.Spec.License != nil &&
		gw.Spec.License.ExternalSecret.Enabled && gw.Spec.License.SecretRef != nil {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "license"),
			"both",
			"externalSecret and secretRef are mutually exclusive",
		))
	}

	if gw.Spec.OpenAPI != nil && gw.Spec.OpenAPI.Enabled {
		gwPort := resources.GatewayPort(gw)
		oaPort := resources.OpenAPIPort(gw)
		if oaPort == gwPort {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "openapi", "port"),
				oaPort,
				"openapi port must differ from the gateway listen port",
			))
		}
	}

	if gw.Spec.Plugins != nil {
		pvcCount := 0
		for _, src := range gw.Spec.Plugins.Sources {
			if src.PersistentVolumeClaimRef != nil {
				pvcCount++
			}
		}
		if pvcCount > 1 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "plugins", "sources"),
				pvcCount,
				"only one PVC plugin source is supported",
			))
		}
	}

	if gw.Spec.PostRestartJob != nil && gw.Spec.PostRestartJob.Enabled {
		var oldPRJ *v1alpha1.PostRestartJobSpec
		// Only a previously-ENABLED spec can have been grandfathered by an
		// older operator; a disabled one was never validated, so enabling it
		// must get the full check rather than ratcheting off its stored value.
		if old != nil && old.Spec.PostRestartJob != nil && old.Spec.PostRestartJob.Enabled {
			oldPRJ = old.Spec.PostRestartJob
		}
		prjErrs, prjWarnings := validatePostRestartJob(gw.Spec.PostRestartJob, oldPRJ)
		errs = append(errs, prjErrs...)
		warnings = append(warnings, prjWarnings...)
	}

	return warnings, errs.ToAggregate()
}

// validatePostRestartJob validates spec.postRestartJob when enabled. Split
// out of GatewayValidator.validate to keep that function's cyclomatic
// complexity in check (gocyclo) as this block grew with round-2 review
// fixes (ids 3805157408 #1, 3805157457 #5, 3805157497 #9) and round-3
// fixes (ids 3807285627 #2, 3807285645 #6). old is the previously-stored
// spec on an Update (nil on Create).
func validatePostRestartJob(
	prj *v1alpha1.PostRestartJobSpec, old *v1alpha1.PostRestartJobSpec,
) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings

	if prj.Script == "" {
		errs = append(errs, field.Required(
			field.NewPath("spec", "postRestartJob", "script"),
			"script is required when postRestartJob is enabled",
		))
	}

	if w := validatePostRestartWorkingDir(prj); w != "" {
		warnings = append(warnings, w)
	}

	errs = append(errs, validatePostRestartRunAsRoot(prj, old)...)

	// review id 3805157457 (#5): a negative tmpSizeLimit is nonsensical
	// (emptyDir SizeLimit is a cap, not a delta) and, more importantly,
	// silently means "no cap" to the kubelet in a way that looks like the
	// opposite of the user's intent — reject it outright rather than let it
	// pass through and surprise the user later.
	if prj.TmpSizeLimit != nil && prj.TmpSizeLimit.Sign() < 0 {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "postRestartJob", "tmpSizeLimit"),
			prj.TmpSizeLimit.String(),
			"must not be negative; use \"0\" for no cap",
		))
	}

	return errs, warnings
}

// validatePostRestartRunAsRoot rejects a postRestartJob spec that will hang
// the Job pod Pending (CreateContainerConfigError) until
// activeDeadlineSeconds expires, because the kubelet's EFFECTIVE
// {runAsUser, runAsNonRoot} pair for the container resolves to {0, true}.
//
// Review id 3807285645 (#6): collapses what were three separate outcomes
// into one rule. Previously:
//  1. container-scope runAsUser: 0 (with runAsNonRoot unset everywhere) —
//     rejected at admission (review id 3805157408, round 2 #1).
//  2. pod-scope runAsUser: 0 (with runAsNonRoot unset everywhere) — not
//     rejected, but self-healed at build time: job.go's
//     mergePodSecurityContext drops the inherited runAsNonRoot: true
//     default specifically when the user's PodSecurityContext sets
//     RunAsUser: 0 and leaves RunAsNonRoot unset. Kept as-is here — see
//     "Keep the builder fixup" below.
//  3. pod-scope runAsUser: 0 WITH an explicit runAsNonRoot: true (container
//     or pod scope) — the pod-scope hole: neither rejected NOR self-healed
//     (the builder fixup only triggers when RunAsNonRoot is unset), so the
//     pod hangs.
//
// The EFFECTIVE uid is computed exactly as the kubelet resolves it: the
// container-level value wins when set, otherwise the pod-level value
// applies. When that effective uid is 0, we reject:
//   - unconditionally, when the uid0 came from the CONTAINER scope — the
//     builder fixup only inspects PodSecurityContext, so a container-level
//     override is never self-healed, and this preserves outcome 1 exactly
//     (unset runAsNonRoot is presumed to inherit the hardened true
//     default, same as before);
//   - only when runAsNonRoot is EXPLICITLY true somewhere (container or
//     pod scope), when the uid0 came from the POD scope only — this closes
//     the outcome-3 hole while leaving outcome 2 (fully unset) alone, since
//     the builder's own fixup already makes that combination safe at
//     runtime. "Keep the builder fixup for the pod-unset case as
//     defense-in-depth" (per review) — it remains the ONLY protection for
//     that combination on webhook-bypass paths (webhooks.enabled: false,
//     cert-manager absent, webhook downtime — see docs/upgrade-guide.md).
//
// An explicit runAsNonRoot: false, at either scope, is always an
// acknowledged opt-out and short-circuits the whole check.
//
// Review id 3807285627 (#2): the check is skipped entirely (ratcheted) on
// an Update when none of the four fields it inspects
// (securityContext.{runAsUser,runAsNonRoot}, podSecurityContext.
// runAsNonRoot — podSecurityContext.runAsUser is intentionally included
// too, see securityContextRunAsFieldsUnchanged) changed from the stored
// spec — a CR accepted by an older operator version (before this reject
// existed) must not start failing on every unrelated update.
func validatePostRestartRunAsRoot(prj, old *v1alpha1.PostRestartJobSpec) field.ErrorList {
	if old != nil && securityContextRunAsFieldsUnchanged(prj, old) {
		return nil
	}

	uid0, fromContainer := effectiveRunAsRoot(prj.SecurityContext, prj.PodSecurityContext)
	if !uid0 {
		return nil
	}

	containerOptsOut := prj.SecurityContext != nil &&
		prj.SecurityContext.RunAsNonRoot != nil && !*prj.SecurityContext.RunAsNonRoot
	podOptsOut := prj.PodSecurityContext != nil &&
		prj.PodSecurityContext.RunAsNonRoot != nil && !*prj.PodSecurityContext.RunAsNonRoot
	if containerOptsOut || podOptsOut {
		return nil
	}

	containerAssertsTrue := prj.SecurityContext != nil &&
		prj.SecurityContext.RunAsNonRoot != nil && *prj.SecurityContext.RunAsNonRoot
	podAssertsTrue := prj.PodSecurityContext != nil &&
		prj.PodSecurityContext.RunAsNonRoot != nil && *prj.PodSecurityContext.RunAsNonRoot

	if !fromContainer && !containerAssertsTrue && !podAssertsTrue {
		// Pod-scope uid0, runAsNonRoot unset everywhere: self-healed by
		// job.go's mergePodSecurityContext fixup — not rejected here.
		return nil
	}

	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "postRestartJob", "securityContext", "runAsUser"),
		int64(0),
		"runAsUser: 0 conflicts with the hardened runAsNonRoot: true default (kubelet "+
			"pod-level runAsNonRoot defaults to true and is inherited unless overridden); "+
			"the Job pod will hang Pending (CreateContainerConfigError) until "+
			"activeDeadlineSeconds expires. Also set "+
			"spec.postRestartJob.podSecurityContext.runAsNonRoot: false (or "+
			"spec.postRestartJob.securityContext.runAsNonRoot: false) to acknowledge "+
			"running as root.",
	)}
}

// effectiveRunAsRoot reports whether the kubelet-resolved effective
// runAsUser for the post-restart container is 0, and whether that value
// came from the container-scope securityContext (as opposed to falling
// back to the pod-scope podSecurityContext) — the container value wins
// when set, exactly as the kubelet resolves per-field inheritance.
func effectiveRunAsRoot(
	container *corev1.SecurityContext, pod *corev1.PodSecurityContext,
) (isRoot, fromContainer bool) {
	if container != nil && container.RunAsUser != nil {
		return *container.RunAsUser == 0, true
	}
	if pod != nil && pod.RunAsUser != nil {
		return *pod.RunAsUser == 0, false
	}
	return false, false
}

// securityContextRunAsFieldsUnchanged reports whether the fields
// validatePostRestartRunAsRoot inspects are identical between the new and
// old postRestartJob spec — the ratchet condition for review id 3807285627
// (#2). old may be nil (postRestartJob newly added on this update); new is
// never nil (validatePostRestartJob is only called when enabled).
func securityContextRunAsFieldsUnchanged(newPRJ, old *v1alpha1.PostRestartJobSpec) bool {
	var oldContainer *corev1.SecurityContext
	var oldPod *corev1.PodSecurityContext
	if old != nil {
		oldContainer = old.SecurityContext
		oldPod = old.PodSecurityContext
	}
	return int64PtrEqual(securityContextRunAsUser(newPRJ.SecurityContext), securityContextRunAsUser(oldContainer)) &&
		boolPtrEqual(securityContextRunAsNonRoot(newPRJ.SecurityContext), securityContextRunAsNonRoot(oldContainer)) &&
		int64PtrEqual(podSecurityContextRunAsUser(newPRJ.PodSecurityContext), podSecurityContextRunAsUser(oldPod)) &&
		boolPtrEqual(podSecurityContextRunAsNonRoot(newPRJ.PodSecurityContext), podSecurityContextRunAsNonRoot(oldPod))
}

func securityContextRunAsUser(sc *corev1.SecurityContext) *int64 {
	if sc == nil {
		return nil
	}
	return sc.RunAsUser
}

func securityContextRunAsNonRoot(sc *corev1.SecurityContext) *bool {
	if sc == nil {
		return nil
	}
	return sc.RunAsNonRoot
}

func podSecurityContextRunAsUser(psc *corev1.PodSecurityContext) *int64 {
	if psc == nil {
		return nil
	}
	return psc.RunAsUser
}

func podSecurityContextRunAsNonRoot(psc *corev1.PodSecurityContext) *bool {
	if psc == nil {
		return nil
	}
	return psc.RunAsNonRoot
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// validatePostRestartWorkingDir returns a non-empty warning string when
// spec.postRestartJob.workingDir is overridden outside the writable /tmp
// emptyDir mount while readOnlyRootFilesystem is effectively true (review
// id 3804144425 #4) — the container still starts fine, so this is not
// otherwise caught until the script runs. A warning (not a hard error,
// since it's a legitimate configuration if the image provides its own
// writable directory there) nudges the user toward the documented escape
// hatch instead.
func validatePostRestartWorkingDir(prj *v1alpha1.PostRestartJobSpec) string {
	if prj.WorkingDir == "" ||
		prj.WorkingDir == resources.PostRestartTmpMountPath ||
		strings.HasPrefix(prj.WorkingDir, resources.PostRestartTmpMountPath+"/") {
		return ""
	}

	effectiveROFS := true
	if prj.SecurityContext != nil && prj.SecurityContext.ReadOnlyRootFilesystem != nil {
		effectiveROFS = *prj.SecurityContext.ReadOnlyRootFilesystem
	}
	if !effectiveROFS {
		return ""
	}

	// review id 3805157497 (#9): "use an absolute path under /tmp" is not a
	// sufficient remedy on its own — readOnlyRootFilesystem applies to the
	// ENTIRE container filesystem, not just workingDir. Moving the script's
	// CWD under /tmp only fixes relative-path writes issued from the
	// working directory; any absolute-path write elsewhere on the rootfs
	// (e.g. /var, /etc, npm's global prefix) still fails with EROFS
	// regardless of workingDir.
	return fmt.Sprintf(
		"spec.postRestartJob.workingDir %q is outside the writable %s emptyDir mount, "+
			"and readOnlyRootFilesystem is effectively true — relative-path writes in "+
			"your script will fail with EROFS. Note readOnlyRootFilesystem applies to the "+
			"whole container filesystem, not just workingDir: only %s (or a subdirectory "+
			"of it) is writable, so any absolute-path write elsewhere on the rootfs will "+
			"still fail even after moving workingDir there. Confine all script writes to "+
			"%s, or set spec.postRestartJob.securityContext.readOnlyRootFilesystem: false "+
			"deliberately.",
		prj.WorkingDir, resources.PostRestartTmpMountPath,
		resources.PostRestartTmpMountPath, resources.PostRestartTmpMountPath,
	)
}

// EndpointValidator validates KrakenDEndpoint resources.
type EndpointValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDEndpoint.
func (v *EndpointValidator) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDEndpoint, got %T", obj)
	}
	errs, warnings := v.validate(ctx, ep)
	return warnings, errs.ToAggregate()
}

// ValidateUpdate validates an updated KrakenDEndpoint.
func (v *EndpointValidator) ValidateUpdate(
	ctx context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	ep, ok := newObj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDEndpoint, got %T", newObj)
	}
	errs, warnings := v.validate(ctx, ep)
	return warnings, errs.ToAggregate()
}

// ValidateDelete is a no-op for endpoints.
func (v *EndpointValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

func (v *EndpointValidator) validate(
	ctx context.Context,
	ep *v1alpha1.KrakenDEndpoint,
) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings

	gw := &v1alpha1.KrakenDGateway{}
	gwNS := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace)
	if err := v.Get(ctx, types.NamespacedName{
		Name:      ep.Spec.GatewayRef.Name,
		Namespace: gwNS,
	}, gw); err != nil {
		if apierrors.IsNotFound(err) {
			refPath := field.NewPath("spec", "gatewayRef", "name")
			refValue := ep.Spec.GatewayRef.Name
			if gwNS != ep.Namespace {
				refPath = field.NewPath("spec", "gatewayRef", "namespace")
				refValue = gwNS
			}
			errs = append(errs, field.NotFound(refPath, refValue))
		} else {
			errs = append(errs, field.InternalError(
				field.NewPath("spec", "gatewayRef"),
				fmt.Errorf("looking up gateway: %w", err),
			))
			return errs, warnings
		}
	}

	for i, entry := range ep.Spec.Endpoints {
		for j, be := range entry.Backends {
			if be.PolicyRef != nil {
				policy := &v1alpha1.KrakenDBackendPolicy{}
				polNS := be.PolicyRef.ResolvedNamespace(ep.Namespace)
				if err := v.Get(ctx, types.NamespacedName{
					Name:      be.PolicyRef.Name,
					Namespace: polNS,
				}, policy); err != nil {
					policyPath := field.NewPath("spec", "endpoints").Index(i).
						Child("backends").Index(j).
						Child("policyRef")
					if apierrors.IsNotFound(err) {
						refField := "name"
						refValue := be.PolicyRef.Name
						if polNS != ep.Namespace {
							refField = "namespace"
							refValue = polNS
						}
						errs = append(errs, field.NotFound(
							policyPath.Child(refField),
							refValue,
						))
					} else {
						errs = append(errs, field.InternalError(
							policyPath,
							fmt.Errorf("looking up policy: %w", err),
						))
						return errs, warnings
					}
				}
			}
		}
	}

	// Detect duplicate (endpoint, method) pairs within this CR.
	seenPaths := make(map[string]struct{})
	for i, entry := range ep.Spec.Endpoints {
		key := entry.Method + " " + entry.Endpoint
		if _, dup := seenPaths[key]; dup {
			errs = append(errs, field.Duplicate(
				field.NewPath("spec", "endpoints").Index(i),
				key,
			))
		}
		seenPaths[key] = struct{}{}
	}

	gwKey := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace) + "/" + ep.Spec.GatewayRef.Name
	var existing v1alpha1.KrakenDEndpointList
	if err := v.List(ctx, &existing,
		client.MatchingFields{controller.EndpointGatewayIndex: gwKey},
	); err != nil {
		errs = append(errs, field.InternalError(
			field.NewPath("spec", "gatewayRef"),
			fmt.Errorf("listing endpoints for conflict check: %w", err),
		))
		return errs, warnings
	}
	for _, newEntry := range ep.Spec.Endpoints {
		for _, other := range existing.Items {
			if other.Name == ep.Name && other.Namespace == ep.Namespace {
				continue
			}
			for _, otherEntry := range other.Spec.Endpoints {
				if otherEntry.Endpoint == newEntry.Endpoint &&
					otherEntry.Method == newEntry.Method {
					warnings = append(warnings, fmt.Sprintf(
						"endpoint %s %s already exists on gateway %s "+
							"(defined by %s/%s) — conflict resolved by creationTimestamp",
						newEntry.Method, newEntry.Endpoint,
						gwKey, other.Namespace, other.Name,
					))
				}
			}
		}
	}

	return errs, warnings
}

// PolicyValidator validates KrakenDBackendPolicy resources.
type PolicyValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateCreate(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	policy, ok := obj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", obj)
	}
	return nil, validatePolicyFields(policy)
}

// ValidateUpdate validates an updated KrakenDBackendPolicy.
func (v *PolicyValidator) ValidateUpdate(
	_ context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	policy, ok := newObj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", newObj)
	}
	return nil, validatePolicyFields(policy)
}

// ValidateDelete blocks deletion if the policy is still referenced by endpoints.
func (v *PolicyValidator) ValidateDelete(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	policy, ok := obj.(*v1alpha1.KrakenDBackendPolicy)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDBackendPolicy, got %T", obj)
	}

	var endpoints v1alpha1.KrakenDEndpointList
	indexKey := policy.Namespace + "/" + policy.Name
	if err := v.List(ctx, &endpoints,
		client.MatchingFields{controller.EndpointPolicyIndex: indexKey},
	); err != nil {
		return nil, fmt.Errorf("listing endpoints: %w", err)
	}

	var references []string
	for _, ep := range endpoints.Items {
		references = append(references, ep.Namespace+"/"+ep.Name)
	}
	sort.Strings(references)

	if len(references) > 0 {
		return nil, field.ErrorList{
			field.Forbidden(
				field.NewPath("metadata", "name"),
				fmt.Sprintf("policy is referenced by endpoints: %s",
					strings.Join(references, ", ")),
			),
		}.ToAggregate()
	}
	return nil, nil
}

func validatePolicyFields(policy *v1alpha1.KrakenDBackendPolicy) error {
	var errs field.ErrorList

	if policy.Spec.CircuitBreaker != nil {
		if policy.Spec.CircuitBreaker.MaxErrors <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "circuitBreaker", "maxErrors"),
				policy.Spec.CircuitBreaker.MaxErrors,
				"must be greater than 0",
			))
		}
		if policy.Spec.CircuitBreaker.Interval <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "circuitBreaker", "interval"),
				policy.Spec.CircuitBreaker.Interval,
				"must be greater than 0",
			))
		}
		if policy.Spec.CircuitBreaker.Timeout <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "circuitBreaker", "timeout"),
				policy.Spec.CircuitBreaker.Timeout,
				"must be greater than 0",
			))
		}
	}

	if policy.Spec.RateLimit != nil {
		if policy.Spec.RateLimit.MaxRate <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "rateLimit", "maxRate"),
				policy.Spec.RateLimit.MaxRate,
				"must be greater than 0",
			))
		}
	}

	return errs.ToAggregate()
}

// AutoConfigValidator validates KrakenDAutoConfig resources.
type AutoConfigValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDAutoConfig.
func (v *AutoConfigValidator) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	ac, ok := obj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", obj)
	}
	return nil, v.validate(ctx, ac)
}

// ValidateUpdate validates an updated KrakenDAutoConfig.
func (v *AutoConfigValidator) ValidateUpdate(
	ctx context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	ac, ok := newObj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", newObj)
	}
	return nil, v.validate(ctx, ac)
}

// ValidateDelete is a no-op for autoconfigs.
func (v *AutoConfigValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

func (v *AutoConfigValidator) validate(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
) error {
	var errs field.ErrorList

	gw := &v1alpha1.KrakenDGateway{}
	gwNS := ac.Spec.GatewayRef.ResolvedNamespace(ac.Namespace)
	if err := v.Get(ctx, types.NamespacedName{
		Name:      ac.Spec.GatewayRef.Name,
		Namespace: gwNS,
	}, gw); err != nil {
		if apierrors.IsNotFound(err) {
			refPath := field.NewPath("spec", "gatewayRef", "name")
			refValue := ac.Spec.GatewayRef.Name
			if gwNS != ac.Namespace {
				refPath = field.NewPath("spec", "gatewayRef", "namespace")
				refValue = gwNS
			}
			errs = append(errs, field.NotFound(refPath, refValue))
		} else {
			return field.ErrorList{field.InternalError(
				field.NewPath("spec", "gatewayRef"),
				fmt.Errorf("looking up gateway: %w", err),
			)}.ToAggregate()
		}
	}

	hasURL := ac.Spec.OpenAPI.URL != ""
	hasCM := ac.Spec.OpenAPI.ConfigMapRef != nil
	if hasURL && hasCM {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "openapi"),
			"both",
			"url and configMapRef are mutually exclusive",
		))
	}
	if !hasURL && !hasCM {
		errs = append(errs, field.Required(
			field.NewPath("spec", "openapi"),
			"one of url or configMapRef is required",
		))
	}

	if hasCM && !hasURL {
		if ac.Spec.URLTransform == nil || len(ac.Spec.URLTransform.HostMapping) == 0 {
			errs = append(errs, field.Required(
				field.NewPath("spec", "urlTransform", "hostMapping"),
				"hostMapping is required when using configMapRef",
			))
		}
	}

	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic {
		if ac.Spec.Periodic == nil || ac.Spec.Periodic.Interval.Duration == 0 {
			errs = append(errs, field.Required(
				field.NewPath("spec", "periodic", "interval"),
				"interval is required when trigger is Periodic",
			))
		}
	}

	if ac.Spec.OpenAPI.Auth != nil {
		if ac.Spec.OpenAPI.Auth.BearerTokenSecret != nil &&
			ac.Spec.OpenAPI.Auth.BasicAuthSecret != nil {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "openapi", "auth"),
				"both",
				"bearerTokenSecret and basicAuthSecret are mutually exclusive",
			))
		}
	}

	errs = append(errs, validateAdditionalEndpoints(ac)...)

	return errs.ToAggregate()
}

// validateAdditionalEndpoints validates the additionalEndpoints field and the
// additionalEndpointsBasePath field of a KrakenDAutoConfig.
func validateAdditionalEndpoints(ac *v1alpha1.KrakenDAutoConfig) field.ErrorList {
	var errs field.ErrorList

	if ac.Spec.AdditionalEndpointsBasePath != "" &&
		!strings.HasPrefix(ac.Spec.AdditionalEndpointsBasePath, "/") {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "additionalEndpointsBasePath"),
			ac.Spec.AdditionalEndpointsBasePath,
			"must start with '/'"))
	}

	if ac.Spec.AdditionalEndpointsBasePath != "" &&
		ac.Spec.URLTransform != nil && ac.Spec.URLTransform.AddPathPrefix != "" {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "additionalEndpointsBasePath"),
			ac.Spec.AdditionalEndpointsBasePath,
			"is mutually exclusive with spec.urlTransform.addPathPrefix; set only one"))
	}

	seenAdditional := make(map[string]struct{}, len(ac.Spec.AdditionalEndpoints))
	for i, ae := range ac.Spec.AdditionalEndpoints {
		p := field.NewPath("spec", "additionalEndpoints").Index(i)

		if ae.Endpoint == "" {
			errs = append(errs, field.Required(p.Child("endpoint"), "endpoint is required"))
		} else if !strings.HasPrefix(ae.Endpoint, "/") {
			errs = append(errs, field.Invalid(p.Child("endpoint"), ae.Endpoint,
				"endpoint must start with '/'"))
		}

		if len(ae.Backends) > 0 && (ae.Host != "" || ae.BackendURLPattern != "" || ae.Encoding != "") {
			errs = append(errs, field.Invalid(p, "both",
				"backends and the host/backendUrlPattern/encoding shorthand are mutually exclusive"))
		}

		method := ae.Method
		if method == "" {
			method = "GET"
		}
		key := method + " " + ae.Endpoint
		if _, dup := seenAdditional[key]; dup {
			errs = append(errs, field.Duplicate(p, key))
		}
		seenAdditional[key] = struct{}{}
	}

	return errs
}

// SetupWebhooks registers all validating webhooks with the manager.
func SetupWebhooks(mgr ctrl.Manager) error {
	// Ensure field indexes are registered — needed for conflict detection
	// and policy-delete validation even when running webhook-only.
	if err := controller.EnsureEndpointIndexes(mgr); err != nil {
		return fmt.Errorf("registering endpoint indexes: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDGateway{}).
		WithValidator(&GatewayValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up gateway webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDEndpoint{}).
		WithValidator(&EndpointValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up endpoint webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDBackendPolicy{}).
		WithValidator(&PolicyValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up policy webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDAutoConfig{}).
		WithValidator(&AutoConfigValidator{Client: mgr.GetClient()}).
		Complete(); err != nil {
		return fmt.Errorf("setting up autoconfig webhook: %w", err)
	}

	return nil
}
