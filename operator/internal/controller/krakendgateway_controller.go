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

package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"slices"
	"time"

	"go.opentelemetry.io/otel/trace"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	utilclock "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
	"github.com/mycarrier-devops/krakend-operator/internal/util/license"
)

// KrakenDGatewayReconciler reconciles a KrakenDGateway object.
// It orchestrates the full rendering pipeline and manages all owned
// Kubernetes resources.
type KrakenDGatewayReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Renderer renderer.Renderer
	// Checker gathers the render inputs and validates the render, behind the
	// slots every config check in the pod shares.
	Checker ConfigChecker
	Clock   utilclock.Clock
	// APIReader reads uncached from the API server (ReplicaSets for config GC)
	APIReader client.Reader
	// LicenseParser reads EE license certificates
	LicenseParser license.LicenseParser
	// MaxConcurrentReconciles is how many gateways reconcile at once; zero
	// means one. Each reconcile holds one config checker slot at a time.
	MaxConcurrentReconciles int
	// Metrics records the controller's metrics; nil records nothing.
	Metrics GatewayMetrics
	// Tracer records the reconcile's spans; nil records none.
	Tracer trace.Tracer

	// verdicts remembers, per gateway, the config checks its last pass ran,
	// so a gateway whose inputs did not change runs none.
	verdicts verdictMemo

	// cachedOptionalKinds are the optional kinds whose CRDs were installed
	// at startup, so an informer runs for them (the Owns watches).
	// optionalCache reads them through that informer. A kind outside the set
	// is read live.
	cachedOptionalKinds map[schema.GroupVersionKind]struct{}
	optionalCache       client.Reader
	// absentKinds remembers the optional kinds discovery found absent, for
	// the delete path of a disabled feature only.
	absentKinds absentKindMemo
	// verified remembers which version of each config ConfigMap had its
	// payload hashed, so a steady pass does not read it again.
	verified verifiedConfigMaps
}

// ConfigChecker gathers a gateway's render inputs and judges them: the
// gateway root alone, each endpoint alone, and the whole render. The gateway
// controller owns this port; configcheck.Checker is its implementation. Each
// check answers from the memo it is handed when it already judged the same
// content.
type ConfigChecker interface {
	Gather(ctx context.Context, gw *v1alpha1.KrakenDGateway,
		replace []v1alpha1.KrakenDEndpoint) (renderer.RenderInput, error)
	CheckRoot(ctx context.Context, root configcheck.Root, memo configcheck.Memo) (configcheck.Verdict, error)
	CheckEndpoint(ctx context.Context, u configcheck.EndpointUnit,
		memo configcheck.Memo) (configcheck.EndpointVerdict, error)
	CheckRendered(ctx context.Context, in renderer.RenderInput, out *renderer.RenderOutput,
		memo configcheck.Memo) (configcheck.Verdict, error)
}

// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways/status,verbs=update
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints/status,verbs=patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=get;list;watch
// The controller never deletes a Deployment, Service, ServiceAccount or
// PodDisruptionBudget, but the OwnerReferencesPermissionEnforcement admission
// plugin requires delete on an object whose ownerReferences an update
// changes, as when a gateway adopts a same-named object that predates it.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=list
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=dragonflydb.io,resources=dragonflies,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices,verbs=get;list;watch;create;update;delete

// Reconcile implements the gateway rendering pipeline: gather inputs,
// render config, validate, update resource, and reconcile owned objects.
func (r *KrakenDGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, retErr error) {
	ctx, span := startReconcile(ctx, r.Tracer, "KrakenDGateway", req)
	defer func() { tracing.End(span, retErr) }()
	log := logf.FromContext(ctx)
	start := time.Now()
	// A gateway that is gone or terminating is forgotten below and observes no
	// duration: the histogram cannot drop its series, so it must at least stop
	// changing.
	recordDuration := true
	defer func() {
		if recordDuration {
			r.metrics().GatewayReconciled(ctx, req.NamespacedName, time.Since(start))
		}
	}()

	var gw v1alpha1.KrakenDGateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		if errors.IsNotFound(err) {
			recordDuration = false
			r.forgetGateway(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting gateway %s: %w", req.NamespacedName, err)
	}
	spanGeneration(ctx, gw.Generation)

	// A terminating gateway is left alone: under foreground deletion it
	// lingers while garbage collection removes its children, and converging
	// would recreate each one as it goes.
	if !gw.DeletionTimestamp.IsZero() {
		recordDuration = false
		r.forgetGateway(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Status as read, so each write below happens only when it changes.
	before := gw.Status.DeepCopy()

	// Gather the endpoints and the policies they reference, in the order the
	// render is deterministic for.
	in, err := r.Checker.Gather(ctx, &gw, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("gathering render inputs: %w", err)
	}
	endpoints := in.Endpoints

	// Read the deployed license first: a failed read must not follow license
	// transitions (and their events) that the returned error would discard.
	deployed, err := r.deployedChecksums(ctx, &gw)
	if err != nil {
		return ctrl.Result{}, err
	}
	// The license decides whether this gateway renders and runs CE.
	lic := r.reconcileLicense(ctx, &gw)
	ceFallback := lic.ceFallback
	in.CEFallback = ceFallback // this reconcile's license verdict, fresher than status
	edition := renderer.EditionFor(&gw, ceFallback)
	licenseChecksum := lic.checksumFor(deployed.license)

	// Gather plugin ConfigMaps
	pluginConfigMaps, missingPlugins, err := r.gatherPluginConfigMaps(ctx, &gw)
	if err != nil {
		return ctrl.Result{}, err
	}
	pluginsHeldBefore := condFalse(meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionPluginsResolved))
	r.setPluginsResolved(&gw, missingPlugins)

	// Detect Dragonfly state
	in.Dragonfly = r.detectDragonflyState(ctx, &gw)
	in.PluginConfigMaps = pluginConfigMaps

	// Render configuration
	output, err := r.Renderer.Render(in)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rendering config: %w", err)
	}
	r.metrics().ConfigRendered(ctx)

	// Config stage: decide and publish the applied config. Its error is
	// returned only after the infrastructure stage and the status write.
	// A status written before configEdition existed is adopted once, whatever
	// the verdict on this render, so a later edition change cannot re-read it.
	if gw.Status.ConfigChecksum != "" && gw.Status.ConfigEdition == "" {
		gw.Status.ConfigEdition = appliedKey(&gw, edition).edition
	}
	appliedBefore := appliedKey(&gw, edition)
	cfg, configErr := r.reconcileConfig(ctx, &gw, before, in, output, edition)
	// From here on the pass reports on the render the config stage settled
	// on: the applied one, after exclusion, or the newest when nothing was
	// applied.
	output = cfg.output
	r.reconcileCEFallbackCondition(&gw, output, edition)
	image := appliedImage(&gw, edition)
	configChanged := appliedKey(&gw, edition) != appliedBefore ||
		// The hold just lifted: the config applied meanwhile starts rolling now.
		pluginsHeldBefore && cfg.appliedConfigMap != "" && deployed.config != gw.Status.ConfigChecksum

	// Accepted: the applied render sets every endpoint's verdict, and an
	// endpoint that fails on its own gets Accepted=False with its reason. On a
	// pass that applies nothing (a rejected render, or a validator that could
	// not judge), an endpoint that fails on its own gets the same verdict,
	// worded for a config not yet applied, and one every endpoint was judged
	// against loses an exclusion it no longer earns; while no config has ever
	// been applied, every other endpoint loses its Accepted. The gateway's
	// EndpointsExcluded condition and gauge follow. A failed endpoint status
	// write does not stop the infrastructure stage or the gateway status; it
	// is returned after them so the reconcile is retried.
	var (
		acceptanceErr error
		decided       map[types.NamespacedName]*metav1.Condition
	)
	if cfg.served {
		decided, acceptanceErr = r.reconcileEndpointAcceptance(ctx, &gw, endpoints, output, cfg.excluded)
	} else {
		never, neverErr := r.neverApplied(ctx, &gw)
		var recordErr error
		decided, recordErr = r.recordExclusions(ctx, &gw, endpoints, cfg, never)
		acceptanceErr = stderrors.Join(neverErr, recordErr)
	}
	r.reportExclusions(&gw, endpoints, decided, cfg.served)

	// Infrastructure stage: always runs, and deploys the applied config.
	infra := infraInputs{
		appliedChecksum: gw.Status.ConfigChecksum,
		pluginChecksum:  output.PluginChecksum,
		licenseChecksum: licenseChecksum,
		image:           image,
		ceRender:        appliedKey(&gw, edition).edition == v1alpha1.EditionCE,
		configMapName:   cfg.appliedConfigMap,
		heldBecause:     cfg.heldBecause,
		missingPlugins:  missingPlugins,
	}
	saControlled, coreErr := r.reconcileCoreResources(ctx, &gw, infra)
	note := r.noteRollout(&gw, infra, deployed, configChanged, saControlled)
	obs, infraErr := r.reconcileInfrastructure(ctx, &gw, infra, saControlled, coreErr)
	r.inspectDeploymentStatus(ctx, &gw, infra, obs, note)

	// Update final status
	gw.Status.EndpointCount = int32(len(endpoints))
	r.recordGatewayMetrics(&gw, len(endpoints))
	// This generation is not applied while a child resource cannot be
	// reconciled, nor while the Deployment is held because the applied
	// config's ConfigMap cannot be published or verified although the render
	// is the applied config. A rejected render, an unavailable validator and a
	// missing plugin ConfigMap do not hold it back: they are verdicts on this
	// generation that Ready already reports.
	appliedHeld := cfg.served && cfg.appliedConfigMap == ""
	observed := gw.Generation
	if infraErr != nil || appliedHeld {
		observed = before.ObservedGeneration
	}
	setGatewayReadiness(&gw, observed)

	if err := r.updateStatusIfChanged(ctx, &gw, before); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating gateway status: %w", err)
	}
	if err := stderrors.Join(configErr, acceptanceErr, infraErr); err != nil {
		return ctrl.Result{}, err
	}

	log.V(1).Info("gateway reconciled",
		"phase", gw.Status.Phase,
		"checksum", gw.Status.ConfigChecksum,
		"endpoints", gw.Status.EndpointCount)
	return ctrl.Result{RequeueAfter: lic.requeueAfter}, nil
}

