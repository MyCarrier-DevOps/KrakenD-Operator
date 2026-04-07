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
	origConditions := ep.Status.DeepCopy().Conditions

	// Initialize phase on first reconcile
	if ep.Status.Phase == "" {
		ep.Status.Phase = v1alpha1.EndpointPhasePending
	}

	// Validate gateway reference exists
	var gw v1alpha1.KrakenDGateway
	gwKey := types.NamespacedName{Name: ep.Spec.GatewayRef.Name, Namespace: ep.Namespace}
	if err := r.Get(ctx, gwKey, &gw); err != nil {
		if errors.IsNotFound(err) {
			return r.setDetached(ctx, &ep, "GatewayNotFound",
				fmt.Sprintf("gateway %q not found", ep.Spec.GatewayRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("getting gateway %s: %w", gwKey, err)
	}

	// Validate all policy references exist (deduplicated)
	policyNames := make(map[string]struct{})
	for _, entry := range ep.Spec.Endpoints {
		for _, be := range entry.Backends {
			if be.PolicyRef != nil {
				policyNames[be.PolicyRef.Name] = struct{}{}
			}
		}
	}
	for policyName := range policyNames {
		var policy v1alpha1.KrakenDBackendPolicy
		policyKey := types.NamespacedName{Name: policyName, Namespace: ep.Namespace}
		if err := r.Get(ctx, policyKey, &policy); err != nil {
			if errors.IsNotFound(err) {
				return r.setInvalid(ctx, &ep, "PolicyNotFound",
					fmt.Sprintf("policy %q referenced by a backend not found", policyName))
			}
			return ctrl.Result{}, fmt.Errorf("getting policy %s: %w", policyKey, err)
		}
	}

	// All references valid — set Active
	ep.Status.Phase = v1alpha1.EndpointPhaseActive
	ep.Status.ObservedGeneration = ep.Generation
	ep.Status.EndpointCount = int32(len(ep.Spec.Endpoints))
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
	endpointGatewayIndex = ".spec.gatewayRef.name"
	endpointPolicyIndex  = ".spec.endpoints.backends.policyRef.name"
)

// SetupWithManager sets up the controller with the Manager.
func (r *KrakenDEndpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ensureEndpointIndexes(mgr); err != nil {
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
	ep.Status.Phase = v1alpha1.EndpointPhaseDetached
	ep.Status.ObservedGeneration = ep.Generation
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ep.Generation,
		Reason:             reason,
		Message:            message,
	})
	r.Recorder.Event(ep, "Warning", reason, message)
	if err := r.Status().Update(ctx, ep); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating endpoint status to Detached: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *KrakenDEndpointReconciler) setInvalid(
	ctx context.Context, ep *v1alpha1.KrakenDEndpoint, reason, message string,
) (ctrl.Result, error) {
	ep.Status.Phase = v1alpha1.EndpointPhaseInvalid
	ep.Status.ObservedGeneration = ep.Generation
	meta.SetStatusCondition(&ep.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ep.Generation,
		Reason:             reason,
		Message:            message,
	})
	r.Recorder.Event(ep, "Warning", reason, message)
	if err := r.Status().Update(ctx, ep); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating endpoint status to Invalid: %w", err)
	}
	return ctrl.Result{}, nil
}

// gatewayToEndpoints maps a Gateway event to endpoints that reference it via field index.
func (r *KrakenDEndpointReconciler) gatewayToEndpoints(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &endpoints,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{endpointGatewayIndex: obj.GetName()},
	); err != nil {
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
	var endpoints v1alpha1.KrakenDEndpointList
	if err := r.List(ctx, &endpoints,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{endpointPolicyIndex: obj.GetName()},
	); err != nil {
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
