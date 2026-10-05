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
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
)

// KrakenDBackendPolicyReconciler reconciles a KrakenDBackendPolicy object.
// It maintains referencedBy and the Ready condition.
type KrakenDBackendPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// APIReader reads uncached; the finalizer is released only after it
	// confirms that no endpoint references the policy.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendbackendpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendbackendpolicies/finalizers,verbs=update

// Reconcile counts endpoint references and sets the Ready condition.
func (r *KrakenDBackendPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy v1alpha1.KrakenDBackendPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting policy %s: %w", req.NamespacedName, err)
	}

	// Capture original status for change detection
	origRef := policy.Status.ReferencedBy
	origGeneration := policy.Status.ObservedGeneration
	origConditions := policy.Status.DeepCopy().Conditions

	// Count how many endpoints reference this policy using the field index
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &endpoints,
		client.MatchingFields{fieldindex.EndpointPolicy: policy.Namespace + "/" + policy.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing endpoints: %w", err)
	}

	refCount := len(endpoints.Items)

	if done, err := r.reconcileProtection(ctx, &policy, endpoints.Items); done || err != nil {
		return ctrl.Result{}, err
	}

	policy.Status.ReferencedBy = refCount

	// Ready summarizes the policy's own fields; it replaces PolicyValid.
	prevReady := meta.FindStatusCondition(origConditions, v1alpha1.ConditionReady)
	ready := policyReadyCondition(&policy)
	meta.SetStatusCondition(&policy.Status.Conditions, ready)
	meta.RemoveStatusCondition(&policy.Status.Conditions, legacyConditionPolicyValid)
	policy.Status.ObservedGeneration = policy.Generation

	// Only write status if it actually changed
	if policy.Status.ReferencedBy != origRef ||
		policy.Status.ObservedGeneration != origGeneration ||
		!conditionsEqual(origConditions, policy.Status.Conditions) {
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating policy status: %w", err)
		}
		recordConditionTransition(r.Recorder, &policy, prevReady, ready)
	}

	log.V(1).Info("policy reconciled", "referencedBy", refCount)
	return ctrl.Result{}, nil
}

// maxNamedReferrers bounds the endpoints named in the deletion-blocked event.
const maxNamedReferrers = 5

// reconcileProtection keeps the protection finalizer on a policy that is not
// being deleted, and releases it from one that is once nothing references it.
// done reports that the policy is gone, so the caller has nothing left to write.
func (r *KrakenDBackendPolicyReconciler) reconcileProtection(
	ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy, referrers []v1alpha1.KrakenDEndpoint,
) (done bool, err error) {
	if policy.DeletionTimestamp.IsZero() {
		if controllerutil.AddFinalizer(policy, v1alpha1.PolicyProtectionFinalizer) {
			if err := r.Update(ctx, policy); err != nil {
				return false, fmt.Errorf("adding policy-protection finalizer: %w", err)
			}
		}
		return false, nil
	}
	if len(referrers) > 0 {
		r.Recorder.Event(policy, corev1.EventTypeWarning, v1alpha1.ReasonPolicyDeletionBlocked,
			"policy is being deleted but is still referenced by "+namedReferrers(referrers)+
				"; deletion completes when the last reference is removed")
		return false, nil
	}
	// The cache can lag a reference created a moment ago, so confirm on the
	// API server before the policy goes.
	referenced, err := r.referencedOnServer(ctx, policy)
	if err != nil || referenced {
		return false, err
	}
	if controllerutil.RemoveFinalizer(policy, v1alpha1.PolicyProtectionFinalizer) {
		if err := r.Update(ctx, policy); err != nil {
			return false, fmt.Errorf("removing policy-protection finalizer: %w", err)
		}
	}
	return true, nil
}

// namedReferrers lists up to maxNamedReferrers endpoints as "namespace/name".
func namedReferrers(referrers []v1alpha1.KrakenDEndpoint) string {
	names := make([]string, 0, len(referrers))
	for i := range referrers {
		names = append(names, referrers[i].Namespace+"/"+referrers[i].Name)
	}
	slices.Sort(names)
	if len(names) <= maxNamedReferrers {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxNamedReferrers], ", "), len(names)-maxNamedReferrers)
}