// SetupWithManager sets up the controller with the Manager. The optional
// third-party kinds (Dragonfly, ExternalSecret, VirtualService) are
// registered with Owns() when their CRDs exist at startup. A CRD installed
// later is watched only after an operator restart; until then its objects
// are still reconciled on every gateway event, and garbage collected through
// their owner references.
//
// Escape-hatch watch dependency (review id 3805157515, #11): the
// docs/upgrade-guide.md status-patch escape hatch
// (`kubectl patch krakendgateway <name> --subresource=status --type=merge
// -p '{"status":{"lastPostRestartJobChecksum":""}}'`) relies on the
// primary For(&v1alpha1.KrakenDGateway{}) watch below enqueuing an
// IMMEDIATE reconcile for that status-only change. This currently works
// only because the For() watch below carries NO predicate at all — unlike
// the Watches() calls further down, which deliberately use
// predicate.GenerationChangedPredicate{} to filter out status-only churn
// (status/subresource updates do not bump .metadata.generation) for
// SECONDARY resources. If a future change added a
// GenerationChangedPredicate to the primary For() watch too (for example
// to cut reconcile volume from the operator's own status writes, which
// updateStatusIfChanged already limits to real changes), a status-only
// escape-hatch patch would stop triggering an
// immediate reconcile and instead silently degrade to "whenever the next
// unrelated event happens to fire" — the escape hatch would still
// eventually work, just not on-demand. Keep this in mind before adding a
// predicate to the primary watch.
func (r *KrakenDGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := fieldindex.EnsureEndpointIndexes(mgr); err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KrakenDGateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}, builder.OnlyMetadata).
		Owns(&corev1.ServiceAccount{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&batchv1.Job{}).
		Watches(
			&v1alpha1.KrakenDEndpoint{},
			handler.EnqueueRequestsFromMapFunc(r.endpointToGateway),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&v1alpha1.KrakenDBackendPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToGateways),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.licenseSecretToGateway),
			builder.OnlyMetadata,
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.pluginConfigMapToGateway),
			builder.OnlyMetadata,
		).
		WithOptions(crcontroller.Options{
			RateLimiter:             newGatewayRateLimiter(),
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
		}).
		Named("krakendgateway")

	installed, missing, err := installedOptionalKinds(mgr.GetRESTMapper())
	if err != nil {
		return err
	}
	log := mgr.GetLogger().WithName("krakendgateway")
	r.optionalCache = mgr.GetCache()
	r.cachedOptionalKinds = make(map[schema.GroupVersionKind]struct{}, len(installed))
	for _, gvk := range installed {
		r.cachedOptionalKinds[gvk] = struct{}{}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		b = b.Owns(u)
		log.Info("watching optional kind", "kind", gvk.String())
	}
	for _, gvk := range missing {
		log.Info("optional CRD not installed at startup; restart the operator after installing it to watch it",
			"kind", gvk.String())
	}
	return b.Complete(r)
}

// forgetGateway drops what the controller keeps per gateway once the gateway
// is gone or terminating: its metric series, except the reconcile-duration
// histogram's, which cannot drop one, and its remembered verdicts.
func (r *KrakenDGatewayReconciler) forgetGateway(key types.NamespacedName) {
	r.metrics().ForgetGateway(key)
	r.verdicts.forget(key)
	r.verified.forgetGateway(key)
}

// crdAvailable checks whether the given GVK is registered in the cluster's
// API discovery. Returns (false, nil) when the CRD is simply not installed,
// and (false, err) for transient or unexpected errors.
func (r *KrakenDGatewayReconciler) crdAvailable(gvk schema.GroupVersionKind) (bool, error) {
	ok, err := kindInstalled(r.RESTMapper(), gvk)
	if err != nil {
		return false, fmt.Errorf("checking CRD availability for %s: %w", gvk, err)
	}
	return ok, nil
}

// gatherPluginConfigMaps fetches ConfigMaps referenced by plugin sources and
// names the ones that do not exist.
func (r *KrakenDGatewayReconciler) gatherPluginConfigMaps(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
) (found []corev1.ConfigMap, missing []string, err error) {
	ctx, span := tracing.Start(ctx, r.Tracer, "gateway.plugins")
	defer func() { tracing.End(span, err) }()
	if gw.Spec.Plugins == nil {
		return nil, nil, nil
	}
	for _, src := range gw.Spec.Plugins.Sources {
		if src.ConfigMapRef == nil {
			continue
		}
		var cm corev1.ConfigMap
		key := types.NamespacedName{Name: src.ConfigMapRef.Name, Namespace: gw.Namespace}
		if err := r.Get(ctx, key, &cm); err != nil {
			if errors.IsNotFound(err) {
				if !slices.Contains(missing, src.ConfigMapRef.Name) {
					missing = append(missing, src.ConfigMapRef.Name)
				}
				continue
			}
			return nil, nil, fmt.Errorf("getting plugin configmap %s: %w", key, err)
		}
		found = append(found, cm)
	}
	return found, missing, nil
}

// detectDragonflyState checks if a Dragonfly CR exists and reports its readiness.
// It returns nil if Dragonfly is not enabled, and sets the DragonflyReady
// condition and metric on the gateway.
func (r *KrakenDGatewayReconciler) detectDragonflyState(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
) *renderer.DragonflyState {
	if gw.Spec.Dragonfly == nil || !gw.Spec.Dragonfly.Enabled {
		return nil
	}

	log := logf.FromContext(ctx)
	available, err := r.crdAvailable(dragonflyGVK)
	if err != nil {
		log.Error(err, "failed to check Dragonfly CRD availability")
		return nil
	}
	if !available {
		r.setConditionWithEvent(gw, metav1.Condition{
			Type:               v1alpha1.ConditionDragonflyReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonCRDNotInstalled,
			Message:            "Dragonfly is enabled but the dragonflydb.io Dragonfly CRD is not installed in the cluster",
		})
		r.metrics().SetDragonflyReady(client.ObjectKeyFromObject(gw), false)
		return nil
	}

	dfName := resources.DragonflyName(gw)
	df := &unstructured.Unstructured{}
	df.SetGroupVersionKind(dragonflyGVK)

	key := types.NamespacedName{Name: dfName, Namespace: gw.Namespace}
	if err := r.Get(ctx, key, df); err != nil {
		if errors.IsNotFound(err) {
			r.setConditionWithEvent(gw, metav1.Condition{
				Type:               v1alpha1.ConditionDragonflyReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gw.Generation,
				Reason:             v1alpha1.ReasonDragonflyNotReady,
				Message:            "Dragonfly CR not yet created",
			})
			r.metrics().SetDragonflyReady(client.ObjectKeyFromObject(gw), false)
			return &renderer.DragonflyState{Enabled: true, ServiceDNS: resources.DragonflyServiceDNS(gw)}
		}
		log.Error(err, "failed to get Dragonfly CR", "name", dfName)
		r.metrics().SetDragonflyReady(client.ObjectKeyFromObject(gw), false)
		return &renderer.DragonflyState{Enabled: true, ServiceDNS: resources.DragonflyServiceDNS(gw)}
	}

	// Check Dragonfly status phase — absent field defaults to empty string
	phase, _, err := unstructured.NestedString(df.Object, "status", "phase")
	if err != nil {
		log.V(1).Info("unable to read Dragonfly status phase", "error", err)
	}
	isReady := phase == "ready"

	if isReady {
		r.setConditionWithEvent(gw, metav1.Condition{
			Type:               v1alpha1.ConditionDragonflyReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             "DragonflyReady",
			Message:            "Dragonfly instance is ready",
		})
		r.metrics().SetDragonflyReady(client.ObjectKeyFromObject(gw), true)
		gw.Status.DragonflyAddress = resources.DragonflyServiceDNS(gw)
	} else {
		r.setConditionWithEvent(gw, metav1.Condition{
			Type:               v1alpha1.ConditionDragonflyReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonDragonflyNotReady,
			Message:            fmt.Sprintf("Dragonfly phase: %s", phase),
		})
		r.metrics().SetDragonflyReady(client.ObjectKeyFromObject(gw), false)
	}

	return &renderer.DragonflyState{Enabled: true, ServiceDNS: resources.DragonflyServiceDNS(gw)}
}

// inspectDeploymentStatus reads the owned Deployment's status and updates
// the gateway's replica counts, Available and Progressing conditions based on
// rollout health; the phase is derived from them. want is what the
// infrastructure stage just deployed, obs what its Deployment step saw, and
// note the reason this pass's change detection chose for a rollout it started.
//
// Progressing follows the Deployment, not the detection of a change: it is
// raised from obs (the object CreateOrUpdate left behind, never the cache) and
// lowered once the Deployment has converged, so a rollout stays reported
// across a failed status write and a lagging cache. A pass that did not
// reconcile the Deployment (held, or the step failed) starts no rollout, so
// it never raises Progressing; it reads the cached Deployment for the rest.
func (r *KrakenDGatewayReconciler) inspectDeploymentStatus(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	want infraInputs,
	obs deploymentObservation,
	note *rolloutNote,
) {
	// The Deployment this pass reconciled is read from what CreateOrUpdate
	// left behind, not from the cache, which can still describe the Deployment
	// from before the write. A pass that did not reconcile it reads the cache.
	dep := obs.dep
	if dep == nil {
		dep = &appsv1.Deployment{}
		key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
		if err := r.Get(ctx, key, dep); err != nil {
			if !errors.IsNotFound(err) {
				logf.FromContext(ctx).Error(err, "failed to get deployment for status inspection")
			}
			return
		}
	}

	// Propagate observed replica counts.
	gw.Status.Replicas = dep.Status.Replicas
	gw.Status.ReadyReplicas = dep.Status.ReadyReplicas

	// A failed Deployment step leaves Progressing and Available as they were:
	// the cached Deployment is from before the write the step could not make.
	if obs.failed {
		return
	}

	// A progress deadline is honoured only while it describes the current
	// rollout (the Deployment controller has observed the latest spec and, on
	// a pass that reconciled the Deployment, the template is the wanted one);
	// otherwise the Available it caused is reset.
	if failedRolloutApplies(dep) && (obs.dep == nil || templateRunsWant(dep, want)) {
		r.setConditionWithEvent(gw, metav1.Condition{
			Type:               v1alpha1.ConditionProgressing,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonRolloutFailed,
			Message:            "Deployment exceeded its progress deadline",
		})
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAvailable,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonRolloutFailed,
			Message:            "Deployment exceeded its progress deadline",
		})
		return
	}
	resetRolloutFailedAvailability(gw, dep)

	converged := deploymentConverged(dep, want)
	switch {
	case converged:
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionProgressing,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             "RolloutComplete",
			Message:            "Deployment rollout completed successfully",
		})
	case rolloutInFlight(obs, want):
		raiseProgressing(gw, note)
	}

	// Mirror a lost Deployment availability, but not while a rollout is
	// still in flight: a new Deployment is unavailable until its pods start.
	if depAvailable := findDeploymentCondition(dep, appsv1.DeploymentAvailable); depAvailable != nil &&
		depAvailable.Status == corev1.ConditionFalse &&
		(converged || !condTrue(meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionProgressing))) {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAvailable,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             depAvailable.Reason,
			Message:            depAvailable.Message,
		})
		return
	}
	if converged {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             "DeploymentAvailable",
			Message:            "All replicas are available",
		})
	}
}

