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
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	utilclock "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// KrakenDGatewayReconciler reconciles a KrakenDGateway object.
// It orchestrates the full rendering pipeline and manages all owned
// Kubernetes resources.
type KrakenDGatewayReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	Renderer  renderer.Renderer
	Validator renderer.Validator
	Clock     utilclock.Clock
}

// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=dragonflydb.io,resources=dragonflies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the gateway rendering pipeline: gather inputs,
// render config, validate, update resource, and reconcile owned objects.
func (r *KrakenDGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	start := time.Now()
	defer func() {
		reconcileDuration.WithLabelValues("gateway", req.Namespace, req.Name).
			Observe(time.Since(start).Seconds())
	}()

	var gw v1alpha1.KrakenDGateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting gateway %s: %w", req.NamespacedName, err)
	}

	// Initialize phase
	if gw.Status.Phase == "" {
		gw.Status.Phase = v1alpha1.PhasePending
		if err := r.Status().Update(ctx, &gw); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting initial phase: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Gather endpoints via field index
	var endpointList v1alpha1.KrakenDEndpointList
	indexKey := gw.Namespace + "/" + gw.Name
	if err := r.List(ctx, &endpointList,
		client.MatchingFields{EndpointGatewayIndex: indexKey},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing endpoints: %w", err)
	}
	endpoints := endpointList.Items

	// Sort endpoints to keep processing and rendered output deterministic.
	slices.SortFunc(endpoints, func(a, b v1alpha1.KrakenDEndpoint) int {
		if c := cmp.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})

	// Gather referenced policies
	policies, err := r.gatherPolicies(ctx, endpoints)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Determine CE fallback from status conditions
	ceFallback := meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded)

	// Gather plugin ConfigMaps
	pluginConfigMaps, err := r.gatherPluginConfigMaps(ctx, &gw)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Detect Dragonfly state
	dragonflyState := r.detectDragonflyState(ctx, &gw)

	// Render configuration
	output, err := r.Renderer.Render(renderer.RenderInput{
		Gateway:          &gw,
		Endpoints:        endpoints,
		Policies:         policies,
		CEFallback:       ceFallback,
		Dragonfly:        dragonflyState,
		PluginConfigMaps: pluginConfigMaps,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rendering config: %w", err)
	}
	configRenders.Inc()

	// Update endpoint statuses for conflicted/invalid
	if err := r.updateEndpointStatuses(ctx, output); err != nil {
		return ctrl.Result{}, err
	}

	// Determine if config changed
	configChanged := output.Checksum != gw.Status.ConfigChecksum
	imageChanged := output.DesiredImage != gw.Status.ActiveImage
	pluginChanged := output.PluginChecksum != "" && output.PluginChecksum != gw.Status.PluginChecksum

	if configChanged {
		// Rendering pipeline: validate and update ConfigMap
		gw.Status.Phase = v1alpha1.PhaseRendering
		if err := r.Status().Update(ctx, &gw); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting phase Rendering: %w", err)
		}

		gw.Status.Phase = v1alpha1.PhaseValidating
		if err := r.Status().Update(ctx, &gw); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting phase Validating: %w", err)
		}

		if err := r.validateConfig(ctx, &gw, output.JSON, ceFallback); err != nil {
			configValidationFailures.Inc()
			return ctrl.Result{}, r.handleValidationError(ctx, &gw, err)
		}

		// Update ConfigMap
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: gw.Name, Namespace: gw.Namespace,
		}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			resources.BuildConfigMap(cm, &gw, output.JSON)
			return controllerutil.SetControllerReference(&gw, cm, r.Scheme)
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling configmap: %w", err)
		}

		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionConfigValid,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             "ConfigValid",
			Message:            "Configuration passed validation",
		})
		gw.Status.Phase = v1alpha1.PhaseDeploying
		gw.Status.ConfigChecksum = output.Checksum
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionProgressing,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonConfigDeployed,
			Message:            "Configuration updated, rolling deployment",
		})

		r.Recorder.Event(&gw, "Normal", v1alpha1.ReasonConfigDeployed,
			fmt.Sprintf("Configuration updated, checksum: %s", output.Checksum))
		rollingRestarts.Inc()
	} else if imageChanged || pluginChanged {
		gw.Status.Phase = v1alpha1.PhaseDeploying
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionProgressing,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             "DeploymentUpdated",
			Message:            "Deployment updated for image or plugin change",
		})
		rollingRestarts.Inc()
	}

	// Reconcile owned resources
	if err := r.reconcileOwnedResources(ctx, &gw, output); err != nil {
		return ctrl.Result{}, err
	}

	// Inspect Deployment rollout status
	r.inspectDeploymentStatus(ctx, &gw)

	// Update final status
	gw.Status.ActiveImage = output.DesiredImage
	gw.Status.PluginChecksum = output.PluginChecksum
	gw.Status.ObservedGeneration = gw.Generation
	gw.Status.EndpointCount = int32(len(endpoints))
	endpointsPerGateway.WithLabelValues(gw.Namespace, gw.Name).Set(float64(len(endpoints)))
	gatewayInfo.WithLabelValues(gw.Namespace, gw.Name, string(gw.Spec.Edition), gw.Spec.Version).Set(1)
	if gw.Status.Phase != v1alpha1.PhaseDegraded && gw.Status.Phase != v1alpha1.PhaseError {
		if ceFallback {
			gw.Status.Phase = v1alpha1.PhaseDegraded
		} else if gw.Status.Phase != v1alpha1.PhaseDeploying {
			gw.Status.Phase = v1alpha1.PhaseRunning
		}
	}

	if err := r.Status().Update(ctx, &gw); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating gateway status: %w", err)
	}

	log.V(1).Info("gateway reconciled",
		"phase", gw.Status.Phase,
		"checksum", gw.Status.ConfigChecksum,
		"endpoints", gw.Status.EndpointCount)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
