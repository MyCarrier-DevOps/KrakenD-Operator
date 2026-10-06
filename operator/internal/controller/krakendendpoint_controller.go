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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
)

// KrakenDEndpointReconciler reconciles a KrakenDEndpoint object.
// It resolves gateway and policy references into ResolvedRefs, and derives
// Ready and phase. Config rendering, and the Accepted condition, belong to
// the gateway controller.
type KrakenDEndpointReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints/status,verbs=patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile resolves the gateway and policy references of a KrakenDEndpoint
// into its ResolvedRefs condition, and derives Ready and phase from
// ResolvedRefs and the gateway-owned Accepted condition. It patches status
// with an optimistic lock, only when the status changed, so it never
// overwrites the gateway's Accepted condition. The legacy Available condition
// is dropped on the first reconcile.
func (r *KrakenDEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ep v1alpha1.KrakenDEndpoint
	if err := r.Get(ctx, req.NamespacedName, &ep); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting endpoint %s: %w", req.NamespacedName, err)
	}
	base := ep.DeepCopy()

	resolved, err := r.resolveRefs(ctx, &ep)
	if err != nil {
		return ctrl.Result{}, err
	}
	applyEndpointStatus(&ep, resolved)
	if endpointStatusEqual(&base.Status, &ep.Status) {
		log.V(1).Info("endpoint status unchanged", "phase", ep.Status.Phase)
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, &ep,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if errors.IsConflict(err) {
			return lostWriteRace(ctx, err)
		}
		return ctrl.Result{}, fmt.Errorf("patching endpoint status: %w", err)
	}

	recordConditionTransition(r.Recorder, &ep,
		meta.FindStatusCondition(base.Status.Conditions, v1alpha1.ConditionResolvedRefs), resolved)

	log.V(1).Info("endpoint reconciled", "phase", ep.Status.Phase, "endpoints", ep.Status.EndpointCount)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KrakenDEndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := fieldindex.EnsureEndpointIndexes(mgr); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KrakenDEndpoint{}, builder.WithPredicates(endpointPredicate())).
		Watches(
			&v1alpha1.KrakenDGateway{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayToEndpoints),
			builder.WithPredicates(existencePredicate()),
		).
		Watches(
			&v1alpha1.KrakenDBackendPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToEndpoints),
			builder.WithPredicates(existencePredicate()),
		).
		Named("krakendendpoint").
		Complete(r)
}

// resolveRefs returns the ResolvedRefs condition for ep. It is False when its
// gateway or any policy it references does not exist. A lookup error other
// than NotFound is returned, so the reconcile is retried.
func (r *KrakenDEndpointReconciler) resolveRefs(
	ctx context.Context, ep *v1alpha1.KrakenDEndpoint,
) (metav1.Condition, error) {
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionResolvedRefs,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ep.Generation,
		Reason:             v1alpha1.ReasonRefsResolved,
		Message:            "Gateway and all policy references resolved",
	}
	gwKey := types.NamespacedName{
		Name:      ep.Spec.GatewayRef.Name,
		Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace),
	}
	var gw v1alpha1.KrakenDGateway
	if err := r.Get(ctx, gwKey, &gw); err != nil {
		if !errors.IsNotFound(err) {
			return cond, fmt.Errorf("getting gateway %s: %w", gwKey, err)
		}
		cond.Status, cond.Reason = metav1.ConditionFalse, v1alpha1.ReasonGatewayNotFound
		cond.Message = fmt.Sprintf("gateway %s/%s not found", gwKey.Namespace, gwKey.Name)
		return cond, nil
	}
	// policyRefsFromEndpoint lists each referenced policy once, in spec
	// order, so the first missing policy (and the message) is stable.
	for _, ref := range policyRefsFromEndpoint(ep) {
		var policy v1alpha1.KrakenDBackendPolicy
		if err := r.Get(ctx, ref.NamespacedName, &policy); err != nil {
			if !errors.IsNotFound(err) {
				return cond, fmt.Errorf("getting policy %s: %w", ref.NamespacedName, err)
			}
			cond.Status, cond.Reason = metav1.ConditionFalse, v1alpha1.ReasonPolicyNotFound
			cond.Message = fmt.Sprintf("policy %q not found in namespace %q", ref.Name, ref.Namespace)
			return cond, nil
		}
	}
	return cond, nil
}