// deploymentConverged reports whether dep has finished rolling out want: its
// pod template is the wanted one (templateRunsWant), it has observed its
// latest spec, and every replica is updated and available. The cache can still
// hold the Deployment from before an update, or one whose status describes the
// previous ReplicaSet, so the replica counts alone are not proof.
func deploymentConverged(dep *appsv1.Deployment, want infraInputs) bool {
	if !templateRunsWant(dep, want) {
		return false
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	return dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.Replicas == desired &&
		dep.Status.UpdatedReplicas == desired &&
		dep.Status.AvailableReplicas == desired
}

// deployedChecksums are the config and license checksums the gateway
// Deployment's pod template carries now; "" for one it carries none of.
type deployedChecksums struct {
	config, license string
}

// deployedChecksums reads them from the gateway Deployment; both are "" when
// there is no Deployment.
func (r *KrakenDGatewayReconciler) deployedChecksums(
	ctx context.Context, gw *v1alpha1.KrakenDGateway,
) (deployedChecksums, error) {
	var dep appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &dep)
	if errors.IsNotFound(err) {
		return deployedChecksums{}, nil
	}
	if err != nil {
		return deployedChecksums{}, fmt.Errorf("reading the deployed checksums: %w", err)
	}
	annotations := dep.Spec.Template.Annotations
	return deployedChecksums{
		config:  annotations[resources.PostRestartJobChecksumAnnotation],
		license: annotations[resources.LicenseChecksumAnnotation],
	}, nil
}

// findDeploymentCondition returns the Deployment's condition of the given
// type, or nil when it reports none.
func findDeploymentCondition(
	dep *appsv1.Deployment,
	condType appsv1.DeploymentConditionType,
) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == condType {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

// reconcileConfig is the config stage. It decides which render becomes the
// applied config (status.configChecksum), and it is the only code that writes
// config content. The newest render is judged by decide, so an endpoint that
// fails on its own is excluded and the rest applied. Reconcile returns the stage's error only after the infrastructure
// stage has run, so a rejected or unjudged render never stops drift
// correction of the gateway's other resources. The result's output is the
// render the rest of the pass reports on.
func (r *KrakenDGatewayReconciler) reconcileConfig(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	before *v1alpha1.KrakenDGatewayStatus,
	in renderer.RenderInput,
	output *renderer.RenderOutput,
	edition v1alpha1.Edition,
) (configResult, error) {
	d, err := r.decide(ctx, gw, in, output, edition)
	if err != nil {
		res, err := r.keepApplied(ctx, gw, r.handleValidatorUnavailable(gw, before, err))
		res.output = output
		return res, err
	}
	if d.failure != nil {
		r.handleValidationError(gw, before, d.failure.reason, d.failure.message)
		res, err := r.keepApplied(ctx, gw, nil)
		res.output, res.excluded, res.judged = output, d.excluded, d.judged
		return res, err
	}
	if isApplied(gw, d.output, edition) {
		return r.serveApplied(ctx, gw, before, d.output, edition, d.excluded)
	}
	// Publish before recording the checksum as applied: status must never
	// name a config that no ConfigMap holds.
	if err := r.publishConfig(ctx, gw, d.output.JSON, d.output.Checksum); err != nil {
		res, err := r.keepApplied(ctx, gw, r.handleConfigPublishFailed(gw, before, err))
		res.output, res.excluded, res.judged = output, d.excluded, d.judged
		return res, err
	}
	markConfigApplied(gw, d.output.Checksum, edition)
	return configResult{appliedConfigMap: resources.ConfigMapName(gw, d.output.Checksum),
		output: d.output, excluded: d.excluded, judged: d.judged, served: true}, nil
}

// serveApplied is the outcome of a pass whose render, after exclusion, is the
// applied config. It passed validation when it was applied, so a revert to
// it clears a rejection. Recording the edition adopts a status written before
// configEdition existed.
func (r *KrakenDGatewayReconciler) serveApplied(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, before *v1alpha1.KrakenDGatewayStatus,
	output *renderer.RenderOutput, edition v1alpha1.Edition,
	excluded map[types.NamespacedName]configcheck.EndpointVerdict,
) (configResult, error) {
	gw.Status.ConfigEdition = edition
	res, err := r.publishApplied(ctx, gw, output)
	res.output, res.excluded, res.judged, res.served = output, excluded, true, true
	if err != nil {
		// The ConfigMap does not hold this config, so it is not reported as
		// the applied one.
		return res, r.handleConfigPublishFailed(gw, before, err)
	}
	setConfigApplied(gw)
	return res, nil
}

// markConfigApplied makes checksum, validated as edition, the applied config.
func markConfigApplied(gw *v1alpha1.KrakenDGateway, checksum string, edition v1alpha1.Edition) {
	gw.Status.ConfigChecksum = checksum
	gw.Status.ConfigEdition = edition
	setConfigApplied(gw)
}

// noteRollout reports the rollout the infrastructure stage starts for a change
// this pass detected, and returns the reason and message chosen for it. It
// returns nil when the pass detects no change, including while the Deployment
// is held: a held Deployment starts no rollout. A ServiceAccount hold returns
// nil too, and its end reports no config rollout: the Deployment write that
// follows raises Progressing with reason DeploymentUpdated.
func (r *KrakenDGatewayReconciler) noteRollout(
	gw *v1alpha1.KrakenDGateway, in infraInputs, deployed deployedChecksums, configChanged, saControlled bool,
) *rolloutNote {
	switch {
	case !saControlled, len(in.missingPlugins) > 0:
		return nil
	case configChanged:
		return r.reportConfigRollout(gw)
	case in.configMapName != "":
		return r.markDeploymentUpdate(gw, in.image, in.pluginChecksum, in.licenseChecksum != deployed.license)
	}
	return nil
}

// reportConfigRollout records the event for the rollout the infrastructure
// stage starts for a newly applied config and returns the reason and message
// to show for it. The Progressing condition itself follows the Deployment.
func (r *KrakenDGatewayReconciler) reportConfigRollout(gw *v1alpha1.KrakenDGateway) *rolloutNote {
	r.Recorder.Event(gw, corev1.EventTypeNormal, v1alpha1.ReasonConfigDeployed,
		fmt.Sprintf("Configuration updated, checksum: %s", gw.Status.ConfigChecksum))
	return &rolloutNote{reason: v1alpha1.ReasonConfigDeployed, message: "Configuration updated, rolling deployment"}
}

// markDeploymentUpdate describes the rollout the infrastructure stage is about
// to start for an image, plugin or license change when no new config was
// applied. It returns nil when none changed.
func (r *KrakenDGatewayReconciler) markDeploymentUpdate(
	gw *v1alpha1.KrakenDGateway, image, pluginChecksum string, licenseChanged bool,
) *rolloutNote {
	if gw.Status.ConfigChecksum == "" {
		return nil
	}
	imageChanged := image != gw.Status.ActiveImage
	pluginChanged := pluginChecksum != "" && pluginChecksum != gw.Status.PluginChecksum
	if !imageChanged && !pluginChanged && !licenseChanged {
		return nil
	}
	return &rolloutNote{reason: "DeploymentUpdated", message: "Deployment updated for image, plugin or license change"}
}

// handleValidationError records a render none of which can be applied:
// ConfigValid=False with reason and message (bounded by truncateMessage to
// 4 KiB). The Warning event fires only when the recorded verdict changes, so
// a gateway that keeps rendering the same rejected config stays quiet. The
// applied config is left alone; the status, with the derived Ready and phase,
// is written at the end of Reconcile after the infrastructure stage. The
// rejection is persistent and a change to any input re-enqueues the gateway,
// so no error is returned.
func (r *KrakenDGatewayReconciler) handleValidationError(
	gw *v1alpha1.KrakenDGateway, before *v1alpha1.KrakenDGatewayStatus, reason, message string,
) {
	message = truncateMessage(message)
	prev := meta.FindStatusCondition(before.Conditions, v1alpha1.ConditionConfigValid)
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionConfigValid,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gw.Generation,
		Reason:             reason,
		Message:            message,
	})
	if prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason || prev.Message != message {
		r.Recorder.Event(gw, corev1.EventTypeWarning, reason, message)
	}
}

// handleValidatorUnavailable records that the rendered config could not be
// judged: krakend check did not run to completion, or the validation copy
// could not be prepared. The applied config is left as it is, and ConfigValid
// goes Unknown with reason ValidatorUnavailable. The derived Ready and phase
// are written at the end of Reconcile, after the infrastructure stage. It
// returns the error, so the reconcile is retried with backoff.
func (r *KrakenDGatewayReconciler) handleValidatorUnavailable(
	gw *v1alpha1.KrakenDGateway,
	before *v1alpha1.KrakenDGatewayStatus,
	cause error,
) error {
	r.recordConfigUnjudged(gw, before, v1alpha1.ReasonValidatorUnavailable,
		fmt.Sprintf("config validator unavailable, retrying: %v", cause))
	return fmt.Errorf("validating config: %w", cause)
}

// handleConfigPublishFailed records that the rendered config passed
// validation but its ConfigMap could not be published, so it is not the
// applied config. The applied config keeps serving, and ConfigValid goes
// Unknown with reason ConfigPublishFailed so Ready does not report the old
// config as the newest. It returns the error, so the reconcile is retried
// with backoff.
func (r *KrakenDGatewayReconciler) handleConfigPublishFailed(
	gw *v1alpha1.KrakenDGateway,
	before *v1alpha1.KrakenDGatewayStatus,
	cause error,
) error {
	r.recordConfigUnjudged(gw, before, v1alpha1.ReasonConfigPublishFailed,
		fmt.Sprintf("the newest config passed validation but its ConfigMap could not be published, retrying: %v",
			cause))
	return fmt.Errorf("publishing config: %w", cause)
}

// recordConfigUnjudged sets ConfigValid=Unknown with reason and message
// (bounded by truncateMessage), and emits a Warning event when the reason
// changes from the one in before.
func (r *KrakenDGatewayReconciler) recordConfigUnjudged(
	gw *v1alpha1.KrakenDGateway,
	before *v1alpha1.KrakenDGatewayStatus,
	reason, message string,
) {
	message = truncateMessage(message)
	prev := meta.FindStatusCondition(before.Conditions, v1alpha1.ConditionConfigValid)
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionConfigValid,
		Status:             metav1.ConditionUnknown,
		ObservedGeneration: gw.Generation,
		Reason:             reason,
		Message:            message,
	})
	if prev == nil || prev.Reason != reason {
		r.Recorder.Event(gw, "Warning", reason, message)
	}
}

// updateStatusIfChanged writes gw's status only when it differs from
// before, the status read at the start of the reconcile. The primary watch
// has no predicate (the status-patch escape hatch depends on it), so every
// write re-enqueues the gateway; skipping unchanged writes is what lets a
// reconcile settle.
func (r *KrakenDGatewayReconciler) updateStatusIfChanged(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	before *v1alpha1.KrakenDGatewayStatus,
) error {
	if !gatewayStatusChanged(before, &gw.Status) {
		return nil
	}
	return r.Status().Update(ctx, gw)
}

// gatewayStatusChanged reports whether after differs from before.
// Conditions are compared with conditionsEqual, which ignores
// LastTransitionTime.
func gatewayStatusChanged(before, after *v1alpha1.KrakenDGatewayStatus) bool {
	if !conditionsEqual(before.Conditions, after.Conditions) {
		return true
	}
	b, a := *before, *after
	b.Conditions, a.Conditions = nil, nil
	return !equality.Semantic.DeepEqual(b, a)
}