// Optional third-party CRDs (Dragonfly, ExternalSecret, VirtualService) are
// NOT registered with Owns() because they may not be installed in the cluster.
// The operator still sets ownerReferences on instances it creates so that GC
// cleans them up when the gateway is deleted.
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
// GenerationChangedPredicate to the primary For() watch too (e.g. to cut
// reconcile volume from the operator's own frequent Status().Update()
// calls), a status-only escape-hatch patch would stop triggering an
// immediate reconcile and instead silently degrade to "whenever the next
// unrelated event happens to fire" — the escape hatch would still
// eventually work, just not on-demand. Keep this in mind before adding a
// predicate to the primary watch.
func (r *KrakenDGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := EnsureEndpointIndexes(mgr); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KrakenDGateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
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
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.pluginConfigMapToGateway),
		).
		Named("krakendgateway").
		Complete(r)
}

// crdAvailable checks whether the given GVK is registered in the cluster's
// API discovery. Returns (false, nil) when the CRD is simply not installed,
// and (false, err) for transient or unexpected errors.
func (r *KrakenDGatewayReconciler) crdAvailable(gvk schema.GroupVersionKind) (bool, error) {
	_, err := r.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return true, nil
	}
	if meta.IsNoMatchError(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking CRD availability for %s: %w", gvk, err)
}

