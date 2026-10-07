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
	"maps"
	"slices"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// GatewayValidator validates KrakenDGateway resources.
type GatewayValidator struct {
	client.Client
	Checker ConfigChecker
	// Memo remembers recent config verdicts across requests. Nil remembers
	// nothing.
	Memo configcheck.Memo
}

// ValidateCreate validates a new KrakenDGateway. There is no "old" object on
// Create, so the runAsUser:0 ratchet never
// applies here — a brand-new CR gets the hard reject unconditionally.
func (v *GatewayValidator) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	gw, ok := obj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", obj)
	}
	return v.admit(ctx, nil, gw)
}

// ValidateUpdate validates an updated KrakenDGateway. An unchanged spec is not
// validated, and a rule rejects an update only for errors the stored object did
// not already have, so a CR accepted by an older operator version does not
// start failing every unrelated update. The old object is also threaded
// through to validate so the runAsUser:0 reject can be ratcheted.
func (v *GatewayValidator) ValidateUpdate(
	ctx context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	if terminatingWithUnchangedSpec(oldObj, newObj) {
		return nil, nil
	}
	gw, ok := newObj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", newObj)
	}
	old, ok := oldObj.(*v1alpha1.KrakenDGateway)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDGateway, got %T", oldObj)
	}
	if equality.Semantic.DeepEqual(old.Spec, gw.Spec) {
		return nil, nil
	}
	return v.admit(ctx, old, gw)
}

// ValidateDelete is required by admission.CustomValidator. The gateway webhook
// is not registered for DELETE, so it is never called.
func (v *GatewayValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

// admit runs every rule against gw within the admission budget, then renders
// its config. old is the stored object on an update and nil on a create; an
// update is rejected only for field errors the stored object did not already
// have.
func (v *GatewayValidator) admit(
	ctx context.Context, old, gw *v1alpha1.KrakenDGateway,
) (admission.Warnings, error) {
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()

	if err := v.authorizePostRestartJob(ctx, old, gw); err != nil {
		return nil, err
	}
	warnings, errs := v.validate(gw, old)
	if old != nil {
		errs = newErrors(errs, v.storedErrors(gw, old))
	}
	eeErrs, err := v.eeNamespacesOnCE(ctx, old, gw)
	if err != nil {
		return warnings, unavailable(err)
	}
	errs = append(errs, eeErrs...)
	errs = append(errs, eeFieldsOnCE(old, gw)...)
	if len(errs) > 0 {
		return warnings, invalid("KrakenDGateway", gw.Name, errs)
	}
	renderWarnings, err := checkGatewayRender(ctx, v.Client, v.Checker, v.Memo, old, gw)
	return append(append(warnings, renderWarnings...), versionWarning(gw, old)...), err
}

// eeNamespacesOnCE rejects Enterprise-only extra_config namespaces that a CE
// gateway would accept and then silently ignore: in spec.config.extraConfig
// when it is new, changed or newly on CE, and, when the gateway is created as CE
// or switches to CE, in its endpoints and the policies they reference, which
// their own webhooks admitted while the gateway ran EE (or while it did not
// exist).
func (v *GatewayValidator) eeNamespacesOnCE(
	ctx context.Context, old, gw *v1alpha1.KrakenDGateway,
) (field.ErrorList, error) {
	if gw.Spec.Edition != v1alpha1.EditionCE {
		return nil, nil
	}
	var errs field.ErrorList
	if old == nil || old.Spec.Edition != v1alpha1.EditionCE ||
		!equality.Semantic.DeepEqual(old.Spec.Config.ExtraConfig, gw.Spec.Config.ExtraConfig) {
		errs = ceIgnores(field.NewPath("spec", "config", "extraConfig"),
			renderer.EEOnlyNamespacesIn(gw.Spec.Config.ExtraConfig, renderer.LevelService))
	}
	if old != nil && old.Spec.Edition != v1alpha1.EditionEE {
		return errs, nil
	}
	uses, err := v.eeNamespacesInUse(ctx, gw)
	if err != nil {
		return nil, err
	}
	if len(uses) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "edition"), string(gw.Spec.Edition),
			"CE does not serve what these objects use (Enterprise-only extra_config namespaces it silently ignores, "+
				"and /prefix/* wildcards): "+
				truncate(strings.Join(uses, "; "), warningLimit)))
	}
	return errs, nil
}