// endpointAccepted returns the gateway's verdict on ep for the render that is
// now its applied configuration:
//   - True/Accepted when every entry of ep is included;
//   - True/PartiallyAccepted when an older KrakenDEndpoint (or an earlier
//     entry of ep itself) won some but not all of its routes;
//   - False/EndpointConflict when it won all of them;
//   - reason EEFeaturesStripped when a CE-fallback render removed Enterprise-only
//     features from it (False when nothing of it is served);
//   - False with reason EndpointInvalid or PolicyInvalid when the render left ep
//     out because it fails validation on its own (exclusionCondition).
//
// status.conflicts lists the lost entries. The condition is nil when the
// render excluded ep because a policy it references is missing: the endpoint
// controller reports that through ResolvedRefs, and a leftover Accepted=True
// would claim the endpoint is served.
func endpointAccepted(gw *v1alpha1.KrakenDGateway, ep *v1alpha1.KrakenDEndpoint, rv renderVerdicts) acceptance {
	key := client.ObjectKeyFromObject(ep)
	if _, ok := rv.unresolved[key]; ok {
		return acceptance{}
	}
	if v, ok := rv.excluded[key]; ok {
		return acceptance{condition: exclusionCondition(gw, ep, v, true)}
	}
	cond := &metav1.Condition{
		Type:               v1alpha1.ConditionAccepted,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ep.Generation,
		Reason:             v1alpha1.ReasonAccepted,
		Message:            fmt.Sprintf("Included in the configuration of gateway %s/%s", gw.Namespace, gw.Name),
	}
	stripped := rv.stripped[key]
	if _, ok := rv.conflicted[key]; !ok {
		if len(stripped) > 0 {
			eeStripped(cond, ep, stripped)
		}
		return acceptance{condition: cond}
	}
	lost := rv.lost[key]
	// The CE fallback strips wildcards after the conflicts are settled, so a
	// stripped entry is one that won its pair and is not served either.
	served := entryCount(ep) - lostEntryCount(lost) - wildcardCount(stripped)
	if total := entryCount(ep); len(lost) > 0 && served > 0 {
		cond.Reason = v1alpha1.ReasonPartiallyAccepted
		cond.Message = fmt.Sprintf(
			"%d of %d entries are served on gateway %s/%s; status.conflicts lists the entries an older "+
				"KrakenDEndpoint, or an earlier entry of this one, serves", served, total, gw.Namespace, gw.Name)
		noteStripped(cond, stripped)
		return acceptance{condition: cond, conflicts: endpointConflicts(lost)}
	}
	cond.Status = metav1.ConditionFalse
	cond.Reason = v1alpha1.ReasonEndpointConflict
	cond.Message = fmt.Sprintf(
		"Entries conflict with an older KrakenDEndpoint, or an earlier entry of this one, on gateway %s/%s; "+
			"the conflicting entries are not served",
		gw.Namespace, gw.Name)
	noteStripped(cond, stripped)
	return acceptance{condition: cond, conflicts: endpointConflicts(lost)}
}

// lostEntryCount is the number of distinct (endpoint, method) entries in lost,
// which lists an entry once for each older endpoint it lost to.
func lostEntryCount(lost []renderer.EntryConflict) int {
	seen := map[[2]string]struct{}{}
	for _, l := range lost {
		seen[[2]string{l.Endpoint, l.Method}] = struct{}{}
	}
	return len(seen)
}

// eeStripped makes cond the verdict for an endpoint that a CE-fallback render
// removed Enterprise-only features from. It stays True while some entry is
// still served, and turns False when every entry was an EE wildcard.
func eeStripped(cond *metav1.Condition, ep *v1alpha1.KrakenDEndpoint, stripped []renderer.StrippedEEFeature) {
	if wildcardCount(stripped) >= entryCount(ep) {
		cond.Status = metav1.ConditionFalse
	}
	cond.Reason = v1alpha1.ReasonEEFeaturesStripped
	cond.Message = truncateMessage("The gateway runs KrakenD CE in license fallback, which removed these " +
		"Enterprise-only features:\n" + strippedList(stripped))
}

// wildcardCount is how many of features are removed EE wildcard entries.
func wildcardCount(features []renderer.StrippedEEFeature) int {
	n := 0
	for _, f := range features {
		if f.Feature == renderer.FeatureWildcardEndpoint {
			n++
		}
	}
	return n
}

// noteStripped appends what a CE-fallback render removed to a conflict
// verdict, which keeps its reason.
func noteStripped(cond *metav1.Condition, stripped []renderer.StrippedEEFeature) {
	if len(stripped) > 0 {
		cond.Message = truncateMessage(cond.Message + "\nCE fallback also removed:\n" + strippedList(stripped))
	}
}

// namespacedNameSet returns names as a set.
func namespacedNameSet(names []types.NamespacedName) map[types.NamespacedName]struct{} {
	set := make(map[types.NamespacedName]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// reconcileEndpointAcceptance writes the gateway's Accepted verdict on every
// endpoint of this render. It is called only when the render is the gateway's
// applied configuration. Every endpoint is attempted, and the errors are
// returned together. An endpoint that would be plain Accepted gets reason
// SchemaNameConflict, still True, when the documentation takes a component
// schema it defines from another endpoint; every other verdict outranks it.
func (r *KrakenDGatewayReconciler) reconcileEndpointAcceptance(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	endpoints []v1alpha1.KrakenDEndpoint,
	output *renderer.RenderOutput,
	excluded map[types.NamespacedName]configcheck.EndpointVerdict,
) (map[types.NamespacedName]*metav1.Condition, error) {
	rv := newRenderVerdicts(output, excluded)
	schemaMsgs := schemaConflictMessages(output.SchemaConflicts)
	decided := map[types.NamespacedName]*metav1.Condition{}
	var errs []error
	for i := range endpoints {
		key := client.ObjectKeyFromObject(&endpoints[i])
		a := endpointAccepted(gw, &endpoints[i], rv)
		if msg, ok := schemaMsgs[key]; ok &&
			a.condition != nil && a.condition.Reason == v1alpha1.ReasonAccepted {
			a.condition.Reason, a.condition.Message = v1alpha1.ReasonSchemaNameConflict, msg
		}
		decided[key] = a.condition
		if err := r.writeEndpointAccepted(ctx, &endpoints[i], a, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return decided, utilerrors.NewAggregate(errs)
}

// writeEndpointAccepted sets the verdict a on the endpoint the render saw as
// rendered: its Accepted condition, removed when a.condition is nil, and its
// status.conflicts. It reads the
// endpoint again and patches its status with an optimistic lock, so a write
// never replaces conditions the endpoint controller set after the read; a
// Conflict is retried against a new read. It writes only when the condition
// changes, and emits an event only on a transition. When a.condition is nil, an
// Accepted condition is removed only if removable (nil: always) accepts the
// live one, so a stale caller cannot remove a verdict it did not see. An endpoint deleted, or
// deleted and created again, since the render is skipped.
func (r *KrakenDGatewayReconciler) writeEndpointAccepted(
	ctx context.Context,
	rendered *v1alpha1.KrakenDEndpoint,
	a acceptance,
	removable func(live *metav1.Condition) bool,
) error {
	key := client.ObjectKeyFromObject(rendered)
	var (
		ep    v1alpha1.KrakenDEndpoint
		prev  *metav1.Condition
		wrote bool
	)
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		ep, wrote = v1alpha1.KrakenDEndpoint{}, false
		if err := r.Get(ctx, key, &ep); err != nil {
			return err
		}
		if ep.UID != rendered.UID {
			return nil
		}
		base := ep.DeepCopy()
		prev = meta.FindStatusCondition(base.Status.Conditions, v1alpha1.ConditionAccepted)
		if a.condition == nil {
			if removable != nil && !removable(prev) {
				return nil
			}
			meta.RemoveStatusCondition(&ep.Status.Conditions, v1alpha1.ConditionAccepted)
		} else {
			meta.SetStatusCondition(&ep.Status.Conditions, *a.condition)
		}
		if !a.keepConflicts {
			ep.Status.Conflicts = a.conflicts
		}
		if conditionsEqual(base.Status.Conditions, ep.Status.Conditions) &&
			equality.Semantic.DeepEqual(base.Status.Conflicts, ep.Status.Conflicts) {
			return nil
		}
		if err := r.Status().Patch(ctx, &ep,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("writing Accepted on endpoint %s: %w", key, err)
	}
	if wrote && a.condition != nil {
		recordAcceptedTransition(r.Recorder, &ep, prev, *a.condition)
	}
	return nil
}

// recordAcceptedTransition emits the event for a change of an endpoint's
// Accepted condition. PartiallyAccepted is True but loses entries, so a
// transition into it is a Warning, and the way back to Accepted is Normal.
// Every other transition follows recordConditionTransition.
func recordAcceptedTransition(recorder record.EventRecorder, ep *v1alpha1.KrakenDEndpoint, prev *metav1.Condition,
	next metav1.Condition,
) {
	wasPartial := prev != nil && prev.Reason == v1alpha1.ReasonPartiallyAccepted
	switch {
	case next.Reason == v1alpha1.ReasonPartiallyAccepted && !wasPartial:
		recorder.Event(ep, corev1.EventTypeWarning, next.Reason, next.Message)
	case next.Reason == v1alpha1.ReasonAccepted && wasPartial:
		recorder.Event(ep, corev1.EventTypeNormal, next.Reason, next.Message)
	default:
		recordConditionTransition(recorder, ep, prev, next)
	}
}

// infraInputs is what the infrastructure stage deploys. It names the applied
// config and never carries config content, which only the config stage
// writes.
type infraInputs struct {
	// appliedChecksum is status.configChecksum after the config stage ran;
	// "" means no config has passed validation yet.
	appliedChecksum string
	pluginChecksum  string
	// licenseChecksum identifies the license bytes the pods mount, fallback
	// or not; "" when no license is mounted.
	licenseChecksum string
	image           string
	// ceRender: the applied config is a CE render (CE edition or CE fallback).
	ceRender bool
	// configMapName is the ConfigMap holding the applied config; "" means
	// none does.
	configMapName string
	// heldBecause is why configMapName is "": nil when none exists.
	heldBecause error
	// missingPlugins are the plugin ConfigMaps that do not exist; while any
	// is missing the Deployment is held.
	missingPlugins []string
}

// reconcileDeploymentUnlessHeld reconciles the Deployment, or leaves it exactly
// as it is while a hold applies: no config has passed validation yet, no
// ConfigMap holds the applied config, or a plugin ConfigMap is missing. It
// returns the ConfigMap collection error separately, because collection is
// housekeeping that must not hold back the rest of the stage.
func (r *KrakenDGatewayReconciler) reconcileDeploymentUnlessHeld(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	in infraInputs,
) (obs deploymentObservation, gcErr, err error) {
	switch {
	case in.appliedChecksum == "":
		// Nothing has passed validation yet: a Deployment would have
		// nothing valid to mount.
	case in.configMapName == "":
		// The applied config's ConfigMap is gone (deleted out of band while
		// a newer render is rejected). Leave the Deployment exactly as it
		// is rather than point it at a config that does not exist.
		reason := in.heldBecause
		if reason == nil {
			reason = errAppliedConfigMissing
		}
		logf.FromContext(ctx).Error(reason, "holding the Deployment as it is", "checksum", in.appliedChecksum)
	case len(in.missingPlugins) > 0:
		// A pod template that mounts a missing ConfigMap never starts
		// (FailedMount). Leave the Deployment as it is; PluginsResolved
		// names the ConfigMaps, and their creation reconciles the gateway.
	default:
		obs, err = r.reconcileDeployment(ctx, gw, in)
		if err != nil {
			return deploymentObservation{failed: true}, nil, err
		}
		gcErr = r.collectConfigMaps(ctx, gw, in.configMapName)
	}
	return obs, gcErr, nil
}

// reconcileInfrastructure is the infrastructure stage. It creates or updates
// the Kubernetes resources owned by the gateway, except the gateway ConfigMap,
// using the create-or-update pattern, deploying the applied config. The
// ConfigMap holds the config itself, which only the config stage writes.
//
// Every child is attempted, and the errors are joined: a child that keeps
// failing must not starve the ones after it. Three steps wait for the
// Deployment step instead, because they consume it:
//   - ConfigMap collection, after a successful Deployment reconcile;
//   - the deletion of an HPA the gateway no longer wants, so the Deployment
//     carries the replica count before the HPA stops managing it;
//   - the post-restart Job, which runs against the Deployment's pods.
//
// The Deployment and the post-restart Job run as the ServiceAccount named like
// the gateway, so while the gateway does not control it both are held as they
// are, and the pass returns an error and the zero observation. The caller runs
// reconcileCoreResources first and passes its outcome in, so the rollout note
// can honour the hold.
func (r *KrakenDGatewayReconciler) reconcileInfrastructure(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	in infraInputs,
	saControlled bool, coreErr error,
) (deploymentObservation, error) {
	errs := []error{coreErr}
	if !saControlled {
		// The Deployment and the post-restart Job run as the ServiceAccount
		// named like the gateway. While the gateway does not control it
		// (another controller owns it, or it could not be reconciled), both are
		// held as they are.
		errs = append(errs, fmt.Errorf("holding the Deployment and the post-restart Job: "+
			"serviceaccount %s/%s is not controlled by gateway %s", gw.Namespace, gw.Name, gw.Name),
			r.reconcileDragonfly(ctx, gw), r.reconcileExternalSecret(ctx, gw), r.reconcileVirtualService(ctx, gw))
		return deploymentObservation{}, stderrors.Join(errs...)
	}

	obs, gcErr, deploymentErr := r.reconcileDeploymentUnlessHeld(ctx, gw, in)
	errs = append(errs, deploymentErr, gcErr, r.reconcileHPA(ctx, gw, deploymentErr == nil))
	if deploymentErr == nil {
		// Only after the Deployment has rolled out the applied config, image
		// and plugins. Jobs are idempotent by name so each unique config
		// revision produces exactly one Job.
		errs = append(errs, r.reconcilePostRestartJob(ctx, gw, in))
	}
	errs = append(errs,
		r.reconcileDragonfly(ctx, gw),
		r.reconcileExternalSecret(ctx, gw),
		r.reconcileVirtualService(ctx, gw))
	return obs, stderrors.Join(errs...)
}

// reconcileCoreResources creates or updates the gateway's ServiceAccount,
// Service and PodDisruptionBudget. They are independent of each other, so each
// is attempted and the errors joined. saControlled reports whether the
// ServiceAccount step succeeded and gw controls the object CreateOrUpdate left
// behind (the server's copy, or the one it created), decided without a cached
// read: a ServiceAccount another controller owns keeps that controller's
// reference, and a failed write holds the pass.
func (r *KrakenDGatewayReconciler) reconcileCoreResources(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, in infraInputs,
) (saControlled bool, err error) {
	named := metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace}
	sa := &corev1.ServiceAccount{ObjectMeta: named}
	svc := &corev1.Service{ObjectMeta: named}
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: named}
	saErr := r.applyOwned(ctx, gw, sa, "serviceaccount", func() { resources.BuildServiceAccount(sa, gw) })
	err = stderrors.Join(saErr,
		r.applyOwned(ctx, gw, svc, "service", func() { resources.BuildService(svc, gw, in.ceRender) }),
		r.applyOwned(ctx, gw, pdb, "pdb", func() { resources.BuildPDB(pdb, gw) }))
	// The mutate function stamps the gateway's reference on sa before the
	// write, so sa alone does not prove the server accepted it.
	return saErr == nil && metav1.IsControlledBy(sa, gw), err
}

// reconcileHPA creates or updates the HorizontalPodAutoscaler when
// autoscaling is configured. Otherwise one the gateway controls is deleted, but
// only once the Deployment reconciled: it is the Deployment that carries the
// replica count the HPA stops managing.
func (r *KrakenDGatewayReconciler) reconcileHPA(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, deploymentReconciled bool,
) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace}}
	switch {
	case gw.Spec.Autoscaling != nil:
		return r.applyOwned(ctx, gw, hpa, "hpa", func() { resources.BuildHPA(hpa, gw) })
	case deploymentReconciled:
		return r.deleteIfControlled(ctx, r.Client, gw, hpa)
	}
	return nil
}

