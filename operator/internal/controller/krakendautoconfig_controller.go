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
	"crypto/sha256"
	stderrors "errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	utilclock "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
)

const defaultCUEDefinitionsConfigMap = "krakend-cue-definitions"

// defaultResyncInterval is how often OnChange AutoConfigs are re-polled.
// OnChange AutoConfigs react to watch events immediately and are also
// re-polled on this interval so upstream spec changes and out-of-band
// endpoint changes are converged by the next successful sync.
const defaultResyncInterval = 5 * time.Minute

// conflictRequeueDelay is how soon a reconcile that lost a write race is
// retried: one whose status or endpoint write was rejected with a Conflict
// (or an AlreadyExists, for an endpoint create) because it acted on a stale
// cached copy. Such a reconcile requeues quietly, with no error, event or
// status change. The exception is a failed sync whose failure-status write
// conflicts: it keeps the failure's own result (see recordSyncedFailure).
const conflictRequeueDelay = time.Second

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
	// Checker runs the gateway config check over the endpoints a sync would
	// write, before it writes them.
	Checker AutoConfigChecker
	// CheckSlots bounds how many gateway config checks this reconciler's
	// workers run at once; nil means no bound. The pod's one Checker is shared
	// with the gateway controller and the admission webhooks, so a bound set
	// below its slot count, less what the others hold, means the controllers
	// never hold all of them, however many workers there are. Concurrent
	// admission requests can still take the rest.
	CheckSlots chan struct{}
	Clock      utilclock.Clock
	// MaxConcurrentReconciles is how many AutoConfigs reconcile at once;
	// zero means one.
	MaxConcurrentReconciles int
	// heldLogged remembers, per AutoConfig UID, the held operations' causes
	// this process last logged (a heldLog), so a cause is logged once per
	// change per process. It is safe for the concurrent workers.
	heldLogged sync.Map
	// FetchTimeout bounds fetching the OpenAPI spec and resolving its
	// external $refs; zero means defaultFetchTimeout.
	FetchTimeout time.Duration
}

// defaultFetchTimeout bounds one reconcile's spec fetch and external $ref
// resolution when FetchTimeout is zero. Each document fetch is also bounded
// by the fetcher's own per-request timeout.
const defaultFetchTimeout = 2 * time.Minute

// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendautoconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendautoconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendautoconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the autoconfig pipeline: fetch → CUE evaluate → filter
// → generate → diff/create/update/delete endpoints → status. Every reconcile
// runs the whole pipeline, so owned endpoints converge to the desired state
// while the AutoConfig syncs successfully. When a sync fails before or during
// its endpoint writes, existing endpoints are left as they are: it stops
// before touching them, and a failed endpoint write keeps every stale
// endpoint. An operation that fails CUE evaluation, fails the gateway config
// check that runs before the writes, loses its route to another operation, or
// whose endpoint the API server rejects, is held at its last-good endpoint
// instead: the healthy operations still converge, except that an operation
// the config check could not clear within its rounds is held too, no stale
// endpoint is deleted, and Synced is False with reason OperationsFailed
// without an error, since retrying a deterministic failure with backoff gains
// nothing. When the
// config check cannot run, nothing is written or deleted and Synced is False
// with reason ValidatorUnavailable, retried with backoff. A successful
// reconcile that finds nothing to change writes nothing and runs no check.
func (r *KrakenDAutoConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ac v1alpha1.KrakenDAutoConfig
	if err := r.Get(ctx, req.NamespacedName, &ac); err != nil {
		if errors.IsNotFound(err) {
			autoConfigSynced.DeleteLabelValues(req.Namespace, req.Name)
			r.forgetHeldCauses(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting autoconfig %s: %w", req.NamespacedName, err)
	}

	// A terminating AutoConfig is left alone: under foreground deletion it
	// lingers while garbage collection deletes its endpoints, and converging
	// would recreate each one as it goes.
	if !ac.DeletionTimestamp.IsZero() {
		autoConfigSynced.DeleteLabelValues(ac.Namespace, ac.Name)
		r.heldLogged.Delete(ac.UID)
		return ctrl.Result{}, nil
	}

	origStatus := ac.Status.DeepCopy()

	fetchResult, specNotes, err := r.fetchSpec(ctx, &ac)
	if err != nil {
		return r.handleFetchError(ctx, &ac, err)
	}

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
	// warnings holds the input warning events until the terminal status write
	// succeeds. They would otherwise repeat on every resync, so they are
	// collected only when this reconcile's combined checksum differs from the
	// checksum the last successful sync recorded. Failure paths leave
	// status.SpecChecksum unchanged, so each retry of a failing sync whose
	// inputs changed emits them again.
	warnings := &inputWarnings{inputsChanged: combinedChecksum != origStatus.SpecChecksum}
	for _, note := range capList(specWarnings(specNotes)) {
		warnings.add(v1alpha1.ReasonSpecWarning, note)
	}

	defaultDefs, customDefs, err := r.loadAllCUEDefinitions(ctx, &ac)
	if err != nil {
		return r.handleCUEError(ctx, &ac, err, warnings)
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
		Environment:  cueEnvironment(&ac),
		ServiceName:  "_spec",
		DefaultHost:  extractHost(ac.Spec.OpenAPI.URL),
	})
	if err != nil {
		return r.handleCUEError(ctx, &ac, err, warnings)
	}

	if reason, overrideErr := overrideFailure(cueOutput); overrideErr != nil {
		return r.handleSyncedFailure(ctx, &ac, reason, overrideErr, warnings)
	}

	// Apply filters
	filtered := cueOutput.Entries
	if ac.Spec.Filter != nil {
		filtered = r.Filter.Apply(cueOutput.Entries, cueOutput.Tags, cueOutput.OperationIDs, *ac.Spec.Filter)
	}

	skippedOps := r.inScope(&ac, cueOutput.Skipped)
	failedOps := r.inScope(&ac, cueOutput.Failed)

	filtered, replaced, scopeErr := applyAdditionalEndpoints(&ac, filtered, warnings)
	specNotes = append(specNotes, replaced...)
	if scopeErr != nil {
		return r.handleSyncedFailure(ctx, &ac, v1alpha1.ReasonAdditionalEndpointScopeFailed, scopeErr, warnings)
	}

	// Extract component schemas from the spec so they can be attached to each
	// generated KrakenDEndpoint CR.
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
		return r.handleCUEError(ctx, &ac, fmt.Errorf("generating endpoints: %w", err), warnings)
	}

	skippedOps = append(skippedOps, genOutput.Skipped...)

	// Unresolved schema references are spec notes like the fetch-time ones:
	// distinct, counted whole in the status message, and listed up to the cap.
	// A note already emitted for the fetch is not emitted again.
	emitted := specWarnings(specNotes)
	fresh := slices.DeleteFunc(specWarnings(genOutput.Warnings), func(note string) bool {
		return slices.Contains(emitted, note)
	})
	for _, note := range capList(fresh) {
		warnings.add(v1alpha1.ReasonSpecWarning, note)
	}
	specNotes = append(specNotes, genOutput.Warnings...)

	// Warn about duplicate operations the generator skipped
	for _, dup := range genOutput.Skipped {
		warnings.add(v1alpha1.ReasonDuplicateOperationId, fmt.Sprintf("Duplicate operation %s %s skipped: %s",
			dup.Method, dup.Path, dup.Message))
	}

	// Diff and reconcile endpoints
	outcome, err := r.reconcileEndpoints(ctx, &ac, genOutput.Endpoints, len(failedOps) > 0)
	if err != nil {
		return r.handleEndpointError(ctx, &ac, err, warnings)
	}
	if len(outcome.transient) > 0 {
		// A race beside a real failure is part of the same failed pass.
		failures := endpointFailuresError{errs: slices.Concat(outcome.transient, outcome.raced)}
		return r.handleEndpointError(ctx, &ac, failures, warnings)
	}
	if len(outcome.raced) > 0 {
		return lostWriteRace(ctx, kerrors.NewAggregate(outcome.raced))
	}

	failed := append(operationStatuses(failedOps), rejectedStatuses(outcome.rejected, cueOutput.OperationIDs)...)
	sortOperationStatuses(failed)

	if err := r.recordSync(ctx, &ac, origStatus, syncResult{
		checksum:  combinedChecksum,
		generated: len(genOutput.Endpoints),
		skipped:   operationStatuses(skippedOps),
		failed:    failed,
		warnings:  specWarnings(specNotes),
		changes:   outcome.changes,
		readiness: outcome.readiness,
	}, warnings); err != nil {
		return statusWriteFailure(ctx, err)
	}
	r.logHeldCauses(ctx, &ac, failedOps, outcome.rejected)

	log.V(1).Info("autoconfig reconciled",
		"phase", ac.Status.Phase,
		"endpoints", len(genOutput.Endpoints),
		"skipped", len(skippedOps),
		"failed", len(failed),
	)

	return r.requeueResult(&ac), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KrakenDAutoConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := fieldindex.EnsureEndpointIndexes(mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KrakenDAutoConfig{}, builder.WithPredicates(autoConfigPredicate())).
		Owns(&v1alpha1.KrakenDEndpoint{}, builder.WithPredicates(ownedEndpointPredicate())).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.configMapToAutoConfigs),
		).
		WithOptions(crcontroller.Options{
			RateLimiter:             newAutoConfigRateLimiter(),
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
		}).
		Named("krakendautoconfig").
		Complete(r)
}

