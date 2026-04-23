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
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	utilclock "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
)

const defaultCUEDefinitionsConfigMap = "krakend-cue-definitions"

// KrakenDAutoConfigReconciler reconciles a KrakenDAutoConfig object.
// It orchestrates the OpenAPI-to-endpoint pipeline: fetch spec,
// evaluate CUE, filter, generate, and diff/create/update/delete endpoints.
type KrakenDAutoConfigReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     record.EventRecorder
	Fetcher      autoconfig.Fetcher
	CUEEvaluator autoconfig.CUEEvaluator
	Filter       autoconfig.Filter
	Generator    autoconfig.Generator
	Clock        utilclock.Clock
}

// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendautoconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendautoconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendautoconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the autoconfig pipeline: fetch → checksum → CUE evaluate
// → filter → generate → diff/create/update/delete endpoints.
func (r *KrakenDAutoConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ac v1alpha1.KrakenDAutoConfig
	if err := r.Get(ctx, req.NamespacedName, &ac); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting autoconfig %s: %w", req.NamespacedName, err)
	}

	if ac.Status.Phase == "" {
		ac.Status.Phase = v1alpha1.AutoConfigPhasePending
		if err := r.Status().Update(ctx, &ac); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting initial phase: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Phase: Fetching — only update if phase actually changed to avoid
	// triggering unnecessary Watch events that cause reconciliation loops.
	if ac.Status.Phase != v1alpha1.AutoConfigPhaseFetching {
		ac.Status.Phase = v1alpha1.AutoConfigPhaseFetching
		if err := r.Status().Update(ctx, &ac); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting phase Fetching: %w", err)
		}
	}

	fetchResult, err := r.Fetcher.Fetch(ctx, autoconfig.FetchSource{
		URL:               ac.Spec.OpenAPI.URL,
		ConfigMapRef:      ac.Spec.OpenAPI.ConfigMapRef,
		Auth:              ac.Spec.OpenAPI.Auth,
		AllowClusterLocal: ac.Spec.OpenAPI.AllowClusterLocal,
		Namespace:         ac.Namespace,
	})
	if err != nil {
		return r.handleFetchError(ctx, &ac, err)
	}

	// Resolve external $refs (only possible with HTTP sources). Warnings
	// from unresolved refs are logged but do not fail reconciliation.
	if ac.Spec.OpenAPI.URL != "" {
		resolved, warnings, resolveErr := autoconfig.ResolveExternalRefs(
			ctx, fetchResult.Data, ac.Spec.OpenAPI.URL, r.Fetcher,
			autoconfig.FetchSource{
				Auth:              ac.Spec.OpenAPI.Auth,
				AllowClusterLocal: ac.Spec.OpenAPI.AllowClusterLocal,
				Namespace:         ac.Namespace,
			},
		)
		if resolveErr != nil {
			log.Error(resolveErr, "external $ref resolution failed, using raw spec")
		} else {
			fetchResult.Data = resolved
		}
		for _, w := range warnings {
			log.V(1).Info("ref resolver warning", "warning", w)
		}
	}

	// Strip upstream `servers` entries: the KrakenD gateway is the
	// externally-visible server, so upstream URLs must not bleed into
	// generated documentation or endpoint configuration.
	if stripped, stripErr := autoconfig.StripServers(fetchResult.Data); stripErr != nil {
		log.Error(stripErr, "stripping upstream servers failed, using raw spec")
	} else {
		fetchResult.Data = stripped
	}

	// Recompute checksum from the final (possibly resolved / stripped) data
	// so changes from external $ref resolution or server stripping are not
	// silently skipped by the downstream checksum gate.
	fetchResult.Checksum = fmt.Sprintf("%x", sha256.Sum256(fetchResult.Data))

	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSpecAvailable,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ac.Generation,
		Reason:             v1alpha1.ReasonSpecFetched,
		Message:            "OpenAPI spec fetched successfully",
	})

	// Check if spec or CUE defs changed
	cueDefsRV := r.getCUEDefsResourceVersion(ctx, &ac)
	// Include generation so spec-only changes (overrides, defaults,
	// urlTransform, filter) also trigger re-evaluation even when the
	// OpenAPI spec and CUE definitions are unchanged.
	combinedChecksum := fmt.Sprintf("%s:%s:%d", fetchResult.Checksum, cueDefsRV, ac.Generation)
	if combinedChecksum == ac.Status.SpecChecksum {
		if ac.Status.Phase != v1alpha1.AutoConfigPhaseSynced {
			ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
			if err := r.Status().Update(ctx, &ac); err != nil {
				return ctrl.Result{}, fmt.Errorf("updating synced status: %w", err)
			}
		}
		log.V(1).Info("spec unchanged, skipping re-evaluation")
		return r.requeueResult(&ac), nil
	}

	// Phase: Rendering
	if ac.Status.Phase != v1alpha1.AutoConfigPhaseRendering {
		ac.Status.Phase = v1alpha1.AutoConfigPhaseRendering
		if err := r.Status().Update(ctx, &ac); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting phase Rendering: %w", err)
		}
	}

	// Load CUE definitions: prefer ConfigMap, fall back to embedded defaults
	defaultDefs, err := r.loadCUEDefinitions(ctx, ac.Namespace, defaultCUEDefinitionsConfigMap)
	if err != nil {
		if !errors.IsNotFound(err) {
			return r.handleCUEError(ctx, &ac, fmt.Errorf("loading default CUE definitions: %w", err))
		}
		defaultDefs, err = autoconfig.EmbeddedCUEDefinitions()
		if err != nil {
			return r.handleCUEError(ctx, &ac, fmt.Errorf("loading embedded CUE definitions: %w", err))
		}
	}

	var customDefs map[string]string
	if ac.Spec.CUE != nil && ac.Spec.CUE.DefinitionsConfigMapRef != nil {
		customDefs, err = r.loadCUEDefinitions(ctx, ac.Namespace, ac.Spec.CUE.DefinitionsConfigMapRef.Name)
		if err != nil {
			return r.handleCUEError(ctx, &ac, fmt.Errorf("loading custom CUE definitions: %w", err))
		}
	}

	env := ""
	if ac.Spec.CUE != nil {
		env = ac.Spec.CUE.Environment
	}

	// CUE evaluation
	cueOutput, err := r.CUEEvaluator.Evaluate(ctx, autoconfig.CUEInput{
		SpecData:     fetchResult.Data,
		SpecFormat:   ac.Spec.OpenAPI.Format,
		DefaultDefs:  defaultDefs,
		CustomDefs:   customDefs,
		Defaults:     ac.Spec.Defaults,
		Overrides:    ac.Spec.Overrides,
		URLTransform: ac.Spec.URLTransform,
		Environment:  env,
		ServiceName:  "_spec",
		DefaultHost:  extractHost(ac.Spec.OpenAPI.URL),
	})
	if err != nil {
		return r.handleCUEError(ctx, &ac, err)
	}

	// Apply filters
	filtered := cueOutput.Entries
	if ac.Spec.Filter != nil {
		filtered = r.Filter.Apply(cueOutput.Entries, cueOutput.Tags, cueOutput.OperationIDs, *ac.Spec.Filter)
	}

	// Extract component schemas from the spec before CUE evaluation
	// so they can be attached to each generated KrakenDEndpoint CR.
	componentSchemas := autoconfig.ExtractComponentSchemas(fetchResult.Data)

	// Generate endpoint CRs
	genOutput, err := r.Generator.Generate(ctx, autoconfig.GenerateInput{
		AutoConfig:       &ac,
		Entries:          filtered,
		OperationIDs:     cueOutput.OperationIDs,
		GatewayRef:       ac.Spec.GatewayRef,
		ComponentSchemas: componentSchemas,
	})
	if err != nil {
		return r.handleCUEError(ctx, &ac, fmt.Errorf("generating endpoints: %w", err))
	}

	// Emit events for duplicate operations
	for _, dup := range genOutput.Duplicates {
		r.Recorder.Eventf(&ac, "Warning", v1alpha1.ReasonDuplicateOperationId,
			"Duplicate operation %q skipped", dup)
	}

	// Diff and reconcile endpoints
	if err := r.reconcileEndpoints(ctx, &ac, genOutput.Endpoints); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling endpoints: %w", err)
	}

	// Update status
	now := metav1.Now()
	ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
	ac.Status.SpecChecksum = combinedChecksum
	ac.Status.LastSyncTime = &now
	ac.Status.GeneratedEndpoints = len(genOutput.Endpoints)
	ac.Status.SkippedOperations = genOutput.SkippedOperations
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSynced,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ac.Generation,
		Reason:             "Synced",
		Message:            fmt.Sprintf("Generated %d endpoints", len(genOutput.Endpoints)),
	})
	if err := r.Status().Update(ctx, &ac); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating final status: %w", err)
	}

	r.Recorder.Eventf(&ac, "Normal", v1alpha1.ReasonEndpointsGenerated,
		"Generated %d endpoints (%d skipped)", len(genOutput.Endpoints), genOutput.SkippedOperations)

	log.V(1).Info("autoconfig reconciled",
		"phase", ac.Status.Phase,
		"endpoints", len(genOutput.Endpoints),
		"skipped", genOutput.SkippedOperations,
	)

	return r.requeueResult(&ac), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KrakenDAutoConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KrakenDAutoConfig{}).
		Owns(&v1alpha1.KrakenDEndpoint{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.cueConfigMapToAutoConfig),
		).
		Named("krakendautoconfig").
		Complete(r)
}