// reconcileDeployment converges the gateway Deployment on the applied config
// and records what it now runs.
func (r *KrakenDGatewayReconciler) reconcileDeployment(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, in infraInputs,
) (deploymentObservation, error) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace}}
	var before *corev1.PodTemplateSpec
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		before = dep.Spec.Template.DeepCopy()
		resources.BuildDeployment(dep, gw, resources.DeploymentInputs{
			ConfigMapName:   in.configMapName,
			ConfigChecksum:  in.appliedChecksum,
			PluginChecksum:  in.pluginChecksum,
			Image:           in.image,
			LicenseChecksum: in.licenseChecksum,
			CERender:        in.ceRender,
		})
		return controllerutil.SetControllerReference(gw, dep, r.Scheme)
	})
	if err != nil {
		return deploymentObservation{}, fmt.Errorf("reconciling deployment: %w", err)
	}
	gw.Status.ActiveImage = in.image
	gw.Status.PluginChecksum = in.pluginChecksum
	// The server's response is compared, not the template as built:
	// BuildDeployment leaves out the fields the API server defaults, so the
	// built template differs from the stored one on every pass.
	changed := result == controllerutil.OperationResultUpdated &&
		!equality.Semantic.DeepEqual(before, &dep.Spec.Template)
	if changed {
		r.metrics().RollingRestart(ctx)
	}
	return deploymentObservation{
		dep: dep, created: result == controllerutil.OperationResultCreated, templateChanged: changed,
	}, nil
}

// reconcilePostRestartJob creates a Job to run the user-provided bash script
// after the gateway rolls out the current config revision. The Job is named
// with a short prefix of the combined checksum (see
// resources.PostRestartJobChecksum) so each (config, postRestartJob spec)
// revision pair produces at most one Job under that name, ever — including
// a spec-only edit, not just a krakend.json change (nhig root cause 2). The
// Job is only created, or re-created, after the Deployment has converged on
// the applied config, image and plugins (see deploymentConverged).
//
// gw.Status.LastPostRestartJobChecksum is checked before touching the API
// server: it guards against TTLSecondsAfterFinished's cleanup GC'ing a
// finished Job and the next reconcile silently recreating (and
// re-executing) it purely because the object disappeared, not because the
// config or spec changed (nhig root cause 1 — the field was previously
// written but never read). When the checksum matches, the actual decision
// (skip vs. re-create) is delegated to reconcileExistingPostRestartRevision,
// which distinguishes "ran and succeeded" from "ran and failed" (review id
// 3805157426, #2) — see its doc comment for the loop-safety and
// no-over-trigger properties that distinction preserves.
func (r *KrakenDGatewayReconciler) reconcilePostRestartJob(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	in infraInputs,
) error {
	configChecksum := in.appliedChecksum
	spec := gw.Spec.PostRestartJob
	if spec == nil || !spec.Enabled || spec.Script == "" {
		// review id 3807285652 (#7): postRestartJob is off (unset, disabled,
		// or misconfigured with an empty script — the webhook rejects the
		// empty-script case at admission, but this is defense-in-depth for
		// webhook-bypass paths, see #9). Clear any conditions a PRIOR
		// reconcile set while it WAS enabled — otherwise `kubectl describe
		// krakendgateway` keeps showing a stale PostRestartJobSkipped /
		// PostRestartJobReadOnlyRootFilesystem condition forever after the
		// user disables the feature, contradicting
		// setPostRestartJobSkippedCondition's doc (see its comment for the
		// scope of "every branch").
		meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionPostRestartJobSkipped)
		meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionPostRestartJobReadOnlyRootFilesystem)
		return nil
	}
	if configChecksum == "" {
		// Not yet converged: no config has been rendered for this gateway
		// yet. Deliberately does NOT clear conditions (unlike the
		// disabled/empty guard above) — this is a transient "nothing to
		// decide yet" state during a normal reconcile sequence, not a
		// deliberate disable; clearing here would flicker any existing
		// conditions away and back on every reconcile while, e.g., a
		// rollout is merely in progress.
		return nil
	}

	log := logf.FromContext(ctx)

	jobChecksum, err := resources.PostRestartJobChecksum(spec, gw, configChecksum)
	if err != nil {
		return fmt.Errorf("computing post-restart job checksum: %w", err)
	}
	jobName := resources.PostRestartJobName(gw, jobChecksum)

	if gw.Status.LastPostRestartJobChecksum == jobChecksum {
		return r.reconcileExistingPostRestartRevision(ctx, gw, spec, jobName, jobChecksum, in)
	}

	// review id 3807285652 (#7): the Deployment-not-found / not-yet-converged
	// early returns below (see postRestartRolloutDone) deliberately do NOT
	// touch PostRestartJobSkipped/ROFS conditions, unlike the
	// disabled/empty guard above. These describe an in-progress rollout, not
	// a completed decision about this revision — clearing conditions here
	// would make them flicker away and back every reconcile while a rollout
	// is merely underway.
	rolledOut, err := r.postRestartRolloutDone(ctx, gw, in)
	if err != nil {
		return err
	}
	if !rolledOut {
		return nil
	}

	existing := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: gw.Namespace}, existing)
	if err == nil {
		// Job already exists under this (new, not-yet-recorded) revision's
		// name — ensure the status records the checksum even if a previous
		// status update was lost (e.g. conflict). The checksum restore
		// itself is correct in every sub-case below (this Job's checksum
		// DOES match the current revision) — only the reported reason/
		// message need to distinguish "adopted, this reconcile did not
		// create anything" from an actual Create (review id 3811443603,
		// #7; also fixes the pre-existing quirk where every adoption, not
		// just the interrupted-recreate one, was reported under
		// ReasonPostRestartJobCreated).
		gw.Status.LastPostRestartJobChecksum = jobChecksum
		reason := v1alpha1.ReasonPostRestartJobAdopted
		message := fmt.Sprintf("post-restart Job %s already exists for checksum %s", jobName, jobChecksum)
		if postRestartJobFailed(existing) {
			// This branch is reached when the checksum wasn't already
			// recorded as this revision's (e.g. the recreate path of
			// reconcileExistingPostRestartRevision cleared it before a
			// Delete that then failed transiently — review id 3807285616,
			// #1b — leaving the OLD failed Job in place under this name; or
			// a prior status write was lost before it could record a Job
			// that had already run and failed). Either way, say so plainly
			// instead of implying a healthy "created"/"exists" outcome for
			// a Job that has not successfully completed — restoring the
			// checksum here (above) means the next reconcile re-enters
			// reconcileExistingPostRestartRevision, where the actual
			// re-create/retry decision is (re-)evaluated.
			message = fmt.Sprintf(
				"post-restart Job %s already exists for checksum %s and is FAILED — adopting it "+
					"as recorded for this revision; it will be evaluated for re-create on the next "+
					"reconcile",
				jobName, jobChecksum,
			)
		}
		r.setPostRestartJobSkippedCondition(gw, metav1.ConditionFalse, reason, message)
		// review id 3807285633 (#3b): backfill the ROFS posture condition
		// on this skip/exists path too — not just on create/re-create —
		// so a gateway whose Job already exists from a prior reconcile
		// (e.g. after a controller restart, before status caught up)
		// still gets the posture signal.
		r.recordPostRestartJobROFSCondition(gw, existing)
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking existing post-restart job: %w", err)
	}

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      jobName,
		Namespace: gw.Namespace,
	}}
	resources.BuildPostRestartJob(job, gw, configChecksum, jobChecksum)
	if err := controllerutil.SetControllerReference(gw, job, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on post-restart job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating post-restart job: %w", err)
	}

	gw.Status.LastPostRestartJobChecksum = jobChecksum
	r.setPostRestartJobSkippedCondition(gw, metav1.ConditionFalse, v1alpha1.ReasonPostRestartJobCreated,
		fmt.Sprintf("post-restart Job %s created for config checksum %s", jobName, configChecksum))
	r.recordPostRestartJobROFSCondition(gw, job)
	r.Recorder.Event(gw, "Normal", v1alpha1.ReasonPostRestartJobCreated,
		fmt.Sprintf("Created post-restart Job %s for config checksum %s", jobName, configChecksum))
	log.Info("created post-restart job", "name", jobName, "checksum", jobChecksum)
	return nil
}