// inScope returns the issues whose operations spec.filter keeps, applying
// the same rules the filter applies to entries, so an operation the user
// excluded is neither reported nor holds anything back. Each issue is judged
// on its own: two can share a path and method.
func (r *KrakenDAutoConfigReconciler) inScope(
	ac *v1alpha1.KrakenDAutoConfig,
	issues []autoconfig.OperationIssue,
) []autoconfig.OperationIssue {
	if ac.Spec.Filter == nil || len(issues) == 0 {
		return issues
	}
	return slices.DeleteFunc(slices.Clone(issues), func(i autoconfig.OperationIssue) bool {
		key := i.Path + ":" + i.Method
		entry := []v1alpha1.EndpointEntry{{Endpoint: i.Path, Method: i.Method}}
		kept := r.Filter.Apply(entry, map[string][]string{key: i.Tags},
			map[string]string{key: i.OperationID}, *ac.Spec.Filter)
		return len(kept) == 0
	})
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

// ownedEndpointPredicate gates the Owns(KrakenDEndpoint) watch. It passes
// spec changes (generation bumps), label changes (label drift an external
// actor made) and deletes, which the AutoConfig repairs, and changes to an
// endpoint's readiness, which it aggregates into EndpointsReady. Other
// status updates do not re-enqueue the owning AutoConfig.
func ownedEndpointPredicate() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.LabelChangedPredicate{},
		predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
			return endpointReadinessKey(e.ObjectOld) != endpointReadinessKey(e.ObjectNew)
		}},
	)
}

// endpointReadinessKey is what the Owns predicate compares to tell a
// readiness change: the observed generation and the Ready condition's
// status and reason. The condition's message is left out, so a message-only
// status write does not re-enqueue the AutoConfig.
func endpointReadinessKey(obj client.Object) string {
	ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return ""
	}
	key := strconv.FormatInt(ep.Status.ObservedGeneration, 10)
	if c := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionReady); c != nil {
		key += "/" + string(c.Status) + "/" + c.Reason
	}
	return key
}

// handleFetchError fails the sync on a spec fetch failure (including an
// external $ref that cannot be fetched): SpecAvailable False and, through
// handleSyncedFailure, Synced False, both with reason SpecFetchFailed, so the
// conditions agree with the phase, the synced gauge and the event.
func (r *KrakenDAutoConfigReconciler) handleFetchError(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	fetchErr error,
) (ctrl.Result, error) {
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSpecAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ac.Generation,
		Reason:             v1alpha1.ReasonSpecFetchFailed,
		Message:            fetchErr.Error(),
	})
	// The pipeline stopped before anything that buffers input warnings ran.
	return r.handleSyncedFailure(ctx, ac, v1alpha1.ReasonSpecFetchFailed, fetchErr, &inputWarnings{})
}

// handleSyncedFailure fails the sync with the given reason and error (see
// recordSyncedFailure) and returns the requeue/error semantics shared by the
// Synced-failure paths other than endpoint writes — requeue via interval for
// periodic triggers, otherwise the error itself so controller-runtime retries
// with exponential backoff.
func (r *KrakenDAutoConfigReconciler) handleSyncedFailure(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	reason string,
	syncErr error,
	warnings *inputWarnings,
) (ctrl.Result, error) {
	if err := r.recordSyncedFailure(ctx, ac, reason, syncErr.Error(), warnings); err != nil {
		return ctrl.Result{}, err
	}
	if ac.Spec.Trigger == v1alpha1.TriggerPeriodic {
		return r.requeueResult(ac), nil
	}
	return ctrl.Result{}, syncErr
}

// handleEndpointError fails the sync for endpointErr: with ValidatorUnavailable
// when the gateway config check could not run, otherwise with
// EndpointReconcileFailed, for a failed list or the endpointFailuresError of
// one pass, which can hold transient write, raced (beside a transient one),
// adoption and delete errors. A pass whose only failures are raced never gets
// here; it requeues quietly. The error is returned for every trigger, so
// controller-runtime retries it with backoff: these errors are transient, and
// a periodic trigger would otherwise wait a whole interval.
func (r *KrakenDAutoConfigReconciler) handleEndpointError(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	endpointErr error,
	warnings *inputWarnings,
) (ctrl.Result, error) {
	syncErr := fmt.Errorf("reconciling endpoints: %w", endpointErr)
	// The returned error carries every failure for the log; the status and
	// the event name the first few.
	message := syncErr.Error()
	if f, ok := endpointErr.(interface{ Summary() string }); ok {
		message = "reconciling endpoints: " + f.Summary()
	}
	if err := r.recordSyncedFailure(ctx, ac, endpointFailureReason(endpointErr), message, warnings); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, syncErr
}