func (r *KrakenDAutoConfigReconciler) handleFetchError(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	fetchErr error,
) (ctrl.Result, error) {
	ac.Status.Phase = v1alpha1.AutoConfigPhaseError
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSpecAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ac.Generation,
		Reason:             v1alpha1.ReasonSpecFetchFailed,
		Message:            fetchErr.Error(),
	})
	if err := r.Status().Update(ctx, ac); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating fetch error status: %w", err)
	}
	r.Recorder.Event(ac, "Warning", v1alpha1.ReasonSpecFetchFailed, fetchErr.Error())
	// For periodic triggers, requeue via interval; for OnChange, return error
	// so controller-runtime retries with exponential backoff.
	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic {
		return r.requeueResult(ac), nil
	}
	return ctrl.Result{}, fetchErr
}

func (r *KrakenDAutoConfigReconciler) handleCUEError(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	cueErr error,
) (ctrl.Result, error) {
	ac.Status.Phase = v1alpha1.AutoConfigPhaseError
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSynced,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ac.Generation,
		Reason:             v1alpha1.ReasonCUEEvaluationFailed,
		Message:            cueErr.Error(),
	})
	if err := r.Status().Update(ctx, ac); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating CUE error status: %w", err)
	}
	r.Recorder.Event(ac, "Warning", v1alpha1.ReasonCUEEvaluationFailed, cueErr.Error())
	// For periodic triggers, requeue via interval; for OnChange, return error
	// so controller-runtime retries with exponential backoff.
	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic {
		return r.requeueResult(ac), nil
	}
	return ctrl.Result{}, cueErr
}