// gatherPolicies fetches all unique KrakenDBackendPolicy resources referenced
// by the given endpoints. Each policy is looked up in the namespace resolved
// from the PolicyRef (explicit namespace or endpoint namespace as fallback).
// The returned map is keyed by "namespace/name" for full disambiguation.
func (r *KrakenDGatewayReconciler) gatherPolicies(
	ctx context.Context,
	endpoints []v1alpha1.KrakenDEndpoint,
) (map[string]*v1alpha1.KrakenDBackendPolicy, error) {
	policies := make(map[string]*v1alpha1.KrakenDBackendPolicy)
	for _, ep := range endpoints {
		for _, entry := range ep.Spec.Endpoints {
			for _, be := range entry.Backends {
				if be.PolicyRef == nil {
					continue
				}
				mapKey := be.PolicyRef.PolicyKey(ep.Namespace)
				if _, ok := policies[mapKey]; ok {
					continue
				}
				var policy v1alpha1.KrakenDBackendPolicy
				key := types.NamespacedName{
					Name:      be.PolicyRef.Name,
					Namespace: be.PolicyRef.ResolvedNamespace(ep.Namespace),
				}
				if err := r.Get(ctx, key, &policy); err != nil {
					if errors.IsNotFound(err) {
						// Missing policy — renderer will mark endpoint as invalid
						continue
					}
					return nil, fmt.Errorf("getting policy %s: %w", key, err)
				}
				policies[mapKey] = &policy
			}
		}
	}
	return policies, nil
}

// gatherPluginConfigMaps fetches ConfigMaps referenced by plugin sources.
func (r *KrakenDGatewayReconciler) gatherPluginConfigMaps(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
) ([]corev1.ConfigMap, error) {
	if gw.Spec.Plugins == nil {
		return nil, nil
	}
	var cms []corev1.ConfigMap
	for _, src := range gw.Spec.Plugins.Sources {
		if src.ConfigMapRef == nil {
			continue
		}
		var cm corev1.ConfigMap
		key := types.NamespacedName{Name: src.ConfigMapRef.Name, Namespace: gw.Namespace}
		if err := r.Get(ctx, key, &cm); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("getting plugin configmap %s: %w", key, err)
		}
		cms = append(cms, cm)
	}
	return cms, nil
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
	dfGVK := schema.GroupVersionKind{Group: "dragonflydb.io", Version: "v1alpha1", Kind: "Dragonfly"}
	available, err := r.crdAvailable(dfGVK)
	if err != nil {
		log.Error(err, "failed to check Dragonfly CRD availability")
		return nil
	}
	if !available {
		log.V(1).Info("Dragonfly CRD not installed, skipping state detection")
		return nil
	}

	dfName := resources.DragonflyName(gw)
	df := &unstructured.Unstructured{}
	df.SetGroupVersionKind(dfGVK)

	key := types.NamespacedName{Name: dfName, Namespace: gw.Namespace}
	if err := r.Get(ctx, key, df); err != nil {
		if errors.IsNotFound(err) {
			meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionDragonflyReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gw.Generation,
				Reason:             v1alpha1.ReasonDragonflyNotReady,
				Message:            "Dragonfly CR not yet created",
			})
			dragonflyReady.WithLabelValues(gw.Namespace, gw.Name).Set(0)
			return &renderer.DragonflyState{Enabled: true, ServiceDNS: resources.DragonflyServiceDNS(gw)}
		}
		log.Error(err, "failed to get Dragonfly CR", "name", dfName)
		dragonflyReady.WithLabelValues(gw.Namespace, gw.Name).Set(0)
		return &renderer.DragonflyState{Enabled: true, ServiceDNS: resources.DragonflyServiceDNS(gw)}
	}

	// Check Dragonfly status phase — absent field defaults to empty string
	phase, _, err := unstructured.NestedString(df.Object, "status", "phase")
	if err != nil {
		log.V(1).Info("unable to read Dragonfly status phase", "error", err)
	}
	isReady := phase == "ready"

	if isReady {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionDragonflyReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             "DragonflyReady",
			Message:            "Dragonfly instance is ready",
		})
		dragonflyReady.WithLabelValues(gw.Namespace, gw.Name).Set(1)
		gw.Status.DragonflyAddress = resources.DragonflyServiceDNS(gw)
	} else {
		// Only emit DragonflyNotReady event on condition transition
		prevCond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionDragonflyReady)
		wasReady := prevCond != nil && prevCond.Status == metav1.ConditionTrue

		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionDragonflyReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonDragonflyNotReady,
			Message:            fmt.Sprintf("Dragonfly phase: %s", phase),
		})
		dragonflyReady.WithLabelValues(gw.Namespace, gw.Name).Set(0)
		if wasReady || prevCond == nil {
			r.Recorder.Event(gw, "Warning", v1alpha1.ReasonDragonflyNotReady,
				fmt.Sprintf("Dragonfly instance is not ready (phase: %s)", phase))
		}
	}

	return &renderer.DragonflyState{Enabled: true, ServiceDNS: resources.DragonflyServiceDNS(gw)}
}

