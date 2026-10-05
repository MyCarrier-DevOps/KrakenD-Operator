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
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// The third-party kinds a gateway creates when the matching feature is
// enabled. Their CRDs are optional.
var (
	dragonflyGVK      = schema.GroupVersionKind{Group: "dragonflydb.io", Version: "v1alpha1", Kind: "Dragonfly"}
	externalSecretGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	virtualServiceGVK = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "VirtualService"}

	optionalOwnedGVKs = []schema.GroupVersionKind{dragonflyGVK, externalSecretGVK, virtualServiceGVK}
)

// installedOptionalKinds splits the optional kinds into those whose CRDs the
// mapper knows now, and those it does not. It is evaluated once, at
// startup: a CRD installed later is watched only after an operator
// restart. Any lookup error other than "no such kind" is returned, so that
// the operator never starts without a watch it should have.
func installedOptionalKinds(mapper meta.RESTMapper) (installed, missing []schema.GroupVersionKind, err error) {
	for _, gvk := range optionalOwnedGVKs {
		ok, err := kindInstalled(mapper, gvk)
		if err != nil {
			return nil, nil, fmt.Errorf("checking whether the %s CRD is installed: %w", gvk.GroupKind(), err)
		}
		if ok {
			installed = append(installed, gvk)
		} else {
			missing = append(missing, gvk)
		}
	}
	return installed, missing, nil
}

// kindInstalled reports whether the mapper knows gvk. A kind the mapper has
// no match for is (false, nil); any other lookup error is returned.
func kindInstalled(mapper meta.RESTMapper, gvk schema.GroupVersionKind) (bool, error) {
	_, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}

// deleteIfControlled deletes obj, looked up by its name and namespace, when
// it exists and gw controls it. An object someone else owns under the same
// name is left alone.
func (r *KrakenDGatewayReconciler) deleteIfControlled(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, obj client.Object,
) error {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, gw) {
		return nil
	}
	uid := obj.GetUID()
	err := client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid}))
	if err != nil {
		// An unstructured object names its kind; a typed one its Go type.
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if kind == "" {
			kind = fmt.Sprintf("%T", obj)
		}
		return fmt.Errorf("deleting %s %s: %w", kind, key, err)
	}
	return nil
}

// deleteOptionalIfControlled is deleteIfControlled for an optional kind.
// Without its CRD there is nothing to delete.
func (r *KrakenDGatewayReconciler) deleteOptionalIfControlled(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, gvk schema.GroupVersionKind, name string,
) error {
	available, err := r.crdAvailable(gvk)
	if err != nil {
		return fmt.Errorf("checking %s CRD: %w", gvk.Kind, err)
	}
	if !available {
		return nil
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(gw.Namespace)
	return r.deleteIfControlled(ctx, gw, u)
}

// applyOwned creates or updates obj, which gw controls, with what build sets.
// kind names obj in the error.
func (r *KrakenDGatewayReconciler) applyOwned(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, obj client.Object, kind string, build func(),
) error {
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		build()
		return controllerutil.SetControllerReference(gw, obj, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconciling %s: %w", kind, err)
	}
	return nil
}

// applyOptional creates or updates gw's child of the optional kind gvk, built
// by build, when the CRD of that kind is installed, and returns it. applied is
// false, with no error, when the CRD is not installed: there is nothing to
// create.
func (r *KrakenDGatewayReconciler) applyOptional(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, gvk schema.GroupVersionKind, name string,
	build func(u *unstructured.Unstructured),
) (u *unstructured.Unstructured, applied bool, err error) {
	available, err := r.crdAvailable(gvk)
	if err != nil {
		return nil, false, fmt.Errorf("checking %s CRD: %w", gvk.Kind, err)
	}
	if !available {
		return nil, false, nil
	}
	u = &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(gw.Namespace)
	if err := r.applyOwned(ctx, gw, u, strings.ToLower(gvk.Kind), func() { build(u) }); err != nil {
		return nil, false, err
	}
	return u, true, nil
}

// reconcileDragonfly creates or updates the Dragonfly when it is enabled and
// its CRD is installed, and removes the one gw controls when it is not.
func (r *KrakenDGatewayReconciler) reconcileDragonfly(ctx context.Context, gw *v1alpha1.KrakenDGateway) error {
	if gw.Spec.Dragonfly != nil && gw.Spec.Dragonfly.Enabled {
		// Without the CRD there is nothing to create: detectDragonflyState
		// reports DragonflyReady=False/CRDNotInstalled.
		df, applied, err := r.applyOptional(ctx, gw, dragonflyGVK, resources.DragonflyName(gw),
			func(u *unstructured.Unstructured) { resources.BuildDragonfly(u, gw) })
		if applied {
			r.recordDragonflyRunAsRootCondition(gw, df)
		}
		return err
	}
	// Dragonfly is deliberately off (unset or Enabled: false). Mirrors
	// reconcilePostRestartJob's disabled/empty guard (the spec == nil ||
	// !spec.Enabled branch) so `kubectl describe krakendgateway` does not
	// keep showing a stale ConditionDragonflyRunAsRootUnacknowledged
	// forever after the user disables Dragonfly. Deliberately NOT
	// cleared when Dragonfly is enabled but the CRD is not yet installed —
	// that is a transient/environmental state, not a deliberate disable,
	// mirroring reconcilePostRestartJob's configChecksum == "" reasoning for
	// not flickering conditions away during an in-progress/incomplete state.
	meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionDragonflyRunAsRootUnacknowledged)
	if err := r.deleteOptionalIfControlled(ctx, gw, dragonflyGVK, resources.DragonflyName(gw)); err != nil {
		return err
	}
	meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionDragonflyReady)
	gw.Status.DragonflyAddress = ""
	dragonflyReady.DeleteLabelValues(gw.Namespace, gw.Name)
	return nil
}

