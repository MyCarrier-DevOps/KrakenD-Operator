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

// Package webhook implements validating admission webhooks for KrakenD CRDs.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// isTerminating reports whether obj is being deleted (has a deletionTimestamp).
func isTerminating(obj runtime.Object) bool {
	o, ok := obj.(metav1.Object)
	return ok && !o.GetDeletionTimestamp().IsZero()
}

// terminatingWithUnchangedSpec reports whether an UPDATE only touches the
// metadata of an object that is being deleted (removing a finalizer, for
// example). Rejecting such an update would leave the object stuck in
// Terminating, so validators admit it. A spec change is not skipped: the
// gateway still renders terminating endpoints and policies, so it must pass
// the usual rules. If the specs cannot be compared, it reports false.
func terminatingWithUnchangedSpec(oldObj, newObj runtime.Object) bool {
	if !isTerminating(newObj) {
		return false
	}
	oldMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(oldObj)
	if err != nil {
		return false
	}
	newMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newObj)
	if err != nil {
		return false
	}
	return equality.Semantic.DeepEqual(oldMap["spec"], newMap["spec"])
}

// GatewayValidator validates KrakenDGateway resources.
type GatewayValidator struct {
	client.Client
	Checker ConfigChecker
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

	warnings, errs := v.validate(gw, old)
	if old != nil {
		errs = newErrors(errs, v.storedErrors(gw, old))
	}
	eeErrs, err := v.eeNamespacesOnCE(ctx, old, gw)
	if err != nil {
		return warnings, unavailable(err)
	}
	errs = append(errs, eeErrs...)
	if len(errs) > 0 {
		return warnings, invalid("KrakenDGateway", gw.Name, errs)
	}
	renderWarnings, err := checkGatewayRender(ctx, v.Checker, old, gw)
	return append(append(warnings, renderWarnings...), versionWarning(gw, old)...), err
}

// eeNamespacesOnCE rejects Enterprise-only extra_config namespaces that a CE
// gateway would accept and then silently ignore, in spec.config.extraConfig.
func (v *GatewayValidator) eeNamespacesOnCE(
	_ context.Context, _, gw *v1alpha1.KrakenDGateway,
) (field.ErrorList, error) {
	return ceIgnores(field.NewPath("spec", "config", "extraConfig"),
		eeOnlyNamespacesIn(gw.Spec.Config.ExtraConfig, renderer.LevelService)), nil
}

