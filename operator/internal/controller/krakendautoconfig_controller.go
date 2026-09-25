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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
)

const defaultCUEDefinitionsConfigMap = "krakend-cue-definitions"

// defaultResyncInterval is how often OnChange AutoConfigs are re-polled.
// OnChange AutoConfigs react to watch events immediately and are also
// re-polled on this interval so upstream spec changes and out-of-band
// endpoint changes are always converged.
const defaultResyncInterval = 5 * time.Minute

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

// Reconcile implements the autoconfig pipeline: fetch → CUE evaluate → filter
// → generate → diff/create/update/delete endpoints → status. Every reconcile
// runs the whole pipeline, so owned endpoints always converge to the desired
// state; a successful reconcile that finds nothing to change writes nothing.
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
	origStatus := ac.Status.DeepCopy()

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

	r.postProcessSpec(ctx, &ac, fetchResult)

	// Recompute checksum from the final (possibly resolved / stripped) data
	// so status.specChecksum also changes with external $ref resolution or
	// server stripping.
	fetchResult.Checksum = fmt.Sprintf("%x", sha256.Sum256(fetchResult.Data))

	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSpecAvailable,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ac.Generation,
		Reason:             v1alpha1.ReasonSpecFetched,
		Message:            "OpenAPI spec fetched successfully",
	})

	cueDefsRV := r.getCUEDefsResourceVersion(ctx, &ac)
	combinedChecksum := autoConfigSpecChecksum(fetchResult.Checksum, cueDefsRV, ac.Generation)

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

	// Surface entries the evaluator skipped (e.g. failed to marshal) as
	// events before the unmatched-override check below, so they remain
	// visible even when that check then fails the sync.
	for _, warning := range cueOutput.Warnings {
		r.Recorder.Event(&ac, "Warning", v1alpha1.ReasonCUEEvaluationWarning, warning)
	}

	// An override whose operationId matched no generated entry must fail
	// closed rather than be silently dropped: overrides can carry
	// security-relevant config (e.g. auth/validator).
	if len(cueOutput.UnmatchedOverrides) > 0 {
		unmatchedErr := fmt.Errorf(
			"spec.overrides reference operationIds not present in the OpenAPI spec: %s",
			strings.Join(cueOutput.UnmatchedOverrides, ", "))
		return r.handleSyncedFailure(ctx, &ac, v1alpha1.ReasonUnmatchedOverride, unmatchedErr)
	}

	// Apply filters
	filtered := cueOutput.Entries
	if ac.Spec.Filter != nil {
		filtered = r.Filter.Apply(cueOutput.Entries, cueOutput.Tags, cueOutput.OperationIDs, *ac.Spec.Filter)
	}

	filtered, scopeErr := r.applyAdditionalEndpoints(&ac, filtered)
	if scopeErr != nil {
		return r.handleSyncedFailure(ctx, &ac, v1alpha1.ReasonAdditionalEndpointScopeFailed, scopeErr)
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
	changes, err := r.reconcileEndpoints(ctx, &ac, genOutput.Endpoints)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling endpoints: %w", err)
	}

	if err := r.recordSync(ctx, &ac, origStatus, combinedChecksum, genOutput, changes); err != nil {
		return ctrl.Result{}, err
	}

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
		For(&v1alpha1.KrakenDAutoConfig{}, builder.WithPredicates(autoConfigPredicate())).
		Owns(&v1alpha1.KrakenDEndpoint{}, builder.WithPredicates(ownedEndpointPredicate())).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.configMapToAutoConfigs),
		).
		Named("krakendautoconfig").
		Complete(r)
}

// autoConfigPredicate gates the primary KrakenDAutoConfig watch. Status-only
// updates (the phase transitions Reconcile writes itself) must not
// re-enqueue the object — that is what caused the status-write loop.
// Generation covers spec changes; labels are included because application
// deploys relabel the AutoConfig; annotations are included so `kubectl
// annotate` can force an immediate reconcile.
func autoConfigPredicate() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.LabelChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
	)
}

// ownedEndpointPredicate gates the Owns(KrakenDEndpoint) watch: endpoint
// status updates alone must not re-enqueue the owning AutoConfig, only spec
// changes (generation bumps) and deletes (e.g. drift from an external actor).
func ownedEndpointPredicate() predicate.Predicate {
	return predicate.GenerationChangedPredicate{}
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

// handleSyncedFailure fails the sync with the given reason and error: it sets
// phase Error, the Synced condition to False, records a Warning event, and
// returns the requeue/error semantics shared by every Synced-failure path —
// requeue via interval for periodic triggers, otherwise the error itself so
// controller-runtime retries with exponential backoff.
func (r *KrakenDAutoConfigReconciler) handleSyncedFailure(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	reason string,
	syncErr error,
) (ctrl.Result, error) {
	ac.Status.Phase = v1alpha1.AutoConfigPhaseError
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSynced,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ac.Generation,
		Reason:             reason,
		Message:            syncErr.Error(),
	})
	if err := r.Status().Update(ctx, ac); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating %s status: %w", reason, err)
	}
	r.Recorder.Event(ac, "Warning", reason, syncErr.Error())
	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic {
		return r.requeueResult(ac), nil
	}
	return ctrl.Result{}, syncErr
}