// eeNamespacesInUse lists, sorted, each Enterprise-only namespace in gw's
// endpoints and in the policies they reference, as "<kind> <ns>/<name>
// <field> <namespace>", followed by ": <keys>" when CE honors the rest of the
// block, and each /prefix/* wildcard endpoint.
func (v *GatewayValidator) eeNamespacesInUse(ctx context.Context, gw *v1alpha1.KrakenDGateway) ([]string, error) {
	var eps v1alpha1.KrakenDEndpointList
	byGateway := client.MatchingFields{fieldindex.EndpointGateway: gw.Namespace + "/" + gw.Name}
	if err := v.List(ctx, &eps, byGateway); err != nil {
		return nil, fmt.Errorf("listing the endpoints of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	var uses []string
	policies := map[string]bool{}
	for i := range eps.Items {
		ep := &eps.Items[i]
		for j, e := range ep.Spec.Endpoints {
			if renderer.IsEEWildcard(e.Endpoint) {
				uses = append(uses, fmt.Sprintf("KrakenDEndpoint %s/%s spec.endpoints[%d].endpoint %s (an EE wildcard)",
					ep.Namespace, ep.Name, j, e.Endpoint))
			}
			for _, pd := range entryDrops(field.NewPath("spec", "endpoints").Index(j), e) {
				for _, d := range pd.drops {
					uses = append(uses, fmt.Sprintf("KrakenDEndpoint %s/%s %s %s",
						ep.Namespace, ep.Name, pd.path, droppedLabel(d)))
				}
			}
			for _, be := range e.Backends {
				if be.PolicyRef != nil {
					policies[be.PolicyRef.PolicyKey(ep.Namespace)] = true
				}
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(policies)) {
		ns, name, _ := strings.Cut(key, "/")
		var p v1alpha1.KrakenDBackendPolicy
		if err := v.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("getting policy %s: %w", key, err)
		}
		for _, d := range renderer.EEOnlyNamespacesIn(p.Spec.Raw, renderer.LevelBackend) {
			uses = append(uses, fmt.Sprintf("KrakenDBackendPolicy %s spec.raw %s", key, droppedLabel(d)))
		}
	}
	slices.Sort(uses)
	return uses, nil
}

// droppedLabel names what a CE render drops: the namespace, or for a partly
// honored block the namespace and the keys it drops.
func droppedLabel(d renderer.CEDrop) string {
	if len(d.Keys) == 0 {
		return d.Namespace
	}
	return d.Namespace + ": " + strings.Join(d.Keys, ", ")
}

// eeFieldsOnCE refuses, on a CE gateway, the typed fields that configure
// features only KrakenD Enterprise has: KrakenD CE ignores the redis
// connection pools and documentation/openapi, the CE binary has no openapi
// command for the OpenAPI export, and Dragonfly exists to back the redis
// pools. spec.redis and spec.config.documentation are judged when set or
// changed; spec.openapi and spec.dragonfly are keyed on enabled: enabling one
// is refused, and editing the settings of one stored enabled on CE is admitted
// with a warning (openAPIOnCEWarning, dragonflyOnCEWarning). A gateway
// switching from EE to CE is judged on every such field it keeps.
func eeFieldsOnCE(old, gw *v1alpha1.KrakenDGateway) field.ErrorList {
	if gw.Spec.Edition != v1alpha1.EditionCE {
		return nil
	}
	wasCE := old != nil && old.Spec.Edition == v1alpha1.EditionCE
	var errs field.ErrorList
	for _, f := range []struct {
		path      *field.Path
		set, kept bool
		what      string
	}{
		{field.NewPath("spec", "redis"), gw.Spec.Redis != nil,
			wasCE && equality.Semantic.DeepEqual(old.Spec.Redis, gw.Spec.Redis),
			"the redis connection pools are an Enterprise feature; KrakenD CE ignores them"},
		{field.NewPath("spec", "config", "documentation"), gw.Spec.Config.Documentation != nil,
			wasCE && equality.Semantic.DeepEqual(old.Spec.Config.Documentation, gw.Spec.Config.Documentation),
			"documentation/openapi is an Enterprise feature; KrakenD CE ignores it"},
		{field.NewPath("spec", "openapi", "enabled"), openAPIExportEnabled(gw), wasCE && openAPIExportEnabled(old),
			"the OpenAPI export is an Enterprise feature; the CE binary has no openapi command"},
		{field.NewPath("spec", "dragonfly", "enabled"), dragonflyEnabled(gw), wasCE && dragonflyEnabled(old),
			"Dragonfly is an Enterprise feature; KrakenD CE has no redis connection pools to use it"},
	} {
		if f.set && !f.kept {
			errs = append(errs, field.Forbidden(f.path, f.what+" (the gateway runs CE)"))
		}
	}
	return errs
}

// openAPIExportEnabled reports whether gw enables the OpenAPI export.
func openAPIExportEnabled(gw *v1alpha1.KrakenDGateway) bool {
	return gw != nil && gw.Spec.OpenAPI != nil && gw.Spec.OpenAPI.Enabled
}

// dragonflyEnabled reports whether gw enables the managed Dragonfly instance.
func dragonflyEnabled(gw *v1alpha1.KrakenDGateway) bool {
	return gw != nil && gw.Spec.Dragonfly != nil && gw.Spec.Dragonfly.Enabled
}

// validate runs all admission checks for gw. old is the previously-stored
// object on an Update (nil on Create) — see validatePostRestartJob's
// ratchet handling.
func (v *GatewayValidator) validate(gw, old *v1alpha1.KrakenDGateway) (admission.Warnings, field.ErrorList) {
	var errs field.ErrorList
	var warnings admission.Warnings

	warnings = append(warnings, replicasWithAutoscalingWarning(gw)...)
	warnings = append(warnings, openAPIOnCEWarning(old, gw)...)
	warnings = append(warnings, dragonflyOnCEWarning(old, gw)...)
	warnings = append(warnings, redisPoolWarnings(gw)...)

	if gw.Spec.OpenAPI != nil && gw.Spec.OpenAPI.Enabled {
		// Probe validation. Everything rejected here is already broken today: the
		// CR is accepted, the rendered Deployment is rejected by the API server,
		// and the reconcile then fails on backoff. That failure is SILENT -- a
		// probe-only edit does not change the config checksum, so no validation or
		// ConfigValid write happens, and the failed Deployment update leaves the
		// old Deployment in place: inspectDeploymentStatus still finds it
		// converged, so Available is never set False. The CR keeps its last-good
		// Ready and phase with no condition and no event while the Deployment,
		// HPA and post-restart Job freeze. Rejecting the input up front is the only
		// place the user gets told.
		//
		// Like every field rule, these reject an update only for errors it
		// introduces: a probe stored before the rules existed does not block
		// unrelated edits, and the check reads only the new object, so the
		// correcting update always passes.
		effectiveImage := resources.EffectiveOpenAPISidecarImage(gw.Spec.OpenAPI)
		defaultSidecar := effectiveImage == resources.DefaultOpenAPISidecarImage
		oaPath := field.NewPath("spec", "openapi")
		errs = append(errs, validateSidecarProbe(
			oaPath.Child("livenessProbe"), gw.Spec.OpenAPI.LivenessProbe, probeKindLiveness, defaultSidecar)...)
		errs = append(errs, validateSidecarProbe(
			oaPath.Child("readinessProbe"), gw.Spec.OpenAPI.ReadinessProbe, probeKindReadiness, defaultSidecar)...)
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

	if gw.Spec.Dragonfly != nil && gw.Spec.Dragonfly.Enabled {
		var oldDF *v1alpha1.DragonflySpec
		// Mirrors the postRestartJob ratchet above: only a previously-ENABLED
		// spec can have been grandfathered by an older operator.
		if old != nil && old.Spec.Dragonfly != nil && old.Spec.Dragonfly.Enabled {
			oldDF = old.Spec.Dragonfly
		}
		errs = append(errs, validateDragonflyRunAsRoot(gw.Spec.Dragonfly, oldDF)...)
	}

	return warnings, errs
}

// storedErrors returns the errors old already has, leaving out those under a
// sidecar probe the update changed: an error such as Forbidden carries no
// value, so it would read the same for the new probe and hide a real problem.
func (v *GatewayValidator) storedErrors(gw, old *v1alpha1.KrakenDGateway) field.ErrorList {
	_, stored := v.validate(old, old)
	oaPath := field.NewPath("spec", "openapi")
	oldLiveness, oldReadiness := sidecarProbes(old)
	liveness, readiness := sidecarProbes(gw)
	var changed []string
	if !equality.Semantic.DeepEqual(oldLiveness, liveness) {
		changed = append(changed, oaPath.Child("livenessProbe").String())
	}
	if !equality.Semantic.DeepEqual(oldReadiness, readiness) {
		changed = append(changed, oaPath.Child("readinessProbe").String())
	}
	return slices.DeleteFunc(stored, func(e *field.Error) bool {
		return slices.ContainsFunc(changed, func(p string) bool { return strings.HasPrefix(e.Field, p) })
	})
}

// sidecarProbes returns the OpenAPI sidecar's liveness and readiness probes.
func sidecarProbes(gw *v1alpha1.KrakenDGateway) (liveness, readiness *corev1.Probe) {
	if gw.Spec.OpenAPI == nil {
		return nil, nil
	}
	return gw.Spec.OpenAPI.LivenessProbe, gw.Spec.OpenAPI.ReadinessProbe
}

// replicasWithAutoscalingWarning warns when spec.replicas is set together
// with spec.autoscaling: the HorizontalPodAutoscaler owns the replica count,
// so the operator ignores spec.replicas. It is a warning, not a rejection,
// because the combination is harmless and may already be stored.
func replicasWithAutoscalingWarning(gw *v1alpha1.KrakenDGateway) admission.Warnings {
	if gw.Spec.Replicas == nil || gw.Spec.Autoscaling == nil {
		return nil
	}
	return admission.Warnings{
		"spec.replicas is ignored while spec.autoscaling is set: " +
			"the HorizontalPodAutoscaler manages the replica count",
	}
}

// openAPIOnCEWarning warns when a CE gateway keeps an enabled spec.openapi
// that was stored before admission refused it: the CE binary has no openapi
// command, so the operator runs no export or serving there. Enabling it, or
// switching to CE while it is enabled, is rejected instead (eeFieldsOnCE); an
// edit of the settings of a stored, enabled one is admitted with this warning,
// and it takes effect again on an EE gateway.
func openAPIOnCEWarning(old, gw *v1alpha1.KrakenDGateway) admission.Warnings {
	if !keptOnCE(old, gw, openAPIExportEnabled) {
		return nil
	}
	return admission.Warnings{
		"spec.openapi is ignored on CE gateways: the CE binary cannot export OpenAPI, " +
			"so no export or openapi-serve sidecar runs",
	}
}

// dragonflyOnCEWarning warns when a CE gateway keeps a Dragonfly that was
// stored enabled before admission refused it: KrakenD CE ignores the redis
// connection pool, so the Dragonfly instance runs for nothing. Enabling it, or
// switching to CE while it is enabled, is rejected instead (eeFieldsOnCE).
func dragonflyOnCEWarning(old, gw *v1alpha1.KrakenDGateway) admission.Warnings {
	if !keptOnCE(old, gw, dragonflyEnabled) {
		return nil
	}
	return admission.Warnings{
		"spec.dragonfly.enabled has no effect on CE gateways: KrakenD CE ignores the redis connection pool, " +
			"so the Dragonfly instance runs for nothing",
	}
}

// keptOnCE reports whether gw is a CE gateway that already was one with the
// feature enabled, the only way a CE gateway still carries it.
func keptOnCE(old, gw *v1alpha1.KrakenDGateway, enabled func(*v1alpha1.KrakenDGateway) bool) bool {
	return gw.Spec.Edition == v1alpha1.EditionCE && enabled(gw) &&
		old != nil && old.Spec.Edition == v1alpha1.EditionCE && enabled(old)
}

// redisPoolWarnings reports Redis and Dragonfly settings that do not reach
// KrakenD's connection: its redis namespace has no field for readTimeout or
// writeTimeout, and the operator does not render the pool's password or tls.
// On EE it does not render the Dragonfly password either, though Dragonfly
// requires it, so KrakenD's connections to Dragonfly are refused.
func redisPoolWarnings(gw *v1alpha1.KrakenDGateway) admission.Warnings {
	const (
		noSetting   = "has no effect: KrakenD's redis connection pools have no such setting"
		notRendered = "has no effect: the operator does not render it yet, so KrakenD connects without it"
		dfRequires  = "is not rendered: Dragonfly requires this password, but the operator does not render it into " +
			"KrakenD's redis pool, so KrakenD's connections to Dragonfly are refused"
	)
	var pool v1alpha1.RedisConnectionPool
	if gw.Spec.Redis != nil {
		pool = gw.Spec.Redis.ConnectionPool
	}
	//nolint:staticcheck // reads deprecated fields to warn users
	hasRead, hasWrite := pool.ReadTimeout != "", pool.WriteTimeout != ""
	df := gw.Spec.Dragonfly
	var warnings admission.Warnings
	for _, f := range []struct {
		path, why string
		set       bool
	}{
		{"spec.redis.connectionPool.readTimeout", noSetting, hasRead},
		{"spec.redis.connectionPool.writeTimeout", noSetting, hasWrite},
		{"spec.redis.connectionPool.password", notRendered, pool.Password != nil},
		{"spec.redis.connectionPool.tls", notRendered, pool.TLS != nil},
		{"spec.dragonfly.authentication.passwordFromSecret", dfRequires, gw.Spec.Edition == v1alpha1.EditionEE &&
			df != nil && df.Authentication != nil && df.Authentication.PasswordFromSecret != nil},
	} {
		if f.set {
			warnings = append(warnings, f.path+" "+f.why)
		}
	}
	return warnings
}

// validatePostRestartJob validates spec.postRestartJob when enabled. Split
// out of GatewayValidator.validate to keep that function's cyclomatic
// complexity in check (gocyclo) as this block grew. old is the
// previously-stored spec on an Update (nil on Create).
func validatePostRestartJob(
	prj *v1alpha1.PostRestartJobSpec, old *v1alpha1.PostRestartJobSpec,
) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings

	if w := validatePostRestartWorkingDir(prj); w != "" {
		warnings = append(warnings, w)
	}

	errs = append(errs, validatePostRestartRunAsRoot(prj, old)...)

	// A negative tmpSizeLimit is nonsensical
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
//     rejected at admission.
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
// An explicit runAsNonRoot: false at POD scope is always an acknowledged
// opt-out and short-circuits the whole check, since it inherits down to
// every container. An explicit runAsNonRoot: false at CONTAINER scope only
// short-circuits when the effective uid0 came from that same container
// scope — see the containerOptsOut/podOptsOut
// combination below.
//
// Review id 3807285627 (#2): the check is skipped entirely (ratcheted) on
// an Update when none of the four fields it inspects
// (securityContext.{runAsUser,runAsNonRoot}, podSecurityContext.
// runAsNonRoot — podSecurityContext.runAsUser is intentionally included
// too, see runAsFieldsUnchanged) changed from the stored spec — a CR
// accepted by an older operator version (before this reject existed) must
// not start failing on every unrelated update.
func validatePostRestartRunAsRoot(prj, old *v1alpha1.PostRestartJobSpec) field.ErrorList {
	var oldContainer *corev1.SecurityContext
	var oldPod *corev1.PodSecurityContext
	if old != nil {
		oldContainer = old.SecurityContext
		oldPod = old.PodSecurityContext
	}

	return validateRunAsRootConflict(
		prj.SecurityContext, prj.PodSecurityContext,
		oldContainer, oldPod,
		field.NewPath("spec", "postRestartJob", "securityContext", "runAsUser"),
		field.NewPath("spec", "postRestartJob", "podSecurityContext", "runAsUser"),
		// job.go's postRestartJob container default leaves runAs* UNSET at
		// container scope (pod scope governs), so a pod-scope uid0 with
		// runAsNonRoot left unset everywhere is a real, self-healable
		// capability — job.go's mergePodSecurityContext fixup drops the
		// inherited pod-scope runAsNonRoot default in that case (see
		// "Keep the builder fixup" in this function's original doc, now
		// captured by this true argument).
		true,
		// A container-scope runAsNonRoot: false acknowledges root only when the
		// effective uid 0 came from the container scope (see
		// validateRunAsRootConflict's containerOptsOut/podOptsOut asymmetry),
		// so this text is self-qualifying instead of path-specific and stays
		// truthful whether it is emitted at containerPath or podPath.
		"runAsUser: 0 conflicts with the hardened runAsNonRoot: true default (kubelet "+
			"pod-level runAsNonRoot defaults to true and is inherited unless overridden). "+
			"Set spec.postRestartJob.podSecurityContext.runAsNonRoot: false to acknowledge "+
			"running as root; a container-scope "+
			"spec.postRestartJob.securityContext.runAsNonRoot: false only acknowledges a "+
			"container-scope runAsUser: 0. Unless the Job container carries its own "+
			"runAsNonRoot: false, the effective {runAsUser: 0, runAsNonRoot: true} pair "+
			"leaves the pod Pending (CreateContainerConfigError) until activeDeadlineSeconds "+
			"expires.",
	)
}

// probeKind distinguishes the two sidecar probes. Kubernetes validates them
// differently, so a kind-blind validator would itself reintroduce a wedge:
// terminationGracePeriodSeconds is FORBIDDEN outright on a readiness probe but
// must be > 0 when set on a liveness probe.
type probeKind int

const (
	probeKindLiveness probeKind = iota
	probeKindReadiness
)

// validateSidecarProbe rejects the openapi sidecar probe states that the CRD
// schema accepts but the API server refuses on the rendered Deployment, plus
// the two that are structurally impossible against the operator's default
// sidecar image.
//
// Split into three helpers to keep this function's cyclomatic complexity in
// check (gocyclo), mirroring validatePostRestartJob above: the checks grew from
// one to eight over time.
//
// defaultSidecarImage must be computed from resources.EffectiveOpenAPISidecarImage,
// never from a raw `SidecarImage == ""` test.
func validateSidecarProbe(
	p *field.Path, probe *corev1.Probe, kind probeKind, defaultSidecarImage bool,
) field.ErrorList {
	var errs field.ErrorList
	if probe == nil {
		return errs
	}
	errs = append(errs, validateProbeSchedule(p, probe, kind)...)
	errs = append(errs, validateProbePorts(p, probe)...)
	errs = append(errs, validateProbeReachability(p, probe, defaultSidecarImage)...)
	return errs
}

// validateProbeSchedule covers the handler count and the timing/threshold
// fields -- the shape of the probe, independent of which handler it uses.
func validateProbeSchedule(p *field.Path, probe *corev1.Probe, kind probeKind) field.ErrorList {
	var errs field.ErrorList

	handlers := 0
	for _, set := range []bool{probe.Exec != nil, probe.HTTPGet != nil, probe.TCPSocket != nil, probe.GRPC != nil} {
		if set {
			handlers++
		}
	}
	if handlers != 1 {
		errs = append(errs, field.Invalid(p, handlers,
			"exactly one probe handler (exec, httpGet, tcpSocket or grpc) must be specified"))
	}

	if kind == probeKindLiveness && probe.SuccessThreshold != 0 && probe.SuccessThreshold != 1 {
		errs = append(errs, field.Invalid(p.Child("successThreshold"), probe.SuccessThreshold,
			"must be 1 for a liveness probe"))
	}

	// Sorted so the error order is deterministic across runs. successThreshold is
	// here as well as in the liveness branch above: that branch is liveness-only,
	// so without this entry a negative value on a READINESS probe slips through.
	for _, f := range []struct {
		name string
		v    int32
	}{
		{"failureThreshold", probe.FailureThreshold},
		{"initialDelaySeconds", probe.InitialDelaySeconds},
		{"periodSeconds", probe.PeriodSeconds},
		{"successThreshold", probe.SuccessThreshold},
		{"timeoutSeconds", probe.TimeoutSeconds},
	} {
		if f.v < 0 {
			errs = append(errs, field.Invalid(p.Child(f.name), f.v, "must be non-negative"))
		}
	}

	if tgps := probe.TerminationGracePeriodSeconds; tgps != nil {
		switch {
		case kind == probeKindReadiness:
			errs = append(errs, field.Forbidden(p.Child("terminationGracePeriodSeconds"),
				"must not be set on a readiness probe"))
		case *tgps <= 0:
			errs = append(errs, field.Invalid(p.Child("terminationGracePeriodSeconds"), *tgps,
				"must be greater than 0"))
		}
	}

	return errs
}

// validateProbePorts bounds the port on whichever handler is set.
// ValidatePortNumOrName bounds numeric ports to 1-65535 and requires string
// ports to be valid IANA_SVC_NAMEs; the CRD bounds neither -- the constraint
// lives only in description prose.
func validateProbePorts(p *field.Path, probe *corev1.Probe) field.ErrorList {
	var errs field.ErrorList

	checkPort := func(path *field.Path, port intstr.IntOrString) {
		switch port.Type {
		case intstr.Int:
			if port.IntValue() < 1 || port.IntValue() > 65535 {
				errs = append(errs, field.Invalid(path, port.IntValue(), "must be between 1 and 65535"))
			}
		case intstr.String:
			for _, msg := range validation.IsValidPortName(port.StrVal) {
				errs = append(errs, field.Invalid(path, port.StrVal, msg))
			}
		}
	}

	if probe.HTTPGet != nil {
		checkPort(p.Child("httpGet", "port"), probe.HTTPGet.Port)
	}
	if probe.TCPSocket != nil {
		checkPort(p.Child("tcpSocket", "port"), probe.TCPSocket.Port)
	}
	if probe.GRPC != nil && (probe.GRPC.Port < 1 || probe.GRPC.Port > 65535) {
		errs = append(errs, field.Invalid(p.Child("grpc", "port"), probe.GRPC.Port, "must be between 1 and 65535"))
	}

	return errs
}

// validateProbeReachability covers whether the probe can reach the sidecar at
// all: a scheme Kubernetes will not accept, a host outside this pod, and the
// two handlers the operator's default image cannot serve.
func validateProbeReachability(p *field.Path, probe *corev1.Probe, defaultSidecarImage bool) field.ErrorList {
	var errs field.ErrorList

	// Kubernetes accepts only the exact strings HTTP and HTTPS; anything else,
	// including a lowercase "https", is field.NotSupported on the Deployment.
	// Unset is fine -- SetDefaults_HTTPGetAction fills in HTTP. This runs BEFORE
	// the default-image gate below so a lowercase "https" gets NotSupported
	// rather than the "use scheme HTTP" advice, which would be wrong for it.
	if probe.HTTPGet != nil && probe.HTTPGet.Scheme != "" &&
		probe.HTTPGet.Scheme != corev1.URISchemeHTTP && probe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		errs = append(errs, field.NotSupported(p.Child("httpGet", "scheme"), probe.HTTPGet.Scheme,
			[]string{string(corev1.URISchemeHTTP), string(corev1.URISchemeHTTPS)}))
	}

	// Probes are dialed by the kubelet from the NODE network namespace, so an
	// off-pod host bypasses pod-scoped egress NetworkPolicies. As of the
	// 2026-08 fleet audit (PR #29) no live gateway set either probe, so this
	// rejected nothing that was working at the time it landed.
	if probe.HTTPGet != nil && probe.HTTPGet.Host != "" {
		errs = append(errs, field.Forbidden(p.Child("httpGet", "host"),
			"probe host must be the pod IP (leave unset)"))
	}
	if probe.TCPSocket != nil && probe.TCPSocket.Host != "" {
		errs = append(errs, field.Forbidden(p.Child("tcpSocket", "host"),
			"probe host must be the pod IP (leave unset)"))
	}

	// Equality on the EFFECTIVE image is deliberate. busybox:1.36, a digest pin
	// or a distroless image are equally incapable and are accepted false
	// negatives; anything broader would false-positive on a custom image that
	// genuinely speaks these protocols.
	if !defaultSidecarImage {
		return errs
	}
	if probe.GRPC != nil {
		errs = append(errs, field.Invalid(
			p.Child("grpc"),
			probe.GRPC,
			"the default busybox sidecar serves plaintext HTTP/1.1; a grpc handler can never succeed -- set spec.openapi.sidecarImage if your image speaks gRPC",
		))
	}
	if probe.HTTPGet != nil && probe.HTTPGet.Scheme == corev1.URISchemeHTTPS {
		errs = append(errs, field.Invalid(p.Child("httpGet", "scheme"), probe.HTTPGet.Scheme,
			"the default busybox sidecar serves plaintext HTTP; use scheme HTTP or set spec.openapi.sidecarImage"))
	}

	return errs
}

// validateRunAsRootConflict is the scope-agnostic core shared by
// validatePostRestartRunAsRoot and validateDragonflyRunAsRoot (DRY
// extraction). It rejects a spec whose
// kubelet-EFFECTIVE {runAsUser, runAsNonRoot} pair for the container
// resolves to {0, true} — the pair the kubelet rejects at container start
// (CreateContainerConfigError), hanging the pod Pending.
//
// containerPath/podPath are the FULL field.Path to each scope's
// `runAsUser` field (e.g. spec.postRestartJob.securityContext.runAsUser vs
// spec.dragonfly.containerSecurityContext.runAsUser) — the two scopes use
// different JSON field names for the container-level securityContext
// (`securityContext` vs `containerSecurityContext`), so the caller builds
// the full path rather than this helper deriving it from a shared base.
// The emitted field.Invalid uses whichever of the two paths matches WHERE the
// effective uid0 was resolved from (effectiveRunAsRoot's fromContainer) — a pod-scope
// rejection now points at podPath instead of always pointing at
// containerPath, a field the user may never have set. This changes only the
// postRestartJob pod-scope rejection's error path string; the
// accept/reject matrix is otherwise byte-identical (verified by the
// existing webhook test suite, which only asserts on error substrings, not
// exact paths).
//
// allowPodScopeUnsetSelfHeal is the deliberate divergence knob between the
// two scopes: when uid0 comes from the POD scope alone and runAsNonRoot is
// unset everywhere (neither scope asserts true or false),
//   - postRestartJob passes true: job.go's container default leaves runAs*
//     UNSET at container scope, so pod-scope root is a REAL capability —
//     job.go's mergePodSecurityContext fixup self-heals this combination at
//     build time, so admission does not need to reject it (defense in depth
//     only, for webhook-bypass paths).
//   - dragonfly passes false: dragonfly's container default PINS
//     RunAsUser/RunAsGroup/RunAsNonRoot at container scope to match the dfly
//     image's built-in uid, and container-scope always overrides pod-scope
//     per-field at the kubelet — so pod-scope uid0 NEVER changes the
//     dragonfly container's effective uid; it only roots injected sidecars
//     silently and strips nothing useful. There is no legitimate capability
//     to preserve, so this combination is now REJECTED at admission instead
//     of self-healed (see mergeDragonflyPodSecurityContext, whose pod-scope
//     self-heal fixup was removed for the same reason).
//
// The update-ratchet (runAsFieldsUnchanged) still grandfathers an unchanged
// old spec for both scopes.
//
// For the Dragonfly lane only (allowPodScopeUnsetSelfHeal == false), a
// POD-SCOPE root request is independently gated regardless of what
// effectiveRunAsRoot resolves for the primary container. A non-zero
// container.RunAsUser (e.g. Dragonfly's own uid:999 pin) makes
// effectiveRunAsRoot report "not root" — correctly, for THAT one container —
// and without this gate the whole function would return early, silently
// admitting a podSecurityContext.runAsUser: 0 that still renders on the
// shared pod-level securityContext every OTHER container/sidecar in the pod
// inherits when it sets nothing of its own. This gate closes that hole
// without touching effectiveRunAsRoot's own (still correct, for the primary
// container) precedence rules.
func validateRunAsRootConflict(
	container *corev1.SecurityContext, pod *corev1.PodSecurityContext,
	oldContainer *corev1.SecurityContext, oldPod *corev1.PodSecurityContext,
	containerPath, podPath *field.Path,
	allowPodScopeUnsetSelfHeal bool,
	detail string,
) field.ErrorList {
	if runAsFieldsUnchanged(container, pod, oldContainer, oldPod) {
		return nil
	}

	// A pod-scope root request requires a pod-scope acknowledgment (an
	// explicit pod-scope runAsNonRoot: false) — unless the container scope
	// itself carries its own runAsUser: 0, in which case the request routes
	// through the container-path rules below instead (preserving the
	// documented container-root recipe, e.g.
	// container{runAsUser:0,runAsNonRoot:false} + pod{runAsUser:0}, which
	// stays admitted with no security delta vs. pod{runAsUser:0,
	// runAsNonRoot:false} alone). Gated to the Dragonfly lane
	// (!allowPodScopeUnsetSelfHeal): postRestartJob has a single container
	// and no sidecar-injection surface, so its pod-scope-unset case is
	// already fully covered by the self-heal carve-out further down.
	if !allowPodScopeUnsetSelfHeal &&
		pod != nil && pod.RunAsUser != nil && *pod.RunAsUser == 0 &&
		(pod.RunAsNonRoot == nil || *pod.RunAsNonRoot) &&
		(container == nil || container.RunAsUser == nil || *container.RunAsUser != 0) {
		return field.ErrorList{field.Invalid(podPath, int64(0), detail)}
	}

	uid0, fromContainer := effectiveRunAsRoot(container, pod)
	if !uid0 {
		return nil
	}

	containerOptsOut := container != nil &&
		container.RunAsNonRoot != nil && !*container.RunAsNonRoot
	podOptsOut := pod != nil &&
		pod.RunAsNonRoot != nil && !*pod.RunAsNonRoot
	// An opt-out is only a valid acknowledgment from the
	// scope that actually produced the effective root request. Pod-scope
	// opt-out (podOptsOut) is ALWAYS acceptable regardless of fromContainer,
	// because pod scope inherits down to every container that doesn't set
	// its own runAsNonRoot — it covers both a pod-scope root request AND a
	// container-scope one (the container simply falls back to the pod's
	// acknowledged false). But a container-scope opt-out (containerOptsOut)
	// only acknowledges what THAT container asked for — it cannot make a
	// POD-scope root request's rendered pod-level pair startable for other
	// containers/sidecars in the pod that set nothing and so inherit the
	// pod-scope pair directly, bypassing this container's own opt-out
	// entirely. So containerOptsOut only short-circuits when the uid0 came
	// from the container scope in the first place (fromContainer).
	if (fromContainer && containerOptsOut) || podOptsOut {
		return nil
	}

	containerAssertsTrue := container != nil &&
		container.RunAsNonRoot != nil && *container.RunAsNonRoot
	podAssertsTrue := pod != nil &&
		pod.RunAsNonRoot != nil && *pod.RunAsNonRoot

	if !fromContainer && !containerAssertsTrue && !podAssertsTrue && allowPodScopeUnsetSelfHeal {
		return nil
	}

	path := containerPath
	if !fromContainer {
		path = podPath
	}

	return field.ErrorList{field.Invalid(path, int64(0), detail)}
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

// runAsFieldsUnchanged reports whether the four fields
// validateRunAsRootConflict inspects (container runAsUser/runAsNonRoot, pod
// runAsUser/runAsNonRoot) are identical between the new and old
// container/pod securityContext pair — the scope-agnostic ratchet condition
// for the update ratchet, replacing the former
// securityContextRunAsFieldsUnchanged (postRestartJob) and
// dragonflySecurityContextRunAsFieldsUnchanged (Dragonfly), which duplicated
// this same field-by-field comparison per scope. old{Container,Pod} may
// both be nil (no previously-stored spec, or postRestartJob/dragonfly newly
// added/enabled on this update — see validate's oldPRJ/oldDF gating, which
// only threads through a previously-ENABLED spec) — in that case an
// all-nil new pair trivially compares equal (a no-op ratchet), and any
// non-nil new field compares unequal, falling through to full validation
// exactly as a Create would. new{Container,Pod} are never both nil when
// this matters, since validateRunAsRootConflict's own effectiveRunAsRoot
// check would report "not root" for an all-nil pair regardless.
func runAsFieldsUnchanged(
	container *corev1.SecurityContext, pod *corev1.PodSecurityContext,
	oldContainer *corev1.SecurityContext, oldPod *corev1.PodSecurityContext,
) bool {
	return int64PtrEqual(securityContextRunAsUser(container), securityContextRunAsUser(oldContainer)) &&
		boolPtrEqual(securityContextRunAsNonRoot(container), securityContextRunAsNonRoot(oldContainer)) &&
		int64PtrEqual(podSecurityContextRunAsUser(pod), podSecurityContextRunAsUser(oldPod)) &&
		boolPtrEqual(podSecurityContextRunAsNonRoot(pod), podSecurityContextRunAsNonRoot(oldPod))
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

// validateDragonflyRunAsRoot mirrors validatePostRestartRunAsRoot for the
// Dragonfly scope (spec.dragonfly.podSecurityContext /
// spec.dragonfly.containerSecurityContext), now that
// mergeDragonflyPodSecurityContext (internal/resources/dragonfly.go) merges
// user-provided securityContext fields on top of hardened defaults instead
// of replacing them wholesale — the same {runAsUser:0, runAsNonRoot:true}
// admission hole validatePostRestartRunAsRoot closes for postRestartJob now
// also exists here.
//
// Unlike postRestartJob, dragonfly's
// container-scope default PINS RunAsUser/RunAsGroup/RunAsNonRoot (matching
// the dfly image's built-in uid), and container-scope security-context
// fields always override pod-scope per-field at the kubelet — so
// pod-scope runAsUser:0 NEVER changes the Dragonfly container's effective
// uid; it only roots injected sidecars silently and strips nothing useful.
// There is no legitimate NEW capability to preserve for admission purposes,
// so — unlike postRestartJob's pod-scope-unset case, which stays a
// self-heal (allowPodScopeUnsetSelfHeal: true, see
// validatePostRestartRunAsRoot) — the Dragonfly pod-scope-unset case is
// REJECTED at admission for any NEW or CHANGED spec (allowPodScopeUnsetSelfHeal:
// false below). This is an admission-time decision only: the builder
// separately keeps a build-time fixup in mergeDragonflyPodSecurityContext
// (internal/resources/dragonfly.go) — not as an admission self-heal, but to
// keep GRANDFATHERED specs (ratchet-exempted on Update, or reaching the
// builder via a bypassed webhook) rendering at the same main-branch parity
// they always had, rather than newly breaking on this merge-semantics
// change. See that function's doc for the full rationale. The
// update-ratchet still grandfathers an unchanged old spec.
func validateDragonflyRunAsRoot(df, old *v1alpha1.DragonflySpec) field.ErrorList {
	var oldContainer *corev1.SecurityContext
	var oldPod *corev1.PodSecurityContext
	if old != nil {
		oldContainer = old.ContainerSecurityContext
		oldPod = old.PodSecurityContext
	}

	return validateRunAsRootConflict(
		df.ContainerSecurityContext, df.PodSecurityContext,
		oldContainer, oldPod,
		field.NewPath("spec", "dragonfly", "containerSecurityContext", "runAsUser"),
		field.NewPath("spec", "dragonfly", "podSecurityContext", "runAsUser"),
		false,
		// A Dragonfly pod hangs Pending only for a container-scope-originated
		// violation: a pod-scope-only runAsUser: 0 never changes the dragonfly
		// container's own effective uid (the container-scope 999 pin always
		// wins per-field), so it silently roots OTHER containers/sidecars
		// instead. A container-scope runAsNonRoot: false acknowledgment works
		// (per the containerOptsOut/podOptsOut asymmetry) only for a
		// container-scope-originated violation. This text is therefore
		// self-qualifying per shape rather than path-specific, and stays
		// truthful whether it is emitted at containerPath or podPath.
		"runAsUser: 0 is requested without a runAsNonRoot: false that acknowledges it. An "+
			"unacknowledged container-scope runAsUser: 0 renders either the kubelet-invalid "+
			"{runAsUser: 0, runAsNonRoot: true} pair (CreateContainerConfigError) or a "+
			"container silently running as root, depending on the other securityContext "+
			"fields; a pod-scope runAsUser: 0 does not change the Dragonfly container's uid "+
			"at all, because the operator's container default pins runAsUser: 999 unless you "+
			"override it and a container-scope value always wins per-field. To have this "+
			"spec accepted, set spec.dragonfly.podSecurityContext.runAsNonRoot: false; or, if "+
			"the Dragonfly container itself must run as root, set "+
			"spec.dragonfly.containerSecurityContext.runAsUser: 0 together with a "+
			"runAsNonRoot: false at either scope (a container-scope runAsNonRoot: false "+
			"acknowledges only a container-scope runAsUser: 0).",
	)
}

// validatePostRestartWorkingDir returns a non-empty warning string when
// spec.postRestartJob.workingDir is overridden outside the writable /tmp
// emptyDir mount while readOnlyRootFilesystem is effectively true — the container still starts fine, so this is not
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

	// "use an absolute path under /tmp" is not a
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

// checkGatewayRender validates gw's config. An update that renders the same
// config as the stored gateway is not checked. Otherwise:
//  1. an update must not make entries clash in the router that the stored
//     root serves together (refuseNewGatewayClashes);
//  2. gw's root must pass on its own (rootVerdict); its owner writes the
//     gateway, so its output is quoted;
//  3. the root with the endpoints it serves (servedEndpoints) is checked as
//     a group, and the endpoints that check leaves unjudged on their own:
//     judgeServed decides an update, warnWaiting answers a create. Neither
//     quotes an endpoint.
func checkGatewayRender(ctx context.Context, c client.Reader, chk ConfigChecker, memo configcheck.Memo,
	old, gw *v1alpha1.KrakenDGateway) (admission.Warnings, error) {
	if old != nil {
		same, err := chk.SameConfig(ctx, old, gw)
		if err != nil {
			return nil, checkErr(err)
		}
		if same {
			return nil, nil // nothing the check judges has changed
		}
		if err := refuseNewGatewayClashes(ctx, chk, old, gw); err != nil {
			return nil, err
		}
	}
	ceFallback := configcheck.CEFallback(gw)
	root, err := chk.CheckRoot(ctx, configcheck.Root{Gateway: gw, CEFallback: ceFallback}, memo)
	if err != nil {
		return nil, checkErr(err)
	}
	if !root.OK {
		return rootVerdict(ctx, chk, memo, old, gw, root)
	}
	served, err := servedEndpointsOf(ctx, c, gw)
	if err != nil {
		return nil, unavailable(err)
	}
	group, err := chk.CheckGroup(ctx, configcheck.Group{Gateway: gw, Endpoints: served, CEFallback: ceFallback}, memo)
	now := configcheck.EndpointUnit{Gateway: gw, CEFallback: ceFallback}
	if old == nil {
		return warnWaiting(ctx, chk, memo, now, group, err, served), nil
	}
	if err != nil {
		return nil, checkErr(err)
	}
	return judgeServed(ctx, chk, memo, now, old, group, served)
}

// rootVerdict answers a gateway write whose root fails on its own: a denial
// on spec.config quoting the root's own output, unless the stored root (on
// an update) fails on its own too, which only draws a warning.
func rootVerdict(ctx context.Context, chk ConfigChecker, memo configcheck.Memo, old, gw *v1alpha1.KrakenDGateway,
	root configcheck.Verdict) (admission.Warnings, error) {
	if old != nil {
		was, err := chk.CheckRoot(ctx, configcheck.Root{Gateway: old, CEFallback: configcheck.CEFallback(old)}, memo)
		if err != nil {
			return nil, checkErr(err)
		}
		if !was.OK {
			return admission.Warnings{"the gateway's root already fails validation on its own: " +
				root.Excerpt(warningLimit)}, nil
		}
	}
	return nil, invalid("KrakenDGateway", gw.Name, field.ErrorList{field.Invalid(field.NewPath("spec", "config"),
		field.OmitValueType{}, "fails krakend check on its own: "+root.Excerpt(warningLimit))})
}

// servedEndpointsOf lists the endpoints of gw that it serves (servedEndpoints).
func servedEndpointsOf(
	ctx context.Context, c client.Reader, gw *v1alpha1.KrakenDGateway,
) ([]v1alpha1.KrakenDEndpoint, error) {
	var list v1alpha1.KrakenDEndpointList
	if err := c.List(ctx, &list, client.UnsafeDisableDeepCopy,
		client.MatchingFields{fieldindex.EndpointGateway: gw.Namespace + "/" + gw.Name}); err != nil {
		return nil, fmt.Errorf("listing endpoints of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	return servedEndpoints(list.Items), nil
}

// couldNotCheckWaiting begins the warning of a gateway create whose waiting
// endpoints could not be checked: the root passed, so the create stands.
const couldNotCheckWaiting = "could not check the endpoints that already reference this gateway: "

// waitingFail begins the warning of a gateway create whose waiting endpoints
// do not all pass with it.
const waitingFail = "the endpoints that already reference this gateway do not all pass validation with it, " +
	"and are left out until they do"

// warnWaiting answers a gateway create about the endpoints that already
// reference it (served), given the check of them as a group with it (group,
// or groupErr when that check could not run). It only warns: they were
// written before the gateway, so the create is not theirs to refuse. now is
// the unit of the new gateway.
func warnWaiting(ctx context.Context, chk ConfigChecker, memo configcheck.Memo, now configcheck.EndpointUnit,
	group configcheck.Verdict, groupErr error, served []v1alpha1.KrakenDEndpoint) admission.Warnings {
	if groupErr != nil {
		return admission.Warnings{truncate(couldNotCheckWaiting+groupErr.Error(), warningLimit)}
	}
	if group.OK {
		return nil
	}
	s := failingEndpoints(ctx, chk, memo, now, nil, served)
	if len(s.broken) > 0 {
		return admission.Warnings{waitingFail + ": " + brokenList(s, 2*warningLimit-len(waitingFail+": "))}
	}
	return nil
}

// judgeServed decides a gateway update from the endpoints gw serves (served),
// given the check of them as a group with the update (group). When it fails,
// each endpoint is checked on its own with the update (now) and, when that
// fails, with the stored gateway (old). One that fails only with the update
// is broken by it: the update is refused, naming it and quoting nothing of
// it. When every endpoint that fails failed with the stored gateway too, the
// update only draws a warning. When the group failed but no endpoint fails on
// its own, the group with the stored gateway tells whether failing together is
// the update's doing.
func judgeServed(ctx context.Context, chk ConfigChecker, memo configcheck.Memo, now configcheck.EndpointUnit,
	old *v1alpha1.KrakenDGateway, group configcheck.Verdict, served []v1alpha1.KrakenDEndpoint,
) (admission.Warnings, error) {
	if group.OK {
		return nil, nil
	}
	was := &configcheck.EndpointUnit{Gateway: old, CEFallback: configcheck.CEFallback(old)}
	s := failingEndpoints(ctx, chk, memo, now, was, served)
	switch {
	case s.stopped != nil:
		return nil, checkErr(s.stopped)
	case len(s.broken) > 0:
		return nil, invalid("KrakenDGateway", now.Gateway.Name, field.ErrorList{field.Invalid(
			field.NewPath("spec"), field.OmitValueType{}, brokenList(s, 2*warningLimit))})
	case s.already:
		return admission.Warnings{"some of the gateway's endpoints already fail validation with the stored " +
			"config, and stay left out until they pass"}, nil
	}
	before, err := chk.CheckGroup(ctx,
		configcheck.Group{Gateway: old, Endpoints: served, CEFallback: was.CEFallback}, memo)
	if err != nil {
		return nil, checkErr(err)
	}
	if !before.OK {
		return admission.Warnings{"the gateway's endpoints already fail validation together with the stored " +
			"config, though each passes on its own"}, nil
	}
	return nil, invalid("KrakenDGateway", now.Gateway.Name, field.ErrorList{field.Invalid(field.NewPath("spec"),
		field.OmitValueType{}, "with this change the gateway's endpoints fail validation together, though each "+
			"passes on its own")})
}

// refuseNewGatewayClashes rejects gw when its root makes KrakenD's router
// unable to serve an entry next to another endpoint's that the stored root
// serves together, as turning router.auto_options on can: one of the two would
// no longer be served. The denial names the endpoints and their paths. While
// the render stops resolving clashes at its cap, a new one cannot be told
// apart, so gw is rejected.
func refuseNewGatewayClashes(ctx context.Context, chk ConfigChecker, old, gw *v1alpha1.KrakenDGateway) error {
	before, err := chk.Conflicts(ctx, old, nil)
	if err != nil {
		return checkErr(err)
	}
	after, err := chk.Conflicts(ctx, gw, nil)
	if err != nil {
		return checkErr(err)
	}
	errs := clashErrors(field.NewPath("spec", "config"), configcheck.NewClashes(before, after, nil), after.Capped)
	if len(errs) == 0 {
		return nil
	}
	return invalid("KrakenDGateway", gw.Name, errs)
}

// versionWarning warns, when spec.version is set or changed, that gw runs a
// KrakenD minor version other than the one admission and the controller
// validate with: their checks may not match what that version accepts.
func versionWarning(gw, old *v1alpha1.KrakenDGateway) admission.Warnings {
	if old != nil && old.Spec.Version == gw.Spec.Version {
		return nil
	}
	if gw.Spec.Version == "" {
		return admission.Warnings{fmt.Sprintf("spec.version is empty: the image tag is empty unless spec.image "+
			"(and spec.ceImage for the CE fallback) is set, so the KrakenD version is unknown; "+
			"configs are validated with KrakenD %s", configcheck.ValidatorVersion)}
	}
	parts := strings.SplitN(strings.TrimPrefix(gw.Spec.Version, "v"), ".", 3)
	if len(parts) >= 2 && parts[0]+"."+parts[1] == configcheck.ValidatorVersion {
		return nil
	}
	version := truncate(gw.Spec.Version, echoLimit)
	return admission.Warnings{fmt.Sprintf("spec.version %s: configs are validated with KrakenD %s; "+
		"the checks may not match what %s accepts", version, configcheck.ValidatorVersion, version)}
}

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// authorizePostRestartJob requires the requester to be able to create pods in
// the gateway's namespace when an enabled spec.postRestartJob would run as a
// ServiceAccount other than the gateway's own, read a Secret, or relax the
// default security context. The operator creates the Job with its own grant,
// so without this check a gateway writer would borrow whatever
// ServiceAccount of the namespace they name.
func (v *GatewayValidator) authorizePostRestartJob(ctx context.Context, old, gw *v1alpha1.KrakenDGateway) error {
	prj := gw.Spec.PostRestartJob
	if prj == nil || !prj.Enabled {
		return nil
	}
	if !postRestartJobBorrowsRights(prj, gw) {
		return nil
	}
	if old != nil && equality.Semantic.DeepEqual(old.Spec.PostRestartJob, prj) {
		return nil // the requester did not touch it
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return unavailable(err)
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(req.UserInfo.Extra))
	for k, val := range req.UserInfo.Extra {
		extra[k] = authorizationv1.ExtraValue(val)
	}
	createPods := &authorizationv1.ResourceAttributes{Namespace: gw.Namespace, Verb: "create", Resource: "pods"}
	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: req.UserInfo.Username, Groups: req.UserInfo.Groups, UID: req.UserInfo.UID, Extra: extra,
		ResourceAttributes: createPods,
	}}
	if err := v.Create(ctx, sar); err != nil {
		return unavailable(fmt.Errorf("reviewing the requester's access: %w", err))
	}
	if !sar.Status.Allowed {
		gr := schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "krakendgateways"}
		return apierrors.NewForbidden(gr, gw.Name, fmt.Errorf(
			"spec.postRestartJob: %s may not create pods in namespace %s, so the post-restart Job may not "+
				"run as another ServiceAccount, read a Secret, or relax the operator's default security context",
			req.UserInfo.Username, gw.Namespace))
	}
	return nil
}

// postRestartJobBorrowsRights reports whether the Job would run as a
// ServiceAccount other than the gateway's own, read a Secret through envFrom
// or an env secretKeyRef, or run with a security setting outside the
// raisesPrivileges allow-list. podLabels and the other podAnnotations are not
// reviewed.
func postRestartJobBorrowsRights(prj *v1alpha1.PostRestartJobSpec, gw *v1alpha1.KrakenDGateway) bool {
	if prj.ServiceAccountName != "" && prj.ServiceAccountName != gw.Name {
		return true
	}
	if raisesPrivileges(prj) {
		return true
	}
	return slices.ContainsFunc(prj.EnvFrom, func(e corev1.EnvFromSource) bool { return e.SecretRef != nil }) ||
		slices.ContainsFunc(prj.Env, func(e corev1.EnvVar) bool {
			return e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil
		})
}

// raisesPrivileges reports whether the Job's security contexts or pod
// annotations set anything beyond an allow-list of settings that grant no
// privilege: the run-as identity (root included: validatePostRestartRunAsRoot
// makes it an acknowledged choice), the group and filesystem-group settings,
// readOnlyRootFilesystem, allowPrivilegeEscalation false, privileged false,
// procMount Default, a capabilities drop that keeps ALL, and the
// runtime-default seccomp and AppArmor profiles. Any
// other field counts, including one a later Kubernetes release adds, and so
// does the deprecated AppArmor pod annotation, which the kubelet still honors.
func raisesPrivileges(prj *v1alpha1.PostRestartJobSpec) bool {
	if sc := prj.SecurityContext; sc != nil {
		rest := *sc
		rest.RunAsUser, rest.RunAsGroup, rest.RunAsNonRoot, rest.ReadOnlyRootFilesystem = nil, nil, nil, nil
		if rest.AllowPrivilegeEscalation != nil && !*rest.AllowPrivilegeEscalation {
			rest.AllowPrivilegeEscalation = nil
		}
		if rest.Privileged != nil && !*rest.Privileged {
			rest.Privileged = nil
		}
		if rest.ProcMount != nil && *rest.ProcMount == corev1.DefaultProcMount {
			rest.ProcMount = nil
		}
		if c := rest.Capabilities; c != nil && len(c.Add) == 0 && (len(c.Drop) == 0 || slices.Contains(c.Drop, "ALL")) {
			rest.Capabilities = nil
		}
		rest.SeccompProfile = nonDefaultSeccomp(rest.SeccompProfile)
		rest.AppArmorProfile = nonDefaultAppArmor(rest.AppArmorProfile)
		if !equality.Semantic.DeepEqual(rest, corev1.SecurityContext{}) {
			return true
		}
	}
	if psc := prj.PodSecurityContext; psc != nil {
		rest := *psc
		rest.RunAsUser, rest.RunAsGroup, rest.RunAsNonRoot = nil, nil, nil
		rest.FSGroup, rest.FSGroupChangePolicy, rest.SupplementalGroups, rest.SupplementalGroupsPolicy = nil, nil, nil, nil
		rest.SeccompProfile = nonDefaultSeccomp(rest.SeccompProfile)
		rest.AppArmorProfile = nonDefaultAppArmor(rest.AppArmorProfile)
		if !equality.Semantic.DeepEqual(rest, corev1.PodSecurityContext{}) {
			return true
		}
	}
	for key, value := range prj.PodAnnotations {
		if strings.HasPrefix(key, corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix) &&
			value != corev1.DeprecatedAppArmorBetaProfileRuntimeDefault {
			return true
		}
	}
	return false
}

// nonDefaultSeccomp returns p unless it is the runtime-default profile.
func nonDefaultSeccomp(p *corev1.SeccompProfile) *corev1.SeccompProfile {
	if p != nil && p.Type == corev1.SeccompProfileTypeRuntimeDefault {
		return nil
	}
	return p
}

// nonDefaultAppArmor returns p unless it is the runtime-default profile.
func nonDefaultAppArmor(p *corev1.AppArmorProfile) *corev1.AppArmorProfile {
	if p != nil && p.Type == corev1.AppArmorProfileTypeRuntimeDefault {
		return nil
	}
	return p
}