// inspectDeploymentStatus reads the owned Deployment's status and updates
// the gateway's replica counts, Available and Progressing conditions, and
// phase based on rollout health.
func (r *KrakenDGatewayReconciler) inspectDeploymentStatus(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
) {
	log := logf.FromContext(ctx)
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		if errors.IsNotFound(err) {
			return
		}
		log.Error(err, "failed to get deployment for status inspection")
		return
	}

	// Propagate observed replica counts.
	gw.Status.Replicas = dep.Status.Replicas
	gw.Status.ReadyReplicas = dep.Status.ReadyReplicas

	// Check for ProgressDeadlineExceeded.
	for _, c := range dep.Status.Conditions {
		if c.Type != appsv1.DeploymentProgressing ||
			c.Status != corev1.ConditionFalse ||
			c.Reason != "ProgressDeadlineExceeded" {
			continue
		}
		gw.Status.Phase = v1alpha1.PhaseError
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
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
		r.Recorder.Event(gw, "Warning", v1alpha1.ReasonRolloutFailed,
			"Deployment exceeded its progress deadline")
		return
	}

	// Detect rollout convergence: all replicas updated and available.
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if dep.Status.UpdatedReplicas == desired &&
		dep.Status.AvailableReplicas == desired {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionProgressing,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             "RolloutComplete",
			Message:            "Deployment rollout completed successfully",
		})
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAvailable,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             "DeploymentAvailable",
			Message:            "All replicas are available",
		})
		// Clear Deploying phase on convergence (Degraded/Error take precedence)
		if gw.Status.Phase == v1alpha1.PhaseDeploying {
			gw.Status.Phase = v1alpha1.PhaseRunning
		}
	}
}

// validateConfig runs the krakend check validation pipeline.
func (r *KrakenDGatewayReconciler) validateConfig(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	jsonData []byte,
	ceFallback bool,
) error {
	eeWithoutFallback := gw.Spec.Edition == v1alpha1.EditionEE && !ceFallback
	validationJSON, err := r.Validator.PrepareValidationCopy(jsonData, eeWithoutFallback)
	if err != nil {
		return fmt.Errorf("preparing validation copy: %w", err)
	}
	return r.Validator.Validate(ctx, validationJSON)
}

// handleValidationError sets the appropriate status conditions when config
// validation fails.
func (r *KrakenDGatewayReconciler) handleValidationError(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	validationErr error,
) error {
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionConfigValid,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonConfigValidationFailed,
		Message:            validationErr.Error(),
	})
	gw.Status.Phase = v1alpha1.PhaseError
	r.Recorder.Event(gw, "Warning", v1alpha1.ReasonConfigValidationFailed, validationErr.Error())
	if err := r.Status().Update(ctx, gw); err != nil {
		return fmt.Errorf("updating status after validation failure: %w", err)
	}
	return nil
}