func (r *KrakenDAutoConfigReconciler) requeueResult(ac *v1alpha1.KrakenDAutoConfig) ctrl.Result {
	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic && ac.Spec.Periodic != nil {
		return ctrl.Result{RequeueAfter: ac.Spec.Periodic.Interval.Duration}
	}
	return ctrl.Result{}
}

func (r *KrakenDAutoConfigReconciler) getCUEDefsResourceVersion(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
) string {
	rv := ""
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{
		Name:      defaultCUEDefinitionsConfigMap,
		Namespace: ac.Namespace,
	}, &cm); err == nil {
		rv = cm.ResourceVersion
	}
	if ac.Spec.CUE != nil && ac.Spec.CUE.DefinitionsConfigMapRef != nil {
		var customCM corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{
			Name:      ac.Spec.CUE.DefinitionsConfigMapRef.Name,
			Namespace: ac.Namespace,
		}, &customCM); err == nil {
			rv += ":" + customCM.ResourceVersion
		}
	}
	return rv
}

func (r *KrakenDAutoConfigReconciler) loadCUEDefinitions(
	ctx context.Context,
	namespace string,
	configMapName string,
) (map[string]string, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: namespace,
	}, &cm); err != nil {
		return nil, fmt.Errorf("getting CUE definitions ConfigMap %s: %w", configMapName, err)
	}
	return cm.Data, nil
}

func (r *KrakenDAutoConfigReconciler) reconcileEndpoints(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	desired []*v1alpha1.KrakenDEndpoint,
) error {
	// Build set of desired endpoint names
	desiredNames := map[string]struct{}{}
	for _, ep := range desired {
		desiredNames[ep.Name] = struct{}{}
	}

	// List existing generated endpoints owned by this autoconfig
	var existing v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &existing,
		client.InNamespace(ac.Namespace),
		client.MatchingLabels{"gateway.krakend.io/autoconfig": ac.Name},
	); err != nil {
		return fmt.Errorf("listing existing endpoints: %w", err)
	}

	// Delete endpoints that are no longer desired
	for i := range existing.Items {
		if _, ok := desiredNames[existing.Items[i].Name]; !ok {
			if err := r.Delete(ctx, &existing.Items[i]); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("deleting endpoint %s: %w", existing.Items[i].Name, err)
			}
		}
	}

	// Create or update desired endpoints
	for _, ep := range desired {
		existing := &v1alpha1.KrakenDEndpoint{ObjectMeta: metav1.ObjectMeta{
			Name:      ep.Name,
			Namespace: ep.Namespace,
		}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
			existing.Labels = ep.Labels
			existing.Spec = ep.Spec
			return controllerutil.SetControllerReference(ac, existing, r.Scheme)
		}); err != nil {
			return fmt.Errorf("upserting endpoint %s: %w", ep.Name, err)
		}
	}

	return nil
}

func (r *KrakenDAutoConfigReconciler) cueConfigMapToAutoConfig(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}

	var acList v1alpha1.KrakenDAutoConfigList
	if err := r.List(ctx, &acList, client.InNamespace(cm.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range acList.Items {
		ac := &acList.Items[i]
		if cm.Name == defaultCUEDefinitionsConfigMap {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
			})
			continue
		}
		if ac.Spec.CUE != nil && ac.Spec.CUE.DefinitionsConfigMapRef != nil &&
			ac.Spec.CUE.DefinitionsConfigMapRef.Name == cm.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
			})
		}
	}
	return requests
}

// extractHost returns the scheme, host, and optional port from an absolute URL string.
// For example, "http://svc.ns.svc.cluster.local:8080/swagger/v1/swagger.json"
// becomes "http://svc.ns.svc.cluster.local:8080".
// It returns an empty string for non-absolute URLs or invalid inputs.
func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