// recordSyncedFailure records a failed sync: the synced gauge 0, the Synced
// condition False with the given reason and message (bounded by
// truncateMessage), the endpoint readiness as of now (refreshReadiness), and
// the Ready, phase and observedGeneration derived from them, then, once that
// status write succeeds, the buffered input warnings followed by a Warning
// event with the same message. It returns the status write's error, if any, other than a Conflict: a
// reconcile whose failure-status write conflicts read a stale copy of the
// AutoConfig, but its sync failed all the same, so it records no events and
// its caller returns the failure's own result. The quiet conflictRequeueDelay
// requeue would make controller-runtime forget the item and reset its
// exponential backoff, turning a persistent failure's retries into a
// conflictRequeueDelay loop while the cache stays stale.
func (r *KrakenDAutoConfigReconciler) recordSyncedFailure(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	reason string,
	syncMessage string,
	warnings *inputWarnings,
) error {
	// A failure can list every operation or write that failed, which would
	// overrun the condition's size limit: bound the status and event text.
	message := truncateMessage(syncMessage)
	meta.SetStatusCondition(&ac.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionSynced,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ac.Generation,
		Reason:             reason,
		Message:            message,
	})
	r.refreshReadiness(ctx, ac)
	setAutoConfigReadiness(ac)
	// The sync has failed whether or not its status write succeeds.
	autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name).Set(0)
	if err := r.Status().Update(ctx, ac); err != nil {
		if errors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating %s status: %w", reason, err)
	}
	warnings.emit(r.Recorder, ac)
	r.Recorder.Event(ac, "Warning", reason, message)
	return nil
}

// refreshReadiness sets ac's readyEndpoints and EndpointsReady condition from
// the endpoints it controls now, found by the controller UID index without
// adopting any, so a failed sync does not leave them as the last sync saw
// them. When the list fails they keep their last-known values: the failure
// status is written regardless.
func (r *KrakenDAutoConfigReconciler) refreshReadiness(ctx context.Context, ac *v1alpha1.KrakenDAutoConfig) {
	var owned v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &owned, client.InNamespace(ac.Namespace),
		client.MatchingFields{fieldindex.EndpointController: string(ac.UID)},
	); err != nil {
		logf.FromContext(ctx).V(1).Info("keeping the last-known endpoint readiness", "error", err.Error())
		return
	}
	readiness := summarizeReadiness(owned.Items, nil)
	ac.Status.ReadyEndpoints = readiness.ready
	meta.SetStatusCondition(&ac.Status.Conditions, endpointsReadyCondition(readiness, ac.Generation))
}

// statusWriteFailure returns the reconcile result for a failed AutoConfig
// status write that records no failure: a successful sync. A Conflict means
// this reconcile read a stale copy of the AutoConfig: it requeues quietly after
// conflictRequeueDelay, so the retry reads the current copy, instead of
// surfacing a reconciler error. Any other error is returned for
// controller-runtime to retry with backoff.
func statusWriteFailure(ctx context.Context, err error) (ctrl.Result, error) {
	if errors.IsConflict(err) {
		return lostWriteRace(ctx, err)
	}
	return ctrl.Result{}, err
}

// lostWriteRace returns the quiet requeue for a reconcile whose write lost a
// race with a newer copy of the object than it read, logging it only at V(1).
func lostWriteRace(ctx context.Context, err error) (ctrl.Result, error) {
	logf.FromContext(ctx).V(1).Info("write lost a race; requeueing", "error", err)
	return ctrl.Result{RequeueAfter: conflictRequeueDelay}, nil
}

func (r *KrakenDAutoConfigReconciler) handleCUEError(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	cueErr error,
	warnings *inputWarnings,
) (ctrl.Result, error) {
	return r.handleSyncedFailure(ctx, ac, v1alpha1.ReasonCUEEvaluationFailed, cueErr, warnings)
}

// applyAdditionalEndpoints builds, transforms, scopes, and merges additional
// endpoints into the filtered set, adding an AdditionalEndpointOverride
// warning to warnings for each spec-derived endpoint one replaces. It returns
// the combined slice and a note per replacement for status.warnings, or a
// non-nil error when no base path can be determined.
func applyAdditionalEndpoints(
	ac *v1alpha1.KrakenDAutoConfig,
	filtered []v1alpha1.EndpointEntry,
	warnings *inputWarnings,
) ([]v1alpha1.EndpointEntry, []string, error) {
	if len(ac.Spec.AdditionalEndpoints) == 0 {
		return filtered, nil, nil
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
			return nil, nil, fmt.Errorf(
				"cannot derive a base path for additionalEndpoints (generated " +
					"endpoints share no common parent); set " +
					"spec.additionalEndpointsBasePath or spec.urlTransform.addPathPrefix")
		}
	}
	if base != "" {
		autoconfig.ScopeAdditionalEntries(additional, base)
	}

	var replaced, notes []string
	filtered, replaced = autoconfig.MergeAdditional(filtered, additional)
	for _, key := range replaced {
		note := fmt.Sprintf("Additional endpoint %q overrides a spec-derived endpoint", key)
		warnings.add(v1alpha1.ReasonAdditionalEndpointOverride, note)
		notes = append(notes, note)
	}
	return filtered, notes, nil
}