// updateEndpointStatuses marks conflicted and invalid endpoints.
func (r *KrakenDGatewayReconciler) updateEndpointStatuses(
	ctx context.Context,
	output *renderer.RenderOutput,
) error {
	for _, nn := range output.ConflictedEndpoints {
		var ep v1alpha1.KrakenDEndpoint
		if err := r.Get(ctx, nn, &ep); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("getting conflicted endpoint %s: %w", nn, err)
		}
		ep.Status.Phase = v1alpha1.EndpointPhaseConflicted
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAvailable,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: ep.Generation,
			Reason:             v1alpha1.ReasonEndpointConflict,
			Message:            "Endpoint path/method conflicts with an older KrakenDEndpoint",
		})
		if err := r.Status().Update(ctx, &ep); err != nil {
			return fmt.Errorf("updating conflicted endpoint status %s: %w", nn, err)
		}
		r.Recorder.Event(&ep, "Warning", v1alpha1.ReasonEndpointConflict,
			"Endpoint excluded due to path/method conflict with older resource")
	}
	for _, nn := range output.InvalidEndpoints {
		var ep v1alpha1.KrakenDEndpoint
		if err := r.Get(ctx, nn, &ep); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("getting invalid endpoint %s: %w", nn, err)
		}
		ep.Status.Phase = v1alpha1.EndpointPhaseInvalid
		meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAvailable,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: ep.Generation,
			Reason:             v1alpha1.ReasonEndpointInvalid,
			Message:            "Endpoint excluded due to missing policy reference",
		})
		if err := r.Status().Update(ctx, &ep); err != nil {
			return fmt.Errorf("updating invalid endpoint status %s: %w", nn, err)
		}
		r.Recorder.Event(&ep, "Warning", v1alpha1.ReasonEndpointInvalid,
			"Endpoint excluded due to missing policy reference")
	}
	return nil
}