// referencedOnServer reports whether any endpoint references policy, reading
// uncached. The field index exists only in the cache, so it lists every
// endpoint, which is affordable on the one reconcile that ends a deletion.
func (r *KrakenDBackendPolicyReconciler) referencedOnServer(
	ctx context.Context, policy *v1alpha1.KrakenDBackendPolicy,
) (bool, error) {
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.APIReader.List(ctx, &endpoints); err != nil {
		return false, fmt.Errorf("listing endpoints uncached: %w", err)
	}
	key := policy.Namespace + "/" + policy.Name
	for i := range endpoints.Items {
		if slices.Contains(fieldindex.EndpointPolicyKeys(&endpoints.Items[i]), key) {
			return true, nil
		}
	}
	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KrakenDBackendPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := fieldindex.EnsureEndpointIndexes(mgr); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KrakenDBackendPolicy{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&v1alpha1.KrakenDEndpoint{},
			r.endpointPolicyHandler(),
			builder.WithPredicates(policyEndpointPredicate()),
		).
		Named("krakendbackendpolicy").
		Complete(r)
}

// endpointPolicyHandler returns an EventHandler that enqueues policies
// referenced by endpoints. On updates it enqueues the union of old and
// new policy refs so that removed references get their count decremented.
func (r *KrakenDBackendPolicyReconciler) endpointPolicyHandler() handler.EventHandler {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			for _, req := range policyRefsFromEndpoint(e.Object) {
				q.Add(req)
			}
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			// Enqueue union of old and new refs so removed policyRefs
			// get their referencedBy count updated.
			seen := map[types.NamespacedName]struct{}{}
			for _, req := range policyRefsFromEndpoint(e.ObjectOld) {
				if _, ok := seen[req.NamespacedName]; !ok {
					seen[req.NamespacedName] = struct{}{}
					q.Add(req)
				}
			}
			for _, req := range policyRefsFromEndpoint(e.ObjectNew) {
				if _, ok := seen[req.NamespacedName]; !ok {
					seen[req.NamespacedName] = struct{}{}
					q.Add(req)
				}
			}
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			for _, req := range policyRefsFromEndpoint(e.Object) {
				q.Add(req)
			}
		},
	}
}

// legacyConditionPolicyValid is the condition Ready replaced; Reconcile
// removes it from policies written by earlier versions.
const legacyConditionPolicyValid = "PolicyValid"

// policyReadyCondition returns the policy's Ready condition: False with the
// validatePolicy reason when a field is out of range, True otherwise.
func policyReadyCondition(policy *v1alpha1.KrakenDBackendPolicy) metav1.Condition {
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: policy.Generation,
		Reason:             v1alpha1.ReasonReady,
		Message:            "Policy configuration is valid",
	}
	if reason, msg := validatePolicy(policy); reason != "" {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, reason, msg
	}
	return cond
}

// validatePolicy checks policy fields for validity. Returns (reason, message)
// if invalid, or ("", "") if valid.
func validatePolicy(policy *v1alpha1.KrakenDBackendPolicy) (reason, message string) {
	if cb := policy.Spec.CircuitBreaker; cb != nil {
		if cb.MaxErrors <= 0 {
			return "InvalidCircuitBreaker", "circuitBreaker.maxErrors must be positive"
		}
		if cb.Interval <= 0 {
			return "InvalidCircuitBreaker", "circuitBreaker.interval must be positive"
		}
		if cb.Timeout <= 0 {
			return "InvalidCircuitBreaker", "circuitBreaker.timeout must be positive"
		}
	}
	if rl := policy.Spec.RateLimit; rl != nil {
		if rl.MaxRate <= 0 {
			return "InvalidRateLimit", "rateLimit.maxRate must be positive"
		}
	}
	return "", ""
}

// policyRefsFromEndpoint extracts deduplicated reconcile requests for all
// policies referenced by the given endpoint object.
func policyRefsFromEndpoint(obj client.Object) []reconcile.Request {
	ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil
	}
	seen := map[string]struct{}{}
	var requests []reconcile.Request
	for _, entry := range ep.Spec.Endpoints {
		for _, be := range entry.Backends {
			if be.PolicyRef == nil {
				continue
			}
			key := be.PolicyRef.PolicyKey(ep.Namespace)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      be.PolicyRef.Name,
					Namespace: be.PolicyRef.ResolvedNamespace(ep.Namespace),
				},
			})
		}
	}
	return requests
}

// policyEndpointPredicate gates the KrakenDEndpoint watch: referencedBy
// depends only on endpoint specs, so status-only endpoint updates are
// dropped. Creates and deletes always pass.
func policyEndpointPredicate() predicate.Predicate {
	return predicate.GenerationChangedPredicate{}
}