// reconcileExistingPostRestartRevision handles the case where
// gw.Status.LastPostRestartJobChecksum already matches the current
// (config, postRestartJob-spec) revision's checksum — i.e. a Job for this
// exact revision was created at some point. Review id 3805157426 (#2):
// previously this state always meant "skip, unconditionally" (the guard
// could not distinguish "ran and succeeded" from "ran and failed"). This
// now branches on the Job's observable outcome:
//
//  1. Job object gone (TTL-GC'd or `kubectl delete job`'d): outcome is
//     unobservable, so always skip — never guess. This preserves
//     `kubectl delete job` as a no-op and is itself loop-safe (skip is a
//     no-op, not a retry).
//  2. Job succeeded: skip, unconditionally — including when an operator
//     knob (activeDeadlineSeconds/backoffLimit/tmpSizeLimit) was edited
//     afterward. This is the no-over-trigger property: a knob edit on a
//     healthy Job must never re-run it, which is exactly the over-trigger
//     the projection-hash design (superseding a naive whole-spec hash) was
//     built to avoid. See TestReconcileExistingPostRestartRevision_
//     SucceededKnobChangeSkips.
//  3. Job still running (neither Complete nor Failed): skip — nothing to
//     re-create.
//  4. Job failed: only re-create if the USER's spec knobs
//     (activeDeadlineSeconds/backoffLimit/tmpSizeLimit — see
//     postRestartJobProjection, which deliberately excludes them from the
//     checksum/name so purely-cosmetic edits don't re-trigger) that are
//     explicitly SET differ from the existing failed Job's (review id
//     3807285637, #4: comparing the freshly-BUILT desired Job — which
//     always carries the operator's CURRENT defaults for any knob the user
//     left unset — against the existing Job would make a future default
//     bump alone (no user spec change at all) look like a knob edit and
//     mass-recreate every FAILED Job fleet-wide at rollout; comparing the
//     user's spec pointers directly means an unset knob never triggers a
//     recreate regardless of what the operator's default happens to be).
//     This is the loop-safety property: a persistently-failing Job whose
//     spec is UNCHANGED must NOT be re-created every reconcile — only an
//     actual operator knob edit does. See
//     TestReconcileExistingPostRestartRevision_FailedSpecUnchangedSkips,
//     TestReconcileExistingPostRestartRevision_FailedSpecChangedRecreates,
//     and TestPostRestartJobExecutionKnobsChanged_DefaultOnlyDiffDoesNotTrigger.
//     Since the checksum (and therefore the Job name) is unchanged by
//     definition in this branch, re-creating means delete-then-create under
//     the same name, not a new name — see review id 3807285616 (#1) on
//     this function's handling of that Delete-then-Create sequence not
//     being atomic.
//     A re-create runs the script again, so it first waits for the rollout
//     to finish, as a first run does; until then nothing is touched.
//
// Every branch above except a re-create still waiting for the rollout also
// backfills the ROFS posture condition (review id 3807285633, #3b) — not
// just create/re-create — so an already-run gateway that never hits this
// function's create path again still carries the posture signal.
func (r *KrakenDGatewayReconciler) reconcileExistingPostRestartRevision(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	spec *v1alpha1.PostRestartJobSpec,
	jobName string,
	jobChecksum string,
	in infraInputs,
) error {
	configChecksum := in.appliedChecksum
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: gw.Namespace}, existing)
	if errors.IsNotFound(err) {
		r.setPostRestartJobSkippedCondition(gw, metav1.ConditionTrue, v1alpha1.ReasonPostRestartJobAlreadyRun,
			fmt.Sprintf(
				"post-restart Job already triggered for checksum %s (Job object no longer observable — "+
					"TTL-GC'd or deleted, outcome unknown); skipping (once per revision — see "+
					"status.lastPostRestartJobChecksum to force a re-run)",
				jobChecksum,
			))
		// review id 3807285633 (#3b): the Job object is gone, but the
		// steady-state posture signal is still worth reporting — build an
		// ephemeral (never Created) Job purely to derive what the ROFS
		// posture WAS for this revision, from the current spec. This is
		// the "already-run gateway" case the create/re-create-only
		// backfill missed entirely.
		synthetic := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
		resources.BuildPostRestartJob(synthetic, gw, configChecksum, jobChecksum)
		r.recordPostRestartJobROFSCondition(gw, synthetic)
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting existing post-restart job %s: %w", jobName, err)
	}

	if postRestartJobSucceeded(existing) {
		r.setPostRestartJobSkippedCondition(gw, metav1.ConditionTrue, v1alpha1.ReasonPostRestartJobAlreadyRun,
			fmt.Sprintf(
				"post-restart Job already triggered for checksum %s and succeeded; skipping "+
					"(once per revision — see status.lastPostRestartJobChecksum to force a re-run)",
				jobChecksum,
			))
		r.recordPostRestartJobROFSCondition(gw, existing)
		return nil
	}

	if !postRestartJobFailed(existing) {
		r.setPostRestartJobSkippedCondition(gw, metav1.ConditionTrue, v1alpha1.ReasonPostRestartJobAlreadyRun,
			fmt.Sprintf(
				"post-restart Job already triggered for checksum %s and is still running; skipping",
				jobChecksum,
			))
		r.recordPostRestartJobROFSCondition(gw, existing)
		return nil
	}

	// Failed. Compare the USER's spec knobs (nil-vs-set) against the
	// existing failed Job — see postRestartJobExecutionKnobsChanged (review
	// id 3807285637, #4) — and build the desired Job fresh from the current
	// spec for the actual re-create below. script, image, etc. changes
	// already produce a new checksum/name and are handled by the caller's
	// create path, not here.
	if !postRestartJobExecutionKnobsChanged(spec, existing) {
		// Wording note (review id 3811443580, #5): this branch is also
		// reached when the user REMOVES a previously-set knob override
		// (spec.BackoffLimit etc. goes from set to nil) rather than only
		// when the knobs are genuinely identical — postRestartJobExecutionKnobsChanged
		// is guarded by "spec.X != nil", so a removed override is invisible
		// to it (see that function's doc, review id 3807285637 #4, for why
		// this is the accepted trade-off, not a bug). The message must not
		// claim "unchanged" in that case, since the user did change the
		// spec; it just wasn't detected as a retry-triggering edit.
		r.setPostRestartJobSkippedCondition(gw, metav1.ConditionTrue, v1alpha1.ReasonPostRestartJobAlreadyRun,
			fmt.Sprintf(
				"post-restart Job already triggered for checksum %s and failed; no explicitly-set "+
					"activeDeadlineSeconds/backoffLimit/tmpSizeLimit differs from the existing Job, NOT "+
					"re-creating (would retry every reconcile) — set one of those knobs to a new explicit "+
					"value to retry (removing an override is not detected as a change), or edit the "+
					"script/image/etc. (changes the revision checksum)",
				jobChecksum,
			))
		r.recordPostRestartJobROFSCondition(gw, existing)
		return nil
	}

	// A re-create runs the script again, so it waits for the rollout like a
	// first run does. Like the not-yet-converged returns in
	// reconcilePostRestartJob, this leaves the conditions untouched.
	rolledOut, err := r.postRestartRolloutDone(ctx, gw, in)
	if err != nil {
		return err
	}
	if !rolledOut {
		return nil
	}

	desired := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
	resources.BuildPostRestartJob(desired, gw, configChecksum, jobChecksum)

	// review id 3807285616 (#1): a Delete-then-Create recreate is not
	// atomic — a Create failure (transient API error) or a process crash
	// between the two calls can strand this revision forever: the Job
	// would be gone, but gw.Status.LastPostRestartJobChecksum (persisted in
	// etcd) still matches jobChecksum, so every future reconcile re-enters
	// this function, Gets the (now-deleted) Job, hits NotFound, and treats
	// the outcome as permanently unobservable — skipping forever (the
	// TTL-GC branch above) — even though nothing ever actually ran for
	// this revision.
	//
	// Fix: synchronously clear and flush the checksum via Status().Update
	// BEFORE the Delete (crash variant, #1b — protects against a crash
	// between Delete and Create: the persisted status no longer claims
	// this revision already ran). If Create then fails for a transient
	// reason after a successful Delete (#1a), the checksum is already
	// cleared and flushed, so no further write is needed before returning
	// the error — the next reconcile falls through to
	// reconcilePostRestartJob's top-level create path (Get existing Job by
	// name -> NotFound -> fresh Create) instead of this function's
	// now-permanently-skip branch. On success, the checksum is restored
	// in-memory below. The final status write at the end of Reconcile
	// (updateStatusIfChanged) persists it, because the condition change
	// makes the status differ from the start-of-reconcile snapshot.
	gw.Status.LastPostRestartJobChecksum = ""
	if err := r.Status().Update(ctx, gw); err != nil {
		return fmt.Errorf("clearing post-restart job checksum before re-create: %w", err)
	}

	if delErr := r.Delete(ctx, existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil &&
		!errors.IsNotFound(delErr) {
		return fmt.Errorf("deleting failed post-restart job %s for re-create: %w", jobName, delErr)
	}
	if err := controllerutil.SetControllerReference(gw, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on re-created post-restart job: %w", err)
	}
	if err := r.Create(ctx, desired); err != nil {
		if errors.IsAlreadyExists(err) {
			// review id 3807285616 (#1c): tolerate AlreadyExists —
			// background-delete propagation race. DeletePropagationBackground
			// returns as soon as the delete is accepted, not once the object
			// is actually gone; the just-deleted Job's removal may still be
			// in flight when this Create lands under the same name.
			// Symmetric with the create-path handling above (~L941-945):
			// don't error, don't set status here — the checksum was already
			// cleared above, so a subsequent reconcile's "Job already
			// exists" branch (or this branch again) observes the object and
			// reconciles status/conditions/checksum then.
			return nil
		}
		// #1a: Create failed for a reason other than AlreadyExists, after a
		// successful Delete. The checksum was already cleared and flushed
		// above, so the next reconcile retries the create path from
		// scratch instead of treating this revision as
		// already-run-but-now-unobservable.
		return fmt.Errorf("re-creating failed post-restart job %s: %w", jobName, err)
	}

	gw.Status.LastPostRestartJobChecksum = jobChecksum
	r.setPostRestartJobSkippedCondition(gw, metav1.ConditionFalse, v1alpha1.ReasonPostRestartJobCreated,
		fmt.Sprintf(
			"post-restart Job %s re-created after a failed run and an operator knob change "+
				"(activeDeadlineSeconds/backoffLimit/tmpSizeLimit)", jobName,
		))
	r.recordPostRestartJobROFSCondition(gw, desired)
	r.Recorder.Event(gw, "Normal", v1alpha1.ReasonPostRestartJobCreated,
		fmt.Sprintf("Re-created post-restart Job %s after a failed run and an operator knob change", jobName))
	logf.FromContext(ctx).Info("re-created failed post-restart job after knob change",
		"name", jobName, "checksum", jobChecksum)
	return nil
}