// validate runs all admission checks for gw. old is the previously-stored
// object on an Update (nil on Create) — see validatePostRestartJob's
// ratchet handling.
func (v *GatewayValidator) validate(gw, old *v1alpha1.KrakenDGateway) (admission.Warnings, field.ErrorList) {
	var errs field.ErrorList
	var warnings admission.Warnings

	warnings = append(warnings, replicasWithAutoscalingWarning(gw)...)
	warnings = append(warnings, openAPIOnCEWarning(gw)...)
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

// openAPIOnCEWarning warns when spec.openapi enables the export on a CE
// gateway: the CE binary has no openapi command, so the operator runs no
// export or serving there. It is a warning because the spec may already be
// stored, and it takes effect again on an EE gateway.
func openAPIOnCEWarning(gw *v1alpha1.KrakenDGateway) admission.Warnings {
	if gw.Spec.Edition != v1alpha1.EditionCE || gw.Spec.OpenAPI == nil || !gw.Spec.OpenAPI.Enabled {
		return nil
	}
	return admission.Warnings{
		"spec.openapi is ignored on CE gateways: the CE binary cannot export OpenAPI, " +
			"so no export or openapi-serve sidecar runs",
	}
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

// PolicyValidator validates KrakenDBackendPolicy resources.
type PolicyValidator struct {
	client.Client
}

// ValidateCreate admits a new KrakenDBackendPolicy; the CRD schema enforces
// its field rules.
func (v *PolicyValidator) ValidateCreate(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate admits an updated KrakenDBackendPolicy; the CRD schema
// enforces its field rules.
func (v *PolicyValidator) ValidateUpdate(
	context.Context, runtime.Object, runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
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
		client.MatchingFields{fieldindex.EndpointPolicy: indexKey},
	); err != nil {
		return nil, unavailable(fmt.Errorf("listing endpoints: %w", err))
	}

	var references []string
	for _, ep := range endpoints.Items {
		references = append(references, ep.Namespace+"/"+ep.Name)
	}
	sort.Strings(references)

	if len(references) > 0 {
		return nil, invalid("KrakenDBackendPolicy", policy.Name, field.ErrorList{
			field.Forbidden(
				field.NewPath("metadata", "name"),
				fmt.Sprintf("policy is referenced by endpoints: %s",
					strings.Join(references, ", ")),
			),
		})
	}
	return nil, nil
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
	errs, err := v.validateGatewayRef(ctx, ac)
	if err != nil {
		return nil, unavailable(err)
	}
	errs = append(errs, validateFields(ac)...)
	return nil, invalid("KrakenDAutoConfig", ac.Name, errs)
}

// ValidateUpdate validates an updated KrakenDAutoConfig. The gateway
// reference is checked only when it changes, and a field rule rejects the
// update only for errors the stored object did not already have.
func (v *AutoConfigValidator) ValidateUpdate(
	ctx context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	if terminatingWithUnchangedSpec(oldObj, newObj) {
		return nil, nil
	}
	ac, ok := newObj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", newObj)
	}
	old, ok := oldObj.(*v1alpha1.KrakenDAutoConfig)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDAutoConfig, got %T", oldObj)
	}
	if equality.Semantic.DeepEqual(old.Spec, ac.Spec) {
		return nil, nil
	}
	var errs field.ErrorList
	if old.Spec.GatewayRef != ac.Spec.GatewayRef {
		refErrs, err := v.validateGatewayRef(ctx, ac)
		if err != nil {
			return nil, unavailable(err)
		}
		errs = refErrs
	}
	errs = append(errs, newErrors(validateFields(ac), validateFields(old))...)
	return nil, invalid("KrakenDAutoConfig", ac.Name, errs)
}

// ValidateDelete is a no-op for autoconfigs.
func (v *AutoConfigValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

// validateGatewayRef checks that the gateway ac references exists.
func (v *AutoConfigValidator) validateGatewayRef(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
) (field.ErrorList, error) {
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
			return nil, fmt.Errorf("looking up gateway: %w", err)
		}
	}

	return errs, nil
}

// validateFields runs the field rules for ac.
func validateFields(ac *v1alpha1.KrakenDAutoConfig) field.ErrorList {
	var errs field.ErrorList
	for i, ov := range ac.Spec.Overrides {
		errs = append(errs, validateExtraConfigAudience(
			field.NewPath("spec", "overrides").Index(i).Child("extraConfig"),
			ov.ExtraConfig,
		)...)
	}

	if ac.Spec.Defaults != nil && ac.Spec.Defaults.Endpoint != nil {
		errs = append(errs, validateExtraConfigAudience(
			field.NewPath("spec", "defaults", "endpoint", "extraConfig"),
			ac.Spec.Defaults.Endpoint.ExtraConfig,
		)...)
	}

	errs = append(errs, validateAdditionalEndpoints(ac)...)

	return errs
}

// validateAdditionalEndpoints validates the audience in each additional
// endpoint's extraConfig; the CRD enforces the rest.
func validateAdditionalEndpoints(ac *v1alpha1.KrakenDAutoConfig) field.ErrorList {
	var errs field.ErrorList
	for i, ae := range ac.Spec.AdditionalEndpoints {
		errs = append(errs, validateExtraConfigAudience(
			field.NewPath("spec", "additionalEndpoints").Index(i).Child("extraConfig"), ae.ExtraConfig)...)
	}
	return errs
}