// reconcileExternalSecret creates or updates the license ExternalSecret when
// license.externalSecret is enabled and its CRD is installed, and removes the
// one gw controls when it is not.
func (r *KrakenDGatewayReconciler) reconcileExternalSecret(ctx context.Context, gw *v1alpha1.KrakenDGateway) error {
	if gw.Spec.License != nil && gw.Spec.License.ExternalSecret.Enabled {
		// Without the CRD there is nothing to create: reconcileLicense reports
		// LicenseSecretUnavailable=True/CRDNotInstalled.
		_, _, err := r.applyOptional(ctx, gw, externalSecretGVK, resources.ExternalSecretName(gw),
			func(u *unstructured.Unstructured) { resources.BuildExternalSecret(u, gw) })
		return err
	}
	return r.deleteOptionalIfControlled(ctx, gw, externalSecretGVK, resources.ExternalSecretName(gw))
}

// reconcileVirtualService creates or updates the Istio VirtualService when
// Istio is enabled and its CRD is installed, and removes the one gw controls
// when it is not. IstioConfigured says which.
func (r *KrakenDGatewayReconciler) reconcileVirtualService(ctx context.Context, gw *v1alpha1.KrakenDGateway) error {
	if gw.Spec.Istio != nil && gw.Spec.Istio.Enabled {
		_, applied, err := r.applyOptional(ctx, gw, virtualServiceGVK, gw.Name,
			func(u *unstructured.Unstructured) { resources.BuildVirtualService(u, gw) })
		switch {
		case err != nil:
			return err
		case !applied:
			r.setConditionWithEvent(gw, metav1.Condition{
				Type:               v1alpha1.ConditionIstioConfigured,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: gw.Generation,
				Reason:             v1alpha1.ReasonCRDNotInstalled,
				Message:            "Istio is enabled but the networking.istio.io VirtualService CRD is not installed in the cluster",
			})
		default:
			r.setConditionWithEvent(gw, metav1.Condition{
				Type:               v1alpha1.ConditionIstioConfigured,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: gw.Generation,
				Reason:             v1alpha1.ReasonIstioVSCreated,
				Message:            "Istio VirtualService reconciled",
			})
		}
		return nil
	}
	if err := r.deleteOptionalIfControlled(ctx, gw, virtualServiceGVK, gw.Name); err != nil {
		return err
	}
	meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionIstioConfigured)
	return nil
}