// postRestartRolloutDone reports whether the gateway Deployment has finished
// rolling out in, so that a post-restart Job runs against the pods that carry
// it. A missing Deployment, or one scaled to zero, has no pods to run
// against.
func (r *KrakenDGatewayReconciler) postRestartRolloutDone(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, in infraInputs,
) (bool, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting deployment for post-restart check: %w", err)
	}
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
		// Intentionally scaled to zero: no pods have rolled, so a
		// post-restart Job must not run.
		return false, nil
	}
	return deploymentConverged(&dep, in), nil
}

// setPostRestartJobSkippedCondition records the last skip/create decision
// for the post-restart Job guard. Review id 3805157450 (#4): this is now
// set on every branch of the guard's DECISION logic (skip AND
// create/re-create), not only when skipping — previously it was only ever
// set to True, so `kubectl describe` could show a stale True condition from
// a prior revision even after a new Job was successfully created for the
// current one.
//
// Correction (review id 3807285652, #7): "every branch" means every branch
// reachable once postRestartJob is enabled/configured with script and
// config checksum present AND the Deployment has converged on the applied
// config, image and plugins — i.e. every branch of
// reconcileExistingPostRestartRevision that reaches a decision, plus the
// create/already-exists branches of reconcilePostRestartJob. It does NOT
// cover the disabled/unconfigured guard or the not-yet-converged early
// returns in reconcilePostRestartJob, which precede any revision-specific
// decision existing at all, nor a failed Job's re-create while the rollout
// is still running (reconcileExistingPostRestartRevision returns before it
// decides). The disabled/unconfigured guard instead actively REMOVES this
// condition (and the ROFS one); the not-yet-converged returns intentionally
// leave prior conditions untouched (see the comments at each of those call
// sites).
func (r *KrakenDGatewayReconciler) setPostRestartJobSkippedCondition(
	gw *v1alpha1.KrakenDGateway, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPostRestartJobSkipped,
		Status:             status,
		ObservedGeneration: gw.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// recordPostRestartJobROFSCondition sets an informational status Condition
// reporting the post-restart Job container's effective
// readOnlyRootFilesystem posture. Originally set only on Job create/
// re-create (review id 3805157497, #9); review id 3807285633 (#3b) backfills
// it onto every skip/exists branch too (see reconcileExistingPostRestartRevision
// and the "Job already exists" branch of reconcilePostRestartJob) so a
// steady-state gateway whose Job already ran — and therefore never touches
// the create/re-create branches again — still carries the posture signal,
// not only gateways that happen to be creating a Job on this reconcile.
//
// Review id 3807285633 (#3d): effectiveROFS is derived from the BUILT Job's
// container SecurityContext (job.Spec.Template...), not the raw user spec
// — job already reflects mergeContainerSecurityContext's hardened-default
// merge (internal/resources/job.go), so reading it here can never drift
// from what was actually applied to the running container, even if a
// future change to the merge logic changes what "effective" means for some
// field combination.
//
// The admission-time warning in internal/webhook/webhook.go only fires in
// the narrower case of workingDir overridden outside /tmp. This condition
// covers the common prod case too — an unset (default) workingDir with a
// script that still writes outside /tmp (e.g. `npm install -g`) — without
// analyzing the script, and — unlike admission.Warnings, which GitOps
// appliers (ArgoCD, Flux, ...) commonly swallow — is visible via `kubectl
// describe krakendgateway` and the object's Events.
func (r *KrakenDGatewayReconciler) recordPostRestartJobROFSCondition(
	gw *v1alpha1.KrakenDGateway, job *batchv1.Job,
) {
	effectiveROFS := true
	if sc := postRestartJobContainerSecurityContext(job); sc != nil && sc.ReadOnlyRootFilesystem != nil {
		effectiveROFS = *sc.ReadOnlyRootFilesystem
	}

	if !effectiveROFS {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionPostRestartJobReadOnlyRootFilesystem,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonPostRestartJobROFSDisabled,
			Message: "post-restart Job container runs with readOnlyRootFilesystem=false " +
				"(explicit override); the entire container filesystem is writable.",
		})
		return
	}

	// review id 3807285633 (#3a): a tmpSizeLimit of "0" is the emptyDir
	// convention for "unbounded" (no cap enforced by the kubelet), not a
	// literal zero-byte limit — route it to the same "unbounded" message
	// as an entirely-unset limit instead of misleadingly rendering "0".
	sizeLimit := "unbounded"
	if limit := postRestartJobTmpSizeLimit(job); limit != nil && !limit.IsZero() {
		sizeLimit = limit.String()
	}
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPostRestartJobReadOnlyRootFilesystem,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonPostRestartJobROFSEnabled,
		Message: fmt.Sprintf(
			"post-restart Job runs with readOnlyRootFilesystem=true; only %s (emptyDir, %s) is writable",
			resources.PostRestartTmpMountPath, sizeLimit,
		),
	})
}

// recordDragonflyRunAsRootCondition sets an informational status Condition
// reporting whether the BUILT Dragonfly CR's rendered securityContext maps
// carry an unacknowledged runAsUser: 0 request. df is
// the object AFTER resources.BuildDragonfly has already mutated it in the
// CreateOrUpdate mutate callback, so this reads the actual rendered maps
// (post-merge-fixup), not the raw v1alpha1.DragonflySpec — mirroring
// recordPostRestartJobROFSCondition's "read the built object" approach
// (review id 3807285633, #3d) so this can never drift from what
// BuildDragonfly actually produced.
func (r *KrakenDGatewayReconciler) recordDragonflyRunAsRootCondition(
	gw *v1alpha1.KrakenDGateway, df *unstructured.Unstructured,
) {
	// NestedMap's returned bool/error are both ignorable here: a missing or
	// malformed field simply yields a nil map, and
	// resources.DragonflyRunAsRootUnacknowledged treats a nil map the same
	// as an absent field (no runAsUser/runAsNonRoot key), which is the
	// correct "not root" behavior for a spec.podSecurityContext/
	// containerSecurityContext that BuildDragonfly always populates anyway.
	containerMap, _, err := unstructured.NestedMap(df.Object, "spec", "containerSecurityContext")
	if err != nil {
		containerMap = nil
	}
	podMap, _, err := unstructured.NestedMap(df.Object, "spec", "podSecurityContext")
	if err != nil {
		podMap = nil
	}

	if resources.DragonflyRunAsRootUnacknowledged(containerMap, podMap) {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionDragonflyRunAsRootUnacknowledged,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonDragonflyRunAsRootUnacknowledged,
			Message: "the rendered Dragonfly securityContext carries an unacknowledged " +
				"runAsUser: 0 request; set runAsNonRoot: false at the scope that requested " +
				"root to acknowledge it (see docs/upgrade-guide.md item 7).",
		})
		return
	}

	// The False state has two distinct reasons, so a viewer can tell "someone
	// requested root and explicitly acknowledged it" apart from "this gateway
	// never requested root".
	if resources.DragonflyRunAsRootRequested(containerMap, podMap) {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionDragonflyRunAsRootUnacknowledged,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonDragonflyRunAsRootAcknowledged,
			Message:            "the rendered Dragonfly securityContext carries a runAsUser: 0 request that is explicitly acknowledged.",
		})
		return
	}

	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDragonflyRunAsRootUnacknowledged,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonDragonflyRunAsRootNoRequest,
		Message:            "the rendered Dragonfly securityContext does not request runAsUser: 0.",
	})
}

// postRestartJobContainerSecurityContext locates the post-restart
// container's effective (post-merge) SecurityContext on a built Job, or nil
// if the container is absent. Review id 3807285633 (#3d): reading this off
// the BUILT Job — rather than the raw user spec.SecurityContext — means the
// ROFS condition can never drift from what mergeContainerSecurityContext
// (internal/resources/job.go) actually applied.
func postRestartJobContainerSecurityContext(job *batchv1.Job) *corev1.SecurityContext {
	for i := range job.Spec.Template.Spec.Containers {
		c := &job.Spec.Template.Spec.Containers[i]
		if c.Name == resources.PostRestartContainerName {
			return c.SecurityContext
		}
	}
	return nil
}

