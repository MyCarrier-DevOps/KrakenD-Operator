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

package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
		client.MatchingFields{endpointGatewayIndex: indexKey},
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
func (r *KrakenDGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ensureEndpointIndexes(mgr); err != nil {
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
		}
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