// reconcileOwnedResources creates or updates all Kubernetes resources owned
// by the gateway using the create-or-update pattern.
func (r *KrakenDGatewayReconciler) reconcileOwnedResources(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	output *renderer.RenderOutput,
) error {
	log := logf.FromContext(ctx)
	errCRDMissing := fmt.Errorf("CRD not installed")

	// ServiceAccount
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		resources.BuildServiceAccount(sa, gw)
		return controllerutil.SetControllerReference(gw, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconciling serviceaccount: %w", err)
	}

	// Service
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		resources.BuildService(svc, gw)
		return controllerutil.SetControllerReference(gw, svc, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconciling service: %w", err)
	}

	// ConfigMap (may already exist from rendering pipeline)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		resources.BuildConfigMap(cm, gw, output.JSON)
		return controllerutil.SetControllerReference(gw, cm, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconciling configmap: %w", err)
	}

	// PodDisruptionBudget
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		resources.BuildPDB(pdb, gw)
		return controllerutil.SetControllerReference(gw, pdb, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconciling pdb: %w", err)
	}

	// Deployment
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: gw.Name, Namespace: gw.Namespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		resources.BuildDeployment(dep, gw, output.Checksum, output.PluginChecksum, output.DesiredImage)
		return controllerutil.SetControllerReference(gw, dep, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconciling deployment: %w", err)
	}

	// HPA (only if autoscaling is configured)
	if gw.Spec.Autoscaling != nil {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
			Name: gw.Name, Namespace: gw.Namespace,
		}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
			resources.BuildHPA(hpa, gw)
			return controllerutil.SetControllerReference(gw, hpa, r.Scheme)
		}); err != nil {
			return fmt.Errorf("reconciling hpa: %w", err)
		}
	}

	// Post-restart Job (only if enabled, and only after rollout convergence
	// for the current config checksum). Jobs are idempotent by name so each
	// unique config revision produces exactly one Job.
	if err := r.reconcilePostRestartJob(ctx, gw, output.Checksum); err != nil {
		return err
	}

	// Dragonfly (only if enabled AND CRD is installed)
	if gw.Spec.Dragonfly != nil && gw.Spec.Dragonfly.Enabled {
		dfGVK := schema.GroupVersionKind{Group: "dragonflydb.io", Version: "v1alpha1", Kind: "Dragonfly"}
		dfAvailable, dfErr := r.crdAvailable(dfGVK)
		if dfErr != nil {
			return fmt.Errorf("checking Dragonfly CRD: %w", dfErr)
		}
		if !dfAvailable {
			log.Error(errCRDMissing,
				"Dragonfly requested but dragonflydb.io CRD is not available")
			r.Recorder.Event(gw, "Warning", "CRDNotInstalled",
				"Dragonfly is enabled but the dragonflydb.io CRD is not installed in the cluster")
		} else {
			df := &unstructured.Unstructured{}
			df.SetGroupVersionKind(dfGVK)
			df.SetName(resources.DragonflyName(gw))
			df.SetNamespace(gw.Namespace)
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, df, func() error {
				resources.BuildDragonfly(df, gw)
				return controllerutil.SetControllerReference(gw, df, r.Scheme)
			}); err != nil {
				return fmt.Errorf("reconciling dragonfly: %w", err)
			}
			r.recordDragonflyRunAsRootCondition(gw, df)
		}
	} else {
		// Review round 4, D3: Dragonfly is deliberately off (unset or
		// Enabled: false) — mirrors reconcilePostRestartJob's
		// disabled/empty guard (see the spec == nil || !spec.Enabled branch
		// above, ~line 855) so `kubectl describe krakendgateway` does not
		// keep showing a stale ConditionDragonflyRunAsRootUnacknowledged
		// forever after the user disables Dragonfly. Deliberately NOT
		// cleared when Dragonfly is enabled but !dfAvailable (CRD not yet
		// installed) — that is a transient/environmental state, not a
		// deliberate disable, mirroring reconcilePostRestartJob's
		// configChecksum == "" reasoning (~line 870) for not flickering
		// conditions away during an in-progress/incomplete state.
		meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionDragonflyRunAsRootUnacknowledged)
	}

	// ExternalSecret (only if license.externalSecret is enabled AND CRD is installed)
	if gw.Spec.License != nil && gw.Spec.License.ExternalSecret.Enabled {
		esGVK := schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
		esAvailable, esErr := r.crdAvailable(esGVK)
		if esErr != nil {
			return fmt.Errorf("checking ExternalSecret CRD: %w", esErr)
		}
		if !esAvailable {
			log.Error(errCRDMissing,
				"ExternalSecret requested but external-secrets.io CRD is not available")
			r.Recorder.Event(gw, "Warning", "CRDNotInstalled",
				"ExternalSecret is enabled but the external-secrets.io CRD is not installed in the cluster")
		} else {
			es := &unstructured.Unstructured{}
			es.SetGroupVersionKind(esGVK)
			es.SetName(resources.ExternalSecretName(gw))
			es.SetNamespace(gw.Namespace)
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, es, func() error {
				resources.BuildExternalSecret(es, gw)
				return controllerutil.SetControllerReference(gw, es, r.Scheme)
			}); err != nil {
				return fmt.Errorf("reconciling externalsecret: %w", err)
			}
		}
	}

	// VirtualService (only if Istio is enabled AND CRD is installed)
	if gw.Spec.Istio != nil && gw.Spec.Istio.Enabled {
		vsGVK := schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "VirtualService"}
		vsAvailable, vsErr := r.crdAvailable(vsGVK)
		if vsErr != nil {
			return fmt.Errorf("checking VirtualService CRD: %w", vsErr)
		}
		if !vsAvailable {
			log.Error(errCRDMissing,
				"VirtualService requested but networking.istio.io CRD is not available")
			r.Recorder.Event(gw, "Warning", "CRDNotInstalled",
				"Istio is enabled but the networking.istio.io VirtualService CRD is not installed in the cluster")
		} else {
			vs := &unstructured.Unstructured{}
			vs.SetGroupVersionKind(vsGVK)
			vs.SetName(gw.Name)
			vs.SetNamespace(gw.Namespace)
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, vs, func() error {
				resources.BuildVirtualService(vs, gw)
				return controllerutil.SetControllerReference(gw, vs, r.Scheme)
			}); err != nil {
				return fmt.Errorf("reconciling virtualservice: %w", err)
			}
			meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionIstioConfigured,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gw.Generation,
				Reason:             v1alpha1.ReasonIstioVSCreated,
				Message:            "Istio VirtualService reconciled",
			})
			r.Recorder.Event(gw, "Normal", v1alpha1.ReasonIstioVSCreated, "Istio VirtualService reconciled")
		}
	}

	return nil
}