// validateExtraConfigAudience validates the shape of a
// documentation/openapi.audience value inside an ExtraConfig RawExtension.
// KrakenD's OpenAPI documentation plugin requires audience to be a list of
// strings; a malformed value (e.g. a YAML mapping coerced to a JSON object)
// passes CRD and CUE validation unchanged but fails `krakend check -t -n -c`,
// which blocks config updates for every service on the gateway. Invalid JSON
// and an absent documentation/openapi block or audience key are not this
// helper's concern.
func validateExtraConfigAudience(p *field.Path, ec *runtime.RawExtension) field.ErrorList {
	var errs field.ErrorList

	if ec == nil || len(ec.Raw) == 0 {
		return errs
	}

	var blocks map[string]json.RawMessage
	if err := json.Unmarshal(ec.Raw, &blocks); err != nil {
		return errs
	}

	openapiRaw, ok := blocks["documentation/openapi"]
	if !ok {
		return errs
	}

	var openapi map[string]json.RawMessage
	if err := json.Unmarshal(openapiRaw, &openapi); err != nil {
		return errs
	}

	audienceRaw, ok := openapi["audience"]
	if !ok {
		return errs
	}

	// Decode into pointers so JSON null — the whole value or an item —
	// is caught: KrakenD rejects both, but json.Unmarshal would turn them
	// into a nil slice and "" respectively.
	var audience []*string
	if err := json.Unmarshal(audienceRaw, &audience); err != nil || audience == nil || slices.Contains(audience, nil) {
		errs = append(errs, field.Invalid(
			p.Key(`"documentation/openapi"`).Child("audience"),
			string(audienceRaw),
			`must be a list of strings, e.g. ["internal"]`,
		))
	}

	return errs
}

// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendgateway,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendgateways,verbs=create;update,versions=v1alpha1,name=vkrakendgateway.kb.io,admissionReviewVersions=v1,timeoutSeconds=15
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendendpoint,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendendpoints,verbs=create;update,versions=v1alpha1,name=vkrakendendpoint.kb.io,admissionReviewVersions=v1,timeoutSeconds=15
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendbackendpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=create;update;delete,versions=v1alpha1,name=vkrakendbackendpolicy.kb.io,admissionReviewVersions=v1,timeoutSeconds=15
// +kubebuilder:webhook:path=/validate-gateway-krakend-io-v1alpha1-krakendautoconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=gateway.krakend.io,resources=krakendautoconfigs,verbs=create;update,versions=v1alpha1,name=vkrakendautoconfig.kb.io,admissionReviewVersions=v1,timeoutSeconds=15

// Validators are the validating webhooks SetupWebhooks registers.
type Validators struct {
	Gateway    *GatewayValidator
	Endpoint   *EndpointValidator
	Policy     *PolicyValidator
	AutoConfig *AutoConfigValidator
}

// NewValidators builds the validators over c. checker is the config checker
// the gateway controller uses too, so the validators and the controller share
// its validation slots.
// operatorUsername is the username of the operator's own requests; see
// EndpointValidator.OperatorUsername.
func NewValidators(c client.Client, checker ConfigChecker, operatorUsername string) Validators {
	return Validators{
		Gateway:    &GatewayValidator{Client: c, Checker: checker},
		Endpoint:   &EndpointValidator{Client: c, Checker: checker, OperatorUsername: operatorUsername},
		Policy:     &PolicyValidator{Client: c},
		AutoConfig: &AutoConfigValidator{Client: c},
	}
}

// SetupWebhooks registers all validating webhooks with the manager. validators
// come from NewValidators, built over the config checker the gateway
// controller uses too, so both share its validation slots.
func SetupWebhooks(mgr ctrl.Manager, validators Validators) error {
	// Ensure field indexes are registered — needed for conflict detection
	// and policy-delete validation even when running webhook-only.
	if err := fieldindex.EnsureEndpointIndexes(mgr); err != nil {
		return fmt.Errorf("registering endpoint indexes: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDGateway{}).
		WithValidator(validators.Gateway).
		Complete(); err != nil {
		return fmt.Errorf("setting up gateway webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDEndpoint{}).
		WithValidator(validators.Endpoint).
		Complete(); err != nil {
		return fmt.Errorf("setting up endpoint webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDBackendPolicy{}).
		WithValidator(validators.Policy).
		Complete(); err != nil {
		return fmt.Errorf("setting up policy webhook: %w", err)
	}

	if err := ctrl.NewWebhookManagedBy(mgr).
		For(&v1alpha1.KrakenDAutoConfig{}).
		WithValidator(validators.AutoConfig).
		Complete(); err != nil {
		return fmt.Errorf("setting up autoconfig webhook: %w", err)
	}

	return nil
}