// fetchSpec fetches the OpenAPI spec, within FetchTimeout, and prepares it for
// evaluation: it resolves external $refs (URL sources only), strips upstream
// server entries and dereferences parameter $refs. A failure to fetch or
// decode the spec or an external $ref document, a deadline that ends the
// fetch or the resolution, or a parameter expansion past the body size limit,
// is returned and fails the sync closed. notes are the spec problems that do
// not stop the sync (the $refs the resolver could not honour, the external
// $refs of a ConfigMap-sourced spec, which nothing can fetch, and the
// parameter $refs that do not resolve) for status.warnings. A StripServers or parameter decode failure is logged and
// leaves the data as it was.
func (r *KrakenDAutoConfigReconciler) fetchSpec(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
) (*autoconfig.FetchResult, []string, error) {
	log := logf.FromContext(ctx)
	timeout := r.FetchTimeout
	if timeout == 0 {
		timeout = defaultFetchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetchResult, err := r.Fetcher.Fetch(ctx, autoconfig.FetchSource{
		URL:               ac.Spec.OpenAPI.URL,
		ConfigMapRef:      ac.Spec.OpenAPI.ConfigMapRef,
		Auth:              ac.Spec.OpenAPI.Auth,
		AllowClusterLocal: ac.Spec.OpenAPI.AllowClusterLocal,
		Namespace:         ac.Namespace,
	})
	if err != nil {
		return nil, nil, err
	}

	var notes []string
	// Resolve external $refs (only possible with HTTP sources).
	if ac.Spec.OpenAPI.URL != "" {
		resolved, refNotes, resolveErr := autoconfig.ResolveExternalRefs(
			ctx, fetchResult.Data, ac.Spec.OpenAPI.URL, r.Fetcher,
			autoconfig.FetchSource{
				Auth:              ac.Spec.OpenAPI.Auth,
				AllowClusterLocal: ac.Spec.OpenAPI.AllowClusterLocal,
				Namespace:         ac.Namespace,
			},
		)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		fetchResult.Data = resolved
		notes = append(notes, refNotes...)
	} else if external, refErr := autoconfig.ExternalRefs(fetchResult.Data); refErr == nil {
		// Nothing can fetch these. A decode error is ignored: the same data
		// then fails CUE evaluation, which reports it.
		for _, ref := range external {
			notes = append(notes, fmt.Sprintf(
				"external $ref %q is not resolved: a ConfigMap-sourced spec cannot fetch other documents", ref))
		}
	}

	// Strip upstream `servers` entries: the KrakenD gateway is the
	// externally-visible server, so upstream URLs must not bleed into
	// generated documentation or endpoint configuration. A spec that cannot
	// be decoded fails evaluation later, so it is not processed further.
	stripped, stripErr := autoconfig.StripServers(fetchResult.Data)
	if stripErr != nil {
		log.Error(stripErr, "stripping upstream servers failed, using raw spec")
		return fetchResult, notes, nil
	}
	fetchResult.Data = stripped

	deref, paramNotes, derefErr := autoconfig.DereferenceParameters(fetchResult.Data)
	switch {
	case stderrors.Is(derefErr, autoconfig.ErrParameterRefsTooLarge):
		return nil, nil, derefErr
	case derefErr != nil:
		log.Error(derefErr, "dereferencing parameter $refs failed, using the spec as is")
	default:
		fetchResult.Data = deref
	}
	return fetchResult, append(notes, paramNotes...), nil
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

// inputWarnings collects the Warning events about one reconcile's inputs —
// DuplicateOperationId, AdditionalEndpointOverride and SpecWarning — and holds
// them until the reconcile's terminal status write succeeds. A reconcile that
// read a stale AutoConfig and then loses that write to a conflict records
// none of them; its retry records them if they still apply. Warnings are
// collected only when inputsChanged.
type inputWarnings struct {
	inputsChanged bool
	pending       []inputWarning
}

// inputWarning is one buffered Warning event.
type inputWarning struct {
	reason, message string
}

// add buffers a Warning event with the given reason and message, if the
// reconcile's inputs changed.
func (w *inputWarnings) add(reason, message string) {
	if w.inputsChanged {
		w.pending = append(w.pending, inputWarning{reason: reason, message: message})
	}
}

// emit records the first maxStatusListLen buffered events on ac, whatever
// their reasons. The recorder's per-object budget is shared by every Warning
// event, so the cap leaves room for the failure event of a failing pass.
func (w *inputWarnings) emit(recorder record.EventRecorder, ac *v1alpha1.KrakenDAutoConfig) {
	for _, ev := range capList(w.pending) {
		recorder.Event(ac, "Warning", ev.reason, ev.message)
	}
}

// endpointChanges counts the endpoint writes one reconcileEndpoints call issued.
type endpointChanges struct {
	created, updated, deleted int
}

func (c *endpointChanges) total() int {
	return c.created + c.updated + c.deleted
}

// count tallies one CreateOrUpdate result.
func (c *endpointChanges) count(op controllerutil.OperationResult) {
	switch op {
	case controllerutil.OperationResultCreated:
		c.created++
	case controllerutil.OperationResultUpdated:
		c.updated++
	case controllerutil.OperationResultNone,
		controllerutil.OperationResultUpdatedStatus,
		controllerutil.OperationResultUpdatedStatusOnly:
		// None: already in the desired state. The status results come
		// only from CreateOrPatch, never from CreateOrUpdate.
	}
}

// syncResult is what a pipeline pass that reached its endpoint writes
// produced, for recordSync.
type syncResult struct {
	// checksum is the pass's combined input checksum.
	checksum string
	// generated counts the endpoints the pass generated.
	generated int
	// skipped lists the operations the pass generated no endpoint for.
	skipped []v1alpha1.OperationStatus
	// failed lists the operations the pass could not converge.
	failed []v1alpha1.OperationStatus
	// warnings lists every distinct problem that does not stop a sync; the
	// status lists the first maxStatusListLen.
	warnings []string
	changes  endpointChanges
	// readiness summarizes the endpoints the AutoConfig controls afterwards.
	readiness endpointReadiness
}

// recordSync records a sync that reached its endpoint writes: the combined
// checksum, the endpoint counts and lists, and the Synced condition (True, or
// False with reason OperationsFailed while res.failed is not empty) with the
// Ready, phase and observedGeneration derived from it. The synced gauge
// follows Synced. LastSyncTime and the EndpointsGenerated event mark a sync
// that changed something: new inputs (a different combined checksum) or
// endpoint writes, so a steady-state reconcile leaves both alone. Status is
// written only when it differs from orig, the status read at the start of the
// reconcile, and an OperationsFailed Warning event is recorded only when the
// Synced condition or status.failedOperations changed (failureChanged). The
// buffered input warnings are recorded once that write succeeds,
// before EndpointsGenerated.
func (r *KrakenDAutoConfigReconciler) recordSync(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	orig *v1alpha1.KrakenDAutoConfigStatus,
	res syncResult,
	warnings *inputWarnings,
) error {
	changed := res.checksum != orig.SpecChecksum || res.changes.total() > 0
	ac.Status.SpecChecksum = res.checksum
	if changed {
		now := metav1.Now()
		ac.Status.LastSyncTime = &now
	}
	ac.Status.GeneratedEndpoints = res.generated
	ac.Status.ReadyEndpoints = res.readiness.ready
	ac.Status.SkippedOperations = len(res.skipped)
	ac.Status.Skipped = capList(res.skipped)
	ac.Status.FailedOperations = capList(res.failed)
	ac.Status.Warnings = capList(res.warnings)
	synced := syncedCondition(res, ac.Generation)
	meta.SetStatusCondition(&ac.Status.Conditions, synced)
	meta.SetStatusCondition(&ac.Status.Conditions, endpointsReadyCondition(res.readiness, ac.Generation))
	setAutoConfigReadiness(ac)
	statusChanged := autoConfigStatusChanged(orig, &ac.Status)
	if statusChanged {
		if err := r.Status().Update(ctx, ac); err != nil {
			return fmt.Errorf("updating final status: %w", err)
		}
	}
	gauge := 1.0
	if synced.Status != metav1.ConditionTrue {
		gauge = 0
	}
	autoConfigSynced.WithLabelValues(ac.Namespace, ac.Name).Set(gauge)

	warnings.emit(r.Recorder, ac)
	if synced.Status != metav1.ConditionTrue && failureChanged(orig, &ac.Status) {
		r.Recorder.Event(ac, "Warning", v1alpha1.ReasonOperationsFailed, synced.Message)
	}
	if changed {
		r.Recorder.Eventf(ac, "Normal", v1alpha1.ReasonEndpointsGenerated,
			"Generated %s (%d created, %d updated, %d deleted, %d skipped)",
			counted(res.generated, "endpoint"), res.changes.created, res.changes.updated, res.changes.deleted, len(res.skipped))
	}
	return nil
}

// failureChanged reports whether the Synced condition (status, reason or
// message) or status.failedOperations differ between orig and cur, so a held
// operation warns when its failure changes, not on every status change that
// goes with it, such as an endpoint turning ready.
func failureChanged(orig, cur *v1alpha1.KrakenDAutoConfigStatus) bool {
	if !slices.Equal(orig.FailedOperations, cur.FailedOperations) {
		return true
	}
	was := meta.FindStatusCondition(orig.Conditions, v1alpha1.ConditionSynced)
	now := meta.FindStatusCondition(cur.Conditions, v1alpha1.ConditionSynced)
	if was == nil || now == nil {
		return was != now
	}
	return was.Status != now.Status || was.Reason != now.Reason || was.Message != now.Message
}

// syncedCondition is the Synced condition for res: False with reason
// OperationsFailed, naming the first operations, while res.failed is not
// empty, otherwise True, counting what was skipped and warned about.
func syncedCondition(res syncResult, generation int64) metav1.Condition {
	if len(res.failed) > 0 {
		labels := make([]string, len(res.failed))
		for i, f := range res.failed {
			labels[i] = operationLabel(f)
		}
		return metav1.Condition{
			Type: v1alpha1.ConditionSynced, Status: metav1.ConditionFalse, ObservedGeneration: generation,
			Reason: v1alpha1.ReasonOperationsFailed,
			Message: fmt.Sprintf("%s failed; %s and no stale endpoint is deleted until %s "+
				"(see status.failedOperations): %s",
				counted(len(res.failed), "operation"),
				plural(len(res.failed), "it keeps its last-synced endpoint", "they keep their last-synced endpoints"),
				plural(len(res.failed), "it recovers", "they recover"), listed(labels)),
		}
	}
	c := metav1.Condition{
		Type: v1alpha1.ConditionSynced, Status: metav1.ConditionTrue, ObservedGeneration: generation, Reason: "Synced",
		Message: "Generated " + counted(res.generated, "endpoint"),
	}
	if n := len(res.skipped); n > 0 {
		c.Message += "; " + counted(n, "operation") + " skipped (see status.skipped)"
	}
	if n := len(res.warnings); n > 0 {
		c.Message += "; " + counted(n, "spec warning") + " (see status.warnings)"
	}
	return c
}

// autoConfigStatusChanged reports whether cur differs semantically from orig.
// Conditions are compared with conditionsEqual, which ignores
// LastTransitionTime.
func autoConfigStatusChanged(orig, cur *v1alpha1.KrakenDAutoConfigStatus) bool {
	return orig.Phase != cur.Phase ||
		orig.ObservedGeneration != cur.ObservedGeneration ||
		orig.SpecChecksum != cur.SpecChecksum ||
		orig.GeneratedEndpoints != cur.GeneratedEndpoints ||
		orig.ReadyEndpoints != cur.ReadyEndpoints ||
		orig.SkippedOperations != cur.SkippedOperations ||
		!slices.Equal(orig.Skipped, cur.Skipped) ||
		!slices.Equal(orig.FailedOperations, cur.FailedOperations) ||
		!slices.Equal(orig.Warnings, cur.Warnings) ||
		!orig.LastSyncTime.Equal(cur.LastSyncTime) ||
		!conditionsEqual(orig.Conditions, cur.Conditions)
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

// autoConfigReady derives an AutoConfig's Ready condition from its
// SpecAvailable, Synced and EndpointsReady conditions: False with the first
// failing condition's reason, Unknown before the first sync, True once all
// three are True.
func autoConfigReady(conds []metav1.Condition) (status metav1.ConditionStatus, reason, message string) {
	for _, typ := range []string{
		v1alpha1.ConditionSpecAvailable, v1alpha1.ConditionSynced, v1alpha1.ConditionEndpointsReady,
	} {
		c := meta.FindStatusCondition(conds, typ)
		switch {
		case c == nil:
			return metav1.ConditionUnknown, v1alpha1.ReasonPending, "Waiting for the first sync"
		case c.Status != metav1.ConditionTrue:
			return metav1.ConditionFalse, c.Reason, c.Message
		}
	}
	return metav1.ConditionTrue, v1alpha1.ReasonReady, "OpenAPI spec fetched, endpoints in sync and ready"
}

// autoConfigPhase derives the compatibility phase from the Synced condition.
func autoConfigPhase(conds []metav1.Condition) v1alpha1.AutoConfigPhase {
	synced := meta.FindStatusCondition(conds, v1alpha1.ConditionSynced)
	switch {
	case synced == nil:
		return v1alpha1.AutoConfigPhasePending
	case synced.Status == metav1.ConditionTrue:
		return v1alpha1.AutoConfigPhaseSynced
	default:
		return v1alpha1.AutoConfigPhaseError
	}
}

// setAutoConfigReadiness writes the derived Ready condition, phase and
// observedGeneration into ac's in-memory status. Call it immediately before
// every AutoConfig status write.
func setAutoConfigReadiness(ac *v1alpha1.KrakenDAutoConfig) {
	status, reason, message := autoConfigReady(ac.Status.Conditions)
	setReadyCondition(&ac.Status.Conditions, ac.Generation, status, reason, message)
	ac.Status.Phase = autoConfigPhase(ac.Status.Conditions)
	ac.Status.ObservedGeneration = ac.Generation
}

// newAutoConfigRateLimiter is controller-runtime's default rate limiter with
// the per-item backoff capped at defaultResyncInterval instead of 1000s. An
// AutoConfig whose reconcile returns an error (every OnChange failure, and
// EndpointReconcileFailed for either trigger) therefore retries at least as
// often as a healthy OnChange one resyncs, including once a missing gateway,
// policy or auth Secret appears, or an unreachable spec source recovers.
// A Periodic AutoConfig's other failures requeue at spec.periodic.interval.
func newAutoConfigRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return cappedRateLimiter(defaultResyncInterval)
}

// loadAllCUEDefinitions loads the default CUE definitions (the ConfigMap,
// falling back to the embedded ones) and the AutoConfig's custom ones.
func (r *KrakenDAutoConfigReconciler) loadAllCUEDefinitions(
	ctx context.Context, ac *v1alpha1.KrakenDAutoConfig,
) (defaultDefs, customDefs map[string]string, err error) {
	defaultDefs, err = r.loadCUEDefinitions(ctx, ac.Namespace, defaultCUEDefinitionsConfigMap)
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("loading default CUE definitions: %w", err)
		}
		defaultDefs, err = autoconfig.EmbeddedCUEDefinitions()
		if err != nil {
			return nil, nil, fmt.Errorf("loading embedded CUE definitions: %w", err)
		}
	}
	if ac.Spec.CUE != nil && ac.Spec.CUE.DefinitionsConfigMapRef != nil {
		customDefs, err = r.loadCUEDefinitions(ctx, ac.Namespace, ac.Spec.CUE.DefinitionsConfigMapRef.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("loading custom CUE definitions: %w", err)
		}
	}
	return defaultDefs, customDefs, nil
}

