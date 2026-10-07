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
	stderrors "errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
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

// deleteIfControlled deletes obj, looked up through reader by its name and
// namespace, when it exists and gw controls it. An object someone else owns
// under the same name is left alone.
func (r *KrakenDGatewayReconciler) deleteIfControlled(
	ctx context.Context, reader client.Reader, gw *v1alpha1.KrakenDGateway, obj client.Object,
) (retErr error) {
	ctx, span := tracing.Start(ctx, r.Tracer, "gateway.delete_child",
		trace.WithAttributes(tracing.KeyName.String(obj.GetName())))
	defer func() { tracing.End(span, retErr) }()
	key := client.ObjectKeyFromObject(obj)
	if err := reader.Get(ctx, key, obj); err != nil {
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
// Without its CRD there is nothing to delete, and a CRD found absent is not
// asked about again for absentKindWindow. A kind that has an informer is
// looked up through it: a child it does not hold yet cannot be orphaned,
// because the child's Add event enqueues the gateway again and that pass
// deletes it. The child is read as metadata, which is all the informer holds.
func (r *KrakenDGatewayReconciler) deleteOptionalIfControlled(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, gvk schema.GroupVersionKind, name string,
) error {
	now := r.now()
	if r.absentKinds.absent(gvk, now) {
		return nil
	}
	available, err := r.crdAvailable(ctx, gvk)
	if err != nil {
		return fmt.Errorf("checking %s CRD: %w", gvk.Kind, err)
	}
	if !available {
		r.absentKinds.remember(gvk, now)
		return nil
	}
	r.absentKinds.forget(gvk)
	// Only the owner references are read, and a kind with an informer
	// caches metadata only: a full read would start a second informer.
	obj := &metav1.PartialObjectMetadata{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	obj.SetNamespace(gw.Namespace)
	return r.deleteIfControlled(ctx, r.optionalReader(gvk), gw, obj)
}

// now reads the reconciler's clock, the wall clock when none is wired.
func (r *KrakenDGatewayReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock.Now()
}

// optionalReader is the reader for gvk's children: the informer cache for a
// kind that has an informer, the API reader otherwise. The manager client
// would serve a metadata read from its cache and start an informer for it.
func (r *KrakenDGatewayReconciler) optionalReader(gvk schema.GroupVersionKind) client.Reader {
	if _, ok := r.cachedOptionalKinds[gvk]; ok && r.optionalCache != nil {
		return r.optionalCache
	}
	return r.APIReader
}

// errNotControlled is what applyOwned and the Deployment write return, wrapped
// in a notControlledError, for an existing object the gateway may not take
// over.
var errNotControlled = stderrors.New("exists and the gateway does not control it")

// notControlledError names an existing object the gateway refused to take
// over: its kind, its place, and the controller that owns it, if any.
type notControlledError struct {
	kind, namespace, name string
	controller            string
	// consent are the labels that would hand the object over.
	consent map[string]string
}

func (e *notControlledError) Error() string {
	if e.controller != "" {
		return fmt.Sprintf("%s %s/%s is controlled by %s", e.kind, e.namespace, e.name, e.controller)
	}
	return fmt.Sprintf("%s %s/%s has no controller and lacks the labels %s",
		e.kind, e.namespace, e.name, labels.Set(e.consent))
}

func (e *notControlledError) Unwrap() error { return errNotControlled }

// refuseUncontrolled returns a notControlledError when obj, as fetched, exists
// and gw may not take it over. The gateway controls it, or it has no
// controller and carries the consent labels, which only someone who can write
// the object can set: that hands it over, as an object orphaned by
// `kubectl delete --cascade=orphan` still carries them. The consent labels are
// the instance and managed-by labels the operator's own builder stamps on that
// kind. It must run before the builder, which sets those labels on every object.
func refuseUncontrolled(gw *v1alpha1.KrakenDGateway, obj client.Object, kind string, consent map[string]string) error {
	if obj.GetResourceVersion() == "" || metav1.IsControlledBy(obj, gw) {
		return nil
	}
	owner := metav1.GetControllerOf(obj)
	handedOver := labels.SelectorFromSet(consent).Matches(labels.Set(obj.GetLabels()))
	if owner == nil && handedOver {
		return nil
	}
	refused := &notControlledError{
		kind: kind, namespace: obj.GetNamespace(), name: obj.GetName(), consent: consent,
	}
	if owner != nil {
		refused.controller = owner.Kind + "/" + owner.Name
	}
	return refused
}

// notControlledIn returns every notControlledError in err's tree. errors.As
// finds only the first, so it walks the tree by hand.
func notControlledIn(err error) []*notControlledError {
	switch e := err.(type) { //nolint:errorlint // walks the tree; errors.As returns one match
	case nil:
		return nil
	case *notControlledError:
		return []*notControlledError{e}
	case interface{ Unwrap() []error }:
		var all []*notControlledError
		for _, inner := range e.Unwrap() {
			all = append(all, notControlledIn(inner)...)
		}
		return all
	case interface{ Unwrap() error }:
		return notControlledIn(e.Unwrap())
	}
	return nil
}

// setResourcesControlled records on gw which existing objects this pass left
// alone, found in the errors err carries: False, naming each and what to do,
// while there is one. It is True only when the pass refused nothing, failed
// nothing and wrote the Deployment (deploymentWritten); a pass that failed for
// another reason, or held the Deployment, could not evaluate every child, so it
// leaves the condition as it was.
func (r *KrakenDGatewayReconciler) setResourcesControlled(
	gw *v1alpha1.KrakenDGateway, err error, deploymentWritten bool,
) {
	refused := notControlledIn(err)
	if len(refused) == 0 {
		if err != nil || !deploymentWritten {
			return
		}
		r.setConditionWithEvent(gw, metav1.Condition{
			Type:               v1alpha1.ConditionResourcesControlled,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonResourcesControlled,
			Message:            "no object the gateway wrote was refused",
		})
		return
	}
	named := make([]string, len(refused))
	for i, e := range refused {
		named[i] = e.Error()
	}
	slices.Sort(named)
	r.setConditionWithEvent(gw, metav1.Condition{
		Type:               v1alpha1.ConditionResourcesControlled,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonResourceNotControlled,
		Message: fmt.Sprintf("the gateway leaves alone existing objects it would write: %s. "+
			"Rename the gateway, or hand over an object that has no controller by giving it the labels named for it",
			strings.Join(named, "; ")),
	})
}

// applyOwned creates or updates obj, which gw controls, with what build sets.
// kind names obj in the error, and consent are the labels that hand an
// existing obj over (see refuseUncontrolled). An existing obj that gw may not take over
// (see refuseUncontrolled) is left as it is and a notControlledError returned.
func (r *KrakenDGatewayReconciler) applyOwned(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, obj client.Object, kind string, consent map[string]string,
	build func(),
) (retErr error) {
	ctx, span := tracing.Start(ctx, r.Tracer, "apply "+kind,
		trace.WithAttributes(tracing.KeyName.String(obj.GetName())))
	defer func() { tracing.End(span, retErr) }()
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if err := refuseUncontrolled(gw, obj, kind, consent); err != nil {
			return err
		}
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
	consent map[string]string, build func(u *unstructured.Unstructured),
) (u *unstructured.Unstructured, applied bool, err error) {
	available, err := r.crdAvailable(ctx, gvk)
	if err != nil {
		return nil, false, fmt.Errorf("checking %s CRD: %w", gvk.Kind, err)
	}
	if !available {
		return nil, false, nil
	}
	r.absentKinds.forget(gvk)
	u = &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace(gw.Namespace)
	if err := r.applyOwned(ctx, gw, u, strings.ToLower(gvk.Kind), consent, func() { build(u) }); err != nil {
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
		consent := resources.DragonflyConsentLabels(gw)
		df, applied, err := r.applyOptional(ctx, gw, dragonflyGVK, resources.DragonflyName(gw), consent,
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
	r.metrics().ForgetDragonflyReady(client.ObjectKeyFromObject(gw))
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
			resources.SelectorLabels(gw), func(u *unstructured.Unstructured) { resources.BuildExternalSecret(u, gw) })
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
			resources.SelectorLabels(gw), func(u *unstructured.Unstructured) { resources.BuildVirtualService(u, gw) })
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