func (r *KrakenDAutoConfigReconciler) handleCUEError(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	cueErr error,
) (ctrl.Result, error) {
	return r.handleSyncedFailure(ctx, ac, v1alpha1.ReasonCUEEvaluationFailed, cueErr)
}

// applyAdditionalEndpoints builds, transforms, scopes, and merges additional
// endpoints into the filtered set. It returns the combined slice, or a non-nil
// error when no base path can be determined.
func (r *KrakenDAutoConfigReconciler) applyAdditionalEndpoints(
	ac *v1alpha1.KrakenDAutoConfig,
	filtered []v1alpha1.EndpointEntry,
) ([]v1alpha1.EndpointEntry, error) {
	if len(ac.Spec.AdditionalEndpoints) == 0 {
		return filtered, nil
	}

	additional := autoconfig.BuildAdditionalEntries(
		ac.Spec.AdditionalEndpoints, ac.Spec.Defaults, extractHost(ac.Spec.OpenAPI.URL))
	// Option B: additional endpoints receive the same URL transform as
	// spec-derived endpoints, applied before scoping/merge.
	autoconfig.ApplyURLTransformToEntries(additional, ac.Spec.URLTransform)

	// Scope additional endpoints under the application's base path:
	// manual override → addPathPrefix (already applied above) → derived.
	base := ac.Spec.AdditionalEndpointsBasePath
	hasAddPrefix := ac.Spec.URLTransform != nil && ac.Spec.URLTransform.AddPathPrefix != ""
	if base == "" && !hasAddPrefix {
		base = autoconfig.DeriveBasePath(filtered)
		if base == "" {
			return nil, fmt.Errorf(
				"cannot derive a base path for additionalEndpoints (generated " +
					"endpoints share no common parent); set " +
					"spec.additionalEndpointsBasePath or spec.urlTransform.addPathPrefix")
		}
	}
	if base != "" {
		autoconfig.ScopeAdditionalEntries(additional, base)
	}

	var replaced []string
	filtered, replaced = autoconfig.MergeAdditional(filtered, additional)
	for _, key := range replaced {
		r.Recorder.Eventf(ac, "Warning", v1alpha1.ReasonAdditionalEndpointOverride,
			"Additional endpoint %q overrides a spec-derived endpoint", key)
	}
	return filtered, nil
}

// postProcessSpec resolves external $refs and strips upstream server entries
// from the fetched spec data, updating fetchResult.Data in place.
func (r *KrakenDAutoConfigReconciler) postProcessSpec(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	fetchResult *autoconfig.FetchResult,
) {
	log := logf.FromContext(ctx)

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
}

// autoConfigSpecChecksum builds the checksum the controller stores in
// status.specChecksum to record which inputs the last sync used. It includes
// the generation so spec-only changes (overrides, defaults, urlTransform,
// filter) register as new inputs even when the OpenAPI spec and CUE
// definitions are unchanged. It does not gate evaluation — every reconcile
// evaluates — it only decides whether a sync updates LastSyncTime and records
// an EndpointsGenerated event.
func autoConfigSpecChecksum(fetchChecksum, cueDefsRV string, generation int64) string {
	return fmt.Sprintf("%s:%s:%d", fetchChecksum, cueDefsRV, generation)
}

func (r *KrakenDAutoConfigReconciler) requeueResult(ac *v1alpha1.KrakenDAutoConfig) ctrl.Result {
	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic && ac.Spec.Periodic != nil {
		return ctrl.Result{RequeueAfter: ac.Spec.Periodic.Interval.Duration}
	}
	return ctrl.Result{RequeueAfter: defaultResyncInterval}
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

// endpointChanges counts the endpoint writes one reconcileEndpoints call issued.
type endpointChanges struct {
	created, updated, deleted int
}

func (c endpointChanges) total() int {
	return c.created + c.updated + c.deleted
}

// recordSync records a successful sync: phase Synced, the Synced condition,
// the combined checksum, and the endpoint counts. LastSyncTime and the
// EndpointsGenerated event mark a sync that changed something — new inputs (a
// different combined checksum) or endpoint writes — so a steady-state
// reconcile leaves both alone. Status is written only when it differs from
// orig, the status read at the start of the reconcile.
func (r *KrakenDAutoConfigReconciler) recordSync(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	orig *v1alpha1.KrakenDAutoConfigStatus,
	combinedChecksum string,
	genOutput *autoconfig.GenerateOutput,
	changes endpointChanges,
) error {
	changed := combinedChecksum != orig.SpecChecksum || changes.total() > 0
	ac.Status.Phase = v1alpha1.AutoConfigPhaseSynced
	ac.Status.SpecChecksum = combinedChecksum
	if changed {
		now := metav1.Now()
		ac.Status.LastSyncTime = &now
	}
	ac.Status.GeneratedEndpoints = len(genOutput.Endpoints)
	ac.Status.SkippedOperations = genOutput.SkippedOperations
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSynced,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ac.Generation,
		Reason:             "Synced",
		Message:            fmt.Sprintf("Generated %d endpoints", len(genOutput.Endpoints)),
	})
	if autoConfigStatusChanged(orig, &ac.Status) {
		if err := r.Status().Update(ctx, ac); err != nil {
			return fmt.Errorf("updating final status: %w", err)
		}
	}

	if changed {
		r.Recorder.Eventf(ac, "Normal", v1alpha1.ReasonEndpointsGenerated,
			"Generated %d endpoints (%d created, %d updated, %d deleted, %d skipped)",
			len(genOutput.Endpoints), changes.created, changes.updated, changes.deleted, genOutput.SkippedOperations)
	}
	return nil
}