// overrideFailure reports the sync failure for an override that matched no
// generated entry, whose backend index is out of range, or whose operationId
// several operations share. Each must fail closed rather than be silently
// dropped or land on only one operation: overrides can carry security-relevant
// config (e.g. auth/validator). It returns a nil error when every override
// resolved.
func overrideFailure(out *autoconfig.CUEOutput) (string, error) {
	if len(out.UnmatchedOverrides) > 0 {
		return v1alpha1.ReasonUnmatchedOverride, fmt.Errorf(
			"spec.overrides reference operationIds or backend indexes not present in the OpenAPI spec: %s",
			listed(out.UnmatchedOverrides))
	}
	if len(out.AmbiguousOverrides) > 0 {
		return v1alpha1.ReasonAmbiguousOverride, fmt.Errorf(
			"spec.overrides reference operationIds that more than one operation declares: %s",
			listed(out.AmbiguousOverrides))
	}
	return "", nil
}

// cueEnvironment is the CUE environment the AutoConfig selects, "" when unset.
func cueEnvironment(ac *v1alpha1.KrakenDAutoConfig) string {
	if ac.Spec.CUE == nil {
		return ""
	}
	return ac.Spec.CUE.Environment
}

// heldLog is what logHeldCauses last logged for one AutoConfig.
type heldLog struct {
	owner  types.NamespacedName
	digest string
}

