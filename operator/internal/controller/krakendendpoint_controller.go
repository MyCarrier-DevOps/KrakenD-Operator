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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// KrakenDEndpointReconciler reconciles a KrakenDEndpoint object.
// It validates gateway and policy references, maintaining endpoint
// status. Config rendering is handled by the gateway controller.
type KrakenDEndpointReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendendpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendgateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.krakend.io,resources=krakendbackendpolicies,verbs=get;list;watch

// Reconcile validates the gateway and policy references for a
// KrakenDEndpoint and updates its status accordingly.
func (r *KrakenDEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ep v1alpha1.KrakenDEndpoint
	if err := r.Get(ctx, req.NamespacedName, &ep); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting endpoint %s: %w", req.NamespacedName, err)
	}

	// Capture original status for change detection
	origPhase := ep.Status.Phase
	origGeneration := ep.Status.ObservedGeneration
	origCount := ep.Status.EndpointCount
	origMethods := ep.Status.Methods
	origConditions := ep.Status.DeepCopy().Conditions

	// Initialize phase on first reconcile
	if ep.Status.Phase == "" {
		ep.Status.Phase = v1alpha1.EndpointPhasePending
	}

	// Validate gateway reference exists
	var gw v1alpha1.KrakenDGateway
	gwKey := types.NamespacedName{
		Name:      ep.Spec.GatewayRef.Name,
		Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace),
	}
	if err := r.Get(ctx, gwKey, &gw); err != nil {
		if errors.IsNotFound(err) {
			return r.setDetached(ctx, &ep, "GatewayNotFound",
				fmt.Sprintf("gateway %s/%s not found", gwKey.Namespace, gwKey.Name))
		}
		return ctrl.Result{}, fmt.Errorf("getting gateway %s: %w", gwKey, err)
	}

	// Validate all policy references exist (deduplicated)
	policyKeys := make(map[string]types.NamespacedName)
	for _, entry := range ep.Spec.Endpoints {
		for _, be := range entry.Backends {
			if be.PolicyRef != nil {
				mapKey := be.PolicyRef.PolicyKey(ep.Namespace)
				if _, ok := policyKeys[mapKey]; !ok {
					policyKeys[mapKey] = types.NamespacedName{
						Name:      be.PolicyRef.Name,
						Namespace: be.PolicyRef.ResolvedNamespace(ep.Namespace),
					}
				}
			}
		}
	}
	for _, policyKey := range policyKeys {
		var policy v1alpha1.KrakenDBackendPolicy
		if err := r.Get(ctx, policyKey, &policy); err != nil {
			if errors.IsNotFound(err) {
				return r.setInvalid(ctx, &ep, "PolicyNotFound",
					fmt.Sprintf("policy %q not found in namespace %q", policyKey.Name, policyKey.Namespace))
			}
			return ctrl.Result{}, fmt.Errorf("getting policy %s: %w", policyKey, err)
		}
	}

	// All references valid — set Active
	ep.Status.Phase = v1alpha1.EndpointPhaseActive
	ep.Status.ObservedGeneration = ep.Generation
	ep.Status.EndpointCount = int32(len(ep.Spec.Endpoints))
	ep.Status.Methods = distinctMethods(ep.Spec.Endpoints)
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionAvailable,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ep.Generation,
		Reason:             "ReferencesValid",
		Message:            "All gateway and policy references are valid",
	})

	// Only write status if it actually changed
	statusChanged := ep.Status.Phase != origPhase ||
		ep.Status.ObservedGeneration != origGeneration ||
		ep.Status.EndpointCount != origCount ||
		ep.Status.Methods != origMethods ||
		!conditionsEqual(origConditions, ep.Status.Conditions)
	if statusChanged {
		if err := r.Status().Update(ctx, &ep); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating endpoint status to Active: %w", err)
		}
	}

	log.V(1).Info("endpoint reconciled", "phase", ep.Status.Phase, "endpoints", ep.Status.EndpointCount)
	return ctrl.Result{}, nil
}

// Field index keys for efficient watch-to-reconcile mapping.
const (
	// EndpointGatewayIndex is the field index key for looking up endpoints
	// by their gateway reference. Exported for use by the webhook package.
	EndpointGatewayIndex = ".spec.gatewayRef.namespacedName"

	// EndpointPolicyIndex is the field index key for looking up endpoints
	// by their policy references. Exported for use by the webhook package.
	EndpointPolicyIndex = ".spec.endpoints.backends.policyRef.namespacedName"
)

// SetupWithManager sets up the controller with the Manager.
func (r *KrakenDEndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := EnsureEndpointIndexes(mgr); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KrakenDEndpoint{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&v1alpha1.KrakenDGateway{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayToEndpoints),
		).
		Watches(
			&v1alpha1.KrakenDBackendPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.policyToEndpoints),
		).
		Named("krakendendpoint").
		Complete(r)
}

func (r *KrakenDEndpointReconciler) setDetached(
	ctx context.Context, ep *v1alpha1.KrakenDEndpoint, reason, message string,
) (ctrl.Result, error) {
	return r.setErrorPhase(ctx, ep, v1alpha1.EndpointPhaseDetached, reason, message)
}

func (r *KrakenDEndpointReconciler) setInvalid(
	ctx context.Context, ep *v1alpha1.KrakenDEndpoint, reason, message string,
) (ctrl.Result, error) {
	return r.setErrorPhase(ctx, ep, v1alpha1.EndpointPhaseInvalid, reason, message)
}

// setErrorPhase sets the endpoint to the given error phase with change detection.
func (r *KrakenDEndpointReconciler) setErrorPhase(
	ctx context.Context,
	ep *v1alpha1.KrakenDEndpoint,
	phase v1alpha1.EndpointPhase,
	reason, message string,
) (ctrl.Result, error) {
	origPhase := ep.Status.Phase
	origCount := ep.Status.EndpointCount
	origMethods := ep.Status.Methods
	origConditions := ep.Status.DeepCopy().Conditions

	ep.Status.Phase = phase
	ep.Status.ObservedGeneration = ep.Generation
	ep.Status.EndpointCount = int32(len(ep.Spec.Endpoints))
	ep.Status.Methods = distinctMethods(ep.Spec.Endpoints)
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ep.Generation,
		Reason:             reason,
		Message:            message,
	})

	if origPhase != phase {
		r.Recorder.Event(ep, "Warning", reason, message)
	}
	changed := ep.Status.Phase != origPhase ||
		ep.Status.EndpointCount != origCount ||
		ep.Status.Methods != origMethods ||
		!conditionsEqual(origConditions, ep.Status.Conditions)
	if changed {
		if err := r.Status().Update(ctx, ep); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating endpoint status to %s: %w", phase, err)
		}
	}
	return ctrl.Result{}, nil
}

// gatewayToEndpoints maps a Gateway event to endpoints that reference it via field index.
func (r *KrakenDEndpointReconciler) gatewayToEndpoints(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	log := logf.FromContext(ctx)
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &endpoints,
		client.MatchingFields{EndpointGatewayIndex: obj.GetNamespace() + "/" + obj.GetName()},
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
		client.MatchingFields{EndpointPolicyIndex: indexKey},
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