// autoConfigStatusChanged reports whether cur differs semantically from orig.
// Conditions are compared with conditionsEqual, which ignores
// LastTransitionTime.
func autoConfigStatusChanged(orig, cur *v1alpha1.KrakenDAutoConfigStatus) bool {
	return orig.Phase != cur.Phase ||
		orig.SpecChecksum != cur.SpecChecksum ||
		orig.GeneratedEndpoints != cur.GeneratedEndpoints ||
		orig.SkippedOperations != cur.SkippedOperations ||
		!orig.LastSyncTime.Equal(cur.LastSyncTime) ||
		!conditionsEqual(orig.Conditions, cur.Conditions)
}

func (r *KrakenDAutoConfigReconciler) reconcileEndpoints(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	desired []*v1alpha1.KrakenDEndpoint,
) (endpointChanges, error) {
	var changes endpointChanges

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
		return changes, fmt.Errorf("listing existing endpoints: %w", err)
	}

	// Delete endpoints that are no longer desired
	for i := range existing.Items {
		if _, ok := desiredNames[existing.Items[i].Name]; ok {
			continue
		}
		err := r.Delete(ctx, &existing.Items[i])
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return changes, fmt.Errorf("deleting endpoint %s: %w", existing.Items[i].Name, err)
		}
		changes.deleted++
	}

	// Create or update desired endpoints
	for _, ep := range desired {
		existing := &v1alpha1.KrakenDEndpoint{ObjectMeta: metav1.ObjectMeta{
			Name:      ep.Name,
			Namespace: ep.Namespace,
		}}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
			if !maps.Equal(existing.Labels, ep.Labels) {
				existing.Labels = ep.Labels
			}
			if !endpointSpecEqual(existing.Spec, ep.Spec) {
				existing.Spec = ep.Spec
			}
			return controllerutil.SetControllerReference(ac, existing, r.Scheme)
		})
		if err != nil {
			return changes, fmt.Errorf("upserting endpoint %s: %w", ep.Name, err)
		}
		switch op {
		case controllerutil.OperationResultCreated:
			changes.created++
		case controllerutil.OperationResultUpdated:
			changes.updated++
		case controllerutil.OperationResultNone,
			controllerutil.OperationResultUpdatedStatus,
			controllerutil.OperationResultUpdatedStatusOnly:
			// None: already in the desired state. The status results come
			// only from CreateOrPatch, never from CreateOrUpdate.
		}
	}

	return changes, nil
}

// endpointSpecEqual reports whether a and b serialize to the same JSON value.
// Embedded raw JSON (extraConfig, componentSchemas) is compared by value, so
// key order, whitespace, escaping and number formatting do not count: the API
// server re-encodes it differently from how the generator emits it, and a
// byte comparison would update every generated endpoint on every reconcile.
func endpointSpecEqual(a, b v1alpha1.KrakenDEndpointSpec) bool {
	var values [2]any
	for i, spec := range []v1alpha1.KrakenDEndpointSpec{a, b} {
		raw, err := json.Marshal(spec)
		if err != nil {
			return false
		}
		if err := json.Unmarshal(raw, &values[i]); err != nil {
			return false
		}
	}
	return reflect.DeepEqual(values[0], values[1])
}

func (r *KrakenDAutoConfigReconciler) configMapToAutoConfigs(
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
		if !autoConfigReferencesConfigMap(ac, cm.Name) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: ac.Name, Namespace: ac.Namespace},
		})
	}
	return requests
}

// autoConfigReferencesConfigMap reports whether cmName is a ConfigMap the
// given AutoConfig depends on: the default CUE definitions, its custom CUE
// definitions, or its OpenAPI spec source. A single bool per condition keeps
// configMapToAutoConfigs from enqueuing the same AutoConfig twice when more
// than one of these happens to point at the same ConfigMap.
func autoConfigReferencesConfigMap(ac *v1alpha1.KrakenDAutoConfig, cmName string) bool {
	if cmName == defaultCUEDefinitionsConfigMap {
		return true
	}
	if ac.Spec.CUE != nil && ac.Spec.CUE.DefinitionsConfigMapRef != nil &&
		ac.Spec.CUE.DefinitionsConfigMapRef.Name == cmName {
		return true
	}
	if ac.Spec.OpenAPI.ConfigMapRef != nil && ac.Spec.OpenAPI.ConfigMapRef.Name == cmName {
		return true
	}
	return false
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