// forgetHeldCauses forgets what was logged for the AutoConfig named owner,
// which no longer exists: its UID is gone with it, so it is found by name.
func (r *KrakenDAutoConfigReconciler) forgetHeldCauses(owner types.NamespacedName) {
	r.heldLogged.Range(func(uid, entry any) bool {
		if entry.(heldLog).owner == owner {
			r.heldLogged.Delete(uid)
		}
		return true
	})
}

// heldCause is the full cause of one held operation, for the log.
type heldCause struct {
	// operation names the operation: an endpoint, or "METHOD path (operationId)"
	// when it has none.
	operation, cause string
}

// heldCauses lists the full cause of every operation held this pass, sorted:
// the ones that failed CUE evaluation, then the endpoints the config check or
// the API server rejected.
func heldCauses(failedOps []autoconfig.OperationIssue, rejected map[string]rejection) []heldCause {
	var held []heldCause
	for _, op := range failedOps {
		held = append(held, heldCause{
			operation: op.Method + " " + op.Path + " (" + op.OperationID + ")", cause: op.Message,
		})
	}
	for name, rej := range rejected {
		held = append(held, heldCause{operation: name, cause: rej.cause.Error()})
	}
	slices.SortFunc(held, func(a, b heldCause) int {
		return cmp.Or(strings.Compare(a.operation, b.operation), strings.Compare(a.cause, b.cause))
	})
	return held
}

// logHeldCauses logs the full cause of every operation ac holds. The status
// keeps a cut message, so the log is where the whole text lives. It logs when
// the causes differ from what this process last logged for ac, so a restart
// logs them once more, and forgets ac once nothing is held.
func (r *KrakenDAutoConfigReconciler) logHeldCauses(
	ctx context.Context,
	ac *v1alpha1.KrakenDAutoConfig,
	failedOps []autoconfig.OperationIssue,
	rejected map[string]rejection,
) {
	held := heldCauses(failedOps, rejected)
	if len(held) == 0 {
		r.heldLogged.Delete(ac.UID)
		return
	}
	hash := sha256.New()
	for _, h := range held {
		fmt.Fprintf(hash, "%s\x00%s\x00", h.operation, h.cause)
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	if prev, ok := r.heldLogged.Load(ac.UID); ok && prev.(heldLog).digest == digest {
		return
	}
	r.heldLogged.Store(ac.UID, heldLog{owner: client.ObjectKeyFromObject(ac), digest: digest})
	log := logf.FromContext(ctx)
	for _, h := range held {
		log.Info("operation held", "operation", h.operation, "error", h.cause)
	}
}