// reconcilePostRestartJob creates a Job to run the user-provided bash script
// after the gateway rolls out the current config revision. The Job is named
// with a short prefix of the combined checksum (see
// resources.PostRestartJobChecksum) so each (config, postRestartJob spec)
// revision pair produces at most one Job under that name, ever — including
// a spec-only edit, not just a krakend.json change (nhig root cause 2). The
// Job is only created after the Deployment has converged on the current
// config checksum.
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
	configChecksum string,
) error {
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
		return r.reconcileExistingPostRestartRevision(ctx, gw, spec, jobName, jobChecksum, configChecksum)
	}

	// review id 3807285652 (#7): the Deployment-not-found / not-yet-converged
	// early returns below (through the desired==0 and replica-mismatch
	// checks) deliberately do NOT touch PostRestartJobSkipped/ROFS
	// conditions, unlike the disabled/empty guard above. These describe an
	// in-progress rollout, not a completed decision about this revision —
	// clearing conditions here would make them flicker away and back every
	// reconcile while a rollout is merely underway.
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting deployment for post-restart check: %w", err)
	}

	annot := dep.Spec.Template.Annotations[resources.PostRestartJobChecksumAnnotation]
	if annot != configChecksum {
		return nil
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if desired == 0 {
		// Deployment is intentionally scaled to zero — no pods have rolled
		// so a post-restart Job must not be created.
		return nil
	}
	if dep.Status.UpdatedReplicas != desired || dep.Status.AvailableReplicas != desired {
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
			// recorded as this revision's (e.g. reconcileExistingPostRestart
			// Revision's recreate path cleared it before a Delete that then
			// failed transiently — review id 3807285616, #1b — leaving the
			// OLD failed Job in place under this name; or a prior status
			// write was lost before it could record a Job that had already
			// run and failed). Either way, say so plainly instead of
			// implying a healthy "created"/"exists" outcome for a Job that
			// has not successfully completed — restoring the checksum here
			// (above) means the next reconcile re-enters
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
//
// Every branch above also backfills the ROFS posture condition (review id
// 3807285633, #3b) — not just create/re-create — so an already-run
// gateway that never hits this function's create path again still carries
// the posture signal.
func (r *KrakenDGatewayReconciler) reconcileExistingPostRestartRevision(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	spec *v1alpha1.PostRestartJobSpec,
	jobName string,
	jobChecksum string,
	configChecksum string,
) error {
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
	// in-memory below and persisted by the caller's final Status().Update
	// at the end of Reconcile.
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
// config checksum present AND the Deployment has converged on that
// checksum — i.e. every branch of reconcileExistingPostRestartRevision plus
// the create/already-exists branches of reconcilePostRestartJob. It does
// NOT cover the disabled/unconfigured guard or the not-yet-converged early
// returns in reconcilePostRestartJob, which precede any revision-specific
// decision existing at all. The disabled/unconfigured guard instead
// actively REMOVES this condition (and the ROFS one); the not-yet-converged
// returns intentionally leave prior conditions untouched (see the comments
// at each of those call sites).
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
// carry an unacknowledged runAsUser: 0 request (review round 3, C2). df is
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

	// Review round 4, D5b: the False state previously overloaded
	// ReasonDragonflyRunAsRootAcknowledged for both an acknowledged root
	// request AND the far more common no-root-request-at-all case. Split
	// into two distinct reasons so a viewer can tell "someone requested
	// root and explicitly acknowledged it" apart from "this gateway never
	// requested root".
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
		client.MatchingFields{EndpointPolicyIndex: indexKey},
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
		if gw.Spec.License == nil {
			continue
		}
		if gw.Spec.License.SecretRef != nil &&
			gw.Spec.License.SecretRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace},
			})
			continue
		}
		if gw.Spec.License.ExternalSecret.Enabled &&
			obj.GetName() == gw.Name+"-license" {
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