// postRestartJobSucceeded reports whether the Job's most recent run
// completed successfully.
func postRestartJobSucceeded(job *batchv1.Job) bool {
	if job.Status.Succeeded > 0 {
		return true
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// postRestartJobFailed reports whether the Job's most recent run
// exhausted its retries and failed (backoffLimit exceeded, or
// activeDeadlineSeconds exceeded).
func postRestartJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// postRestartJobExecutionKnobsChanged reports whether the USER's spec
// explicitly sets any of the execution-affecting-but-checksum-excluded
// knobs (activeDeadlineSeconds, backoffLimit, tmpSizeLimit — see
// postRestartJobProjection in internal/resources/job.go) to a value that
// differs from what's on the existing (failed) Job. These three are
// excluded from the Job identity checksum deliberately (so purely cosmetic
// edits don't re-trigger the script), but they DO affect whether the
// script can actually complete — a too-short activeDeadlineSeconds,
// too-low backoffLimit, or too-small tmpSizeLimit all produce a failure
// that looks identical to a genuine script bug. This is the only situation
// review id 3805157426 (#2) allows to re-create a FAILED Job without a
// checksum (and therefore name) change.
//
// Review id 3807285637 (#4): this compares spec (the raw
// PostRestartJobSpec, nil-vs-set) rather than a freshly-BUILT desired Job.
// A built Job always carries the operator's CURRENT default for any knob
// the user left unset (see resources.BuildPostRestartJob) — comparing that
// against an existing Job built under a POSSIBLY-OLDER default would make
// a future default bump alone (e.g. bumping defaultPostRestartBackoffLimit
// from 2 to 3), with no user spec change at all, look identical to a real
// knob edit and mass-recreate every FAILED Job fleet-wide on the next
// reconcile after upgrade. Comparing the user's spec pointers directly
// means a knob the user never touched (nil) never triggers a recreate,
// regardless of what value the operator's default happens to resolve to on
// either side. See TestPostRestartJobExecutionKnobsChanged_DefaultOnlyDiffDoesNotTrigger.
func postRestartJobExecutionKnobsChanged(spec *v1alpha1.PostRestartJobSpec, existing *batchv1.Job) bool {
	if spec.BackoffLimit != nil && !int32PtrEqual(spec.BackoffLimit, existing.Spec.BackoffLimit) {
		return true
	}
	if spec.ActiveDeadlineSeconds != nil &&
		!int64PtrEqual(spec.ActiveDeadlineSeconds, existing.Spec.ActiveDeadlineSeconds) {
		return true
	}
	if spec.TmpSizeLimit != nil {
		existingLimit := postRestartJobTmpSizeLimit(existing)
		if existingLimit == nil || spec.TmpSizeLimit.Cmp(*existingLimit) != 0 {
			return true
		}
	}
	return false
}

// postRestartJobTmpSizeLimit locates the /tmp emptyDir volume's SizeLimit on
// a post-restart Job, or nil if absent.
func postRestartJobTmpSizeLimit(job *batchv1.Job) *resource.Quantity {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == resources.PostRestartTmpVolumeName && v.EmptyDir != nil {
			return v.EmptyDir.SizeLimit
		}
	}
	return nil
}

func int32PtrEqual(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// endpointToGateway maps a KrakenDEndpoint to its owning gateway.
func (r *KrakenDGatewayReconciler) endpointToGateway(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{
			Name:      ep.Spec.GatewayRef.Name,
			Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace),
		},
	}}
}

// policyToGateways maps a KrakenDBackendPolicy to all gateways with
// endpoints that reference it. Uses the policy field index for cross-namespace
// lookup.
func (r *KrakenDGatewayReconciler) policyToGateways(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	log := logf.FromContext(ctx)
	indexKey := obj.GetNamespace() + "/" + obj.GetName()
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &endpoints,
		client.MatchingFields{fieldindex.EndpointPolicy: indexKey},
	); err != nil {
		log.Error(err, "policyToGateways: index lookup failed, gateway may not reconcile",
			"policy", obj.GetName(), "namespace", obj.GetNamespace())
		return nil
	}
	seen := map[types.NamespacedName]struct{}{}
	var requests []reconcile.Request
	for i := range endpoints.Items {
		ep := &endpoints.Items[i]
		nn := types.NamespacedName{
			Name:      ep.Spec.GatewayRef.Name,
			Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace),
		}
		if _, ok := seen[nn]; !ok {
			seen[nn] = struct{}{}
			requests = append(requests, reconcile.Request{NamespacedName: nn})
		}
	}
	return requests
}

// licenseSecretToGateway maps a Secret change to gateways that reference it.
func (r *KrakenDGatewayReconciler) licenseSecretToGateway(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	var gateways v1alpha1.KrakenDGatewayList
	if err := r.List(ctx, &gateways, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		if name, _, ok := resources.LicenseSecret(gw); ok && name == obj.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace},
			})
		}
	}
	return requests
}

// pluginConfigMapToGateway maps a ConfigMap change to gateways that reference
// it as a plugin source via spec.plugins.sources[].configMapRef.
func (r *KrakenDGatewayReconciler) pluginConfigMapToGateway(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	var gateways v1alpha1.KrakenDGatewayList
	if err := r.List(ctx, &gateways, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range gateways.Items {
		gw := &gateways.Items[i]
		if gw.Spec.Plugins == nil {
			continue
		}
		for _, src := range gw.Spec.Plugins.Sources {
			if src.ConfigMapRef != nil && src.ConfigMapRef.Name == obj.GetName() {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace},
				})
				break
			}
		}
	}
	return requests
}

// gatewayReadiness is a gateway's derived Ready condition and the
// compatibility phase that goes with it.
type gatewayReadiness struct {
	status  metav1.ConditionStatus
	reason  string
	message string
	phase   v1alpha1.GatewayPhase
}

// gatewayReadinessFor derives a gateway's Ready condition and phase from the
// conditions the gateway controller maintains. The first rule that applies
// wins: a rejected configuration, an expired license without CE fallback, a
// missing plugin ConfigMap, a failed rollout, CE fallback (the removed
// features first), no validated configuration yet, a configuration that could
// not be validated, a rollout in progress, and a Deployment not yet available.
// The gateway is Ready only when none applies.
// A configuration that could not be validated (the validator was unavailable)
// makes Ready Unknown, not False, and leaves the phase at the serving phase:
// the last applied configuration keeps serving.
func gatewayReadinessFor(conds []metav1.Condition) gatewayReadiness {
	configValid := meta.FindStatusCondition(conds, v1alpha1.ConditionConfigValid)
	available := meta.FindStatusCondition(conds, v1alpha1.ConditionAvailable)
	progressing := meta.FindStatusCondition(conds, v1alpha1.ConditionProgressing)
	degraded := meta.FindStatusCondition(conds, v1alpha1.ConditionLicenseDegraded)
	expired := meta.FindStatusCondition(conds, v1alpha1.ConditionLicenseExpired)
	ceFallback := meta.FindStatusCondition(conds, v1alpha1.ConditionCEFallbackApplied)
	plugins := meta.FindStatusCondition(conds, v1alpha1.ConditionPluginsResolved)
	switch {
	case condFalse(configValid):
		return notReady(configValid, v1alpha1.PhaseError)
	case condTrue(expired) && !condTrue(degraded):
		return gatewayReadiness{status: metav1.ConditionFalse, reason: v1alpha1.ReasonLicenseExpiredNoFallback,
			message: expired.Message, phase: v1alpha1.PhaseError}
	case condFalse(plugins):
		return notReady(plugins, v1alpha1.PhaseError)
	case condFalse(available):
		return notReady(available, v1alpha1.PhaseError)
	case condTrue(ceFallback):
		// Serving the CE-fallback render: not as specified, and the message
		// lists what was removed.
		return notReady(ceFallback, v1alpha1.PhaseDegraded)
	case condTrue(degraded):
		return notReady(degraded, v1alpha1.PhaseDegraded)
	case configValid == nil:
		return gatewayReadiness{status: metav1.ConditionUnknown, reason: v1alpha1.ReasonPending,
			message: "Waiting for the first configuration to be validated", phase: v1alpha1.PhasePending}
	case configValid.Status == metav1.ConditionUnknown:
		return gatewayReadiness{status: metav1.ConditionUnknown, reason: configValid.Reason,
			message: configValid.Message, phase: servingPhase(progressing, available)}
	case condTrue(progressing):
		return notReady(progressing, v1alpha1.PhaseDeploying)
	case !condTrue(available):
		return gatewayReadiness{status: metav1.ConditionFalse, reason: v1alpha1.ReasonAwaitingAvailability,
			message: "Waiting for the Deployment to report available replicas", phase: v1alpha1.PhaseDeploying}
	default:
		return gatewayReadiness{status: metav1.ConditionTrue, reason: v1alpha1.ReasonReady,
			message: "Configuration applied and all replicas available", phase: v1alpha1.PhaseRunning}
	}
}

// setGatewayReadiness writes the derived Ready condition, phase and
// observedGeneration into gw's in-memory status. observed is the generation
// the status claims to have applied: gw's own, or an earlier one while this
// pass left part of the spec unapplied. Ready carries it too, so the two never
// disagree. Call it immediately before every gateway status write that ends a
// reconcile.
func setGatewayReadiness(gw *v1alpha1.KrakenDGateway, observed int64) {
	rd := gatewayReadinessFor(gw.Status.Conditions)
	setReadyCondition(&gw.Status.Conditions, observed, rd.status, rd.reason, rd.message)
	gw.Status.Phase = rd.phase
	gw.Status.ObservedGeneration = observed
}

// setConfigApplied records that the rendered configuration passed
// validation and is the gateway's applied configuration.
func setConfigApplied(gw *v1alpha1.KrakenDGateway) {
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionConfigValid,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonConfigApplied,
		Message:            "Configuration passed validation and is applied",
	})
}

// notReady is a False Ready that carries cause's reason and message.
func notReady(cause *metav1.Condition, phase v1alpha1.GatewayPhase) gatewayReadiness {
	return gatewayReadiness{status: metav1.ConditionFalse, reason: cause.Reason, message: cause.Message, phase: phase}
}

// condFalse reports whether c exists and is False.
func condFalse(c *metav1.Condition) bool { return c != nil && c.Status == metav1.ConditionFalse }

// condTrue reports whether c exists and is True.
func condTrue(c *metav1.Condition) bool { return c != nil && c.Status == metav1.ConditionTrue }

// servingPhase is the phase of a gateway judged only by its rollout: Pending
// before anything was rolled out, Deploying while a rollout is in progress or
// the Deployment is not available, Running otherwise.
func servingPhase(progressing, available *metav1.Condition) v1alpha1.GatewayPhase {
	switch {
	case progressing == nil && available == nil:
		return v1alpha1.PhasePending
	case condTrue(progressing) || !condTrue(available):
		return v1alpha1.PhaseDeploying
	default:
		return v1alpha1.PhaseRunning
	}
}

// newGatewayRateLimiter is controller-runtime's default rate limiter with the
// per-item backoff capped at licenseRecheckInterval instead of 1000s, so a
// gateway whose reconcile keeps failing still has its license looked at at
// least that often.
func newGatewayRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return cappedRateLimiter(licenseRecheckInterval)
}

// cappedRateLimiter is controller-runtime's default rate limiter with the
// per-item backoff capped at maxWait.
func cappedRateLimiter(maxWait time.Duration) workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedWithMaxWaitRateLimiter(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](), maxWait)
}