// endpointPredicate gates the primary KrakenDEndpoint watch. It passes spec
// changes (generation bumps) and changes to the gateway-owned Accepted
// condition, from which Ready and phase are derived. The controller's own
// status writes change neither, so they do not enqueue the endpoint again.
func endpointPredicate() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.Funcs{UpdateFunc: acceptedChanged},
	)
}

// acceptedChanged reports whether an update changed the endpoint's Accepted
// condition, ignoring its lastTransitionTime.
func acceptedChanged(e event.UpdateEvent) bool {
	oldEp, okOld := e.ObjectOld.(*v1alpha1.KrakenDEndpoint)
	newEp, okNew := e.ObjectNew.(*v1alpha1.KrakenDEndpoint)
	if !okOld || !okNew {
		return false
	}
	return !sameCondition(
		meta.FindStatusCondition(oldEp.Status.Conditions, v1alpha1.ConditionAccepted),
		meta.FindStatusCondition(newEp.Status.Conditions, v1alpha1.ConditionAccepted),
	)
}

// gatewayToEndpoints maps a Gateway event to endpoints that reference it via field index.
func (r *KrakenDEndpointReconciler) gatewayToEndpoints(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	log := logf.FromContext(ctx)
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &endpoints,
		client.MatchingFields{fieldindex.EndpointGateway: obj.GetNamespace() + "/" + obj.GetName()},
	); err != nil {
		log.Error(err, "failed to list endpoints for gateway mapping", "gateway", obj.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(endpoints.Items))
	for i := range endpoints.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      endpoints.Items[i].Name,
				Namespace: endpoints.Items[i].Namespace,
			},
		})
	}
	return requests
}

// policyToEndpoints maps a BackendPolicy event to endpoints that reference it via field index.
func (r *KrakenDEndpointReconciler) policyToEndpoints(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	log := logf.FromContext(ctx)
	indexKey := obj.GetNamespace() + "/" + obj.GetName()
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &endpoints,
		client.MatchingFields{fieldindex.EndpointPolicy: indexKey},
	); err != nil {
		log.Error(err, "failed to list endpoints for policy mapping", "policy", obj.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(endpoints.Items))
	for i := range endpoints.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      endpoints.Items[i].Name,
				Namespace: endpoints.Items[i].Namespace,
			},
		})
	}
	return requests
}

// distinctMethods returns a sorted, comma-separated string of unique HTTP
// methods across all endpoint entries (e.g. "DELETE,GET,POST").
func distinctMethods(entries []v1alpha1.EndpointEntry) string {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		seen[e.Method] = struct{}{}
	}
	methods := make([]string, 0, len(seen))
	for m := range seen {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	return strings.Join(methods, ",")
}

// applyEndpointStatus sets resolved on ep, drops the legacy Available
// condition, and derives Ready and phase from ResolvedRefs and the
// gateway-owned Accepted condition, which it leaves untouched.
func applyEndpointStatus(ep *v1alpha1.KrakenDEndpoint, resolved metav1.Condition) {
	meta.SetStatusCondition(&ep.Status.Conditions, resolved)
	meta.RemoveStatusCondition(&ep.Status.Conditions, v1alpha1.ConditionAvailable)
	status, reason, message := v1alpha1.EndpointReady(ep.Status.Conditions)
	setReadyCondition(&ep.Status.Conditions, ep.Generation, status, reason, message)
	ep.Status.Phase = v1alpha1.EndpointPhaseFromReady(status, reason)
	ep.Status.ObservedGeneration = ep.Generation
	ep.Status.EndpointCount = int32(len(ep.Spec.Endpoints))
	ep.Status.Methods = distinctMethods(ep.Spec.Endpoints)
}

// endpointStatusEqual reports whether a and b have the same content.
// Conditions are compared with conditionsEqual, which ignores
// LastTransitionTime.
func endpointStatusEqual(a, b *v1alpha1.KrakenDEndpointStatus) bool {
	return a.Phase == b.Phase &&
		a.ObservedGeneration == b.ObservedGeneration &&
		a.EndpointCount == b.EndpointCount &&
		a.Methods == b.Methods &&
		conditionsEqual(a.Conditions, b.Conditions)
}
