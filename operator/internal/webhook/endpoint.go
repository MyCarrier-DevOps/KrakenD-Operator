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

package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

const kindEndpoint = "KrakenDEndpoint"

// EndpointValidator validates KrakenDEndpoint resources.
type EndpointValidator struct {
	client.Client
}

// ValidateCreate validates a new KrakenDEndpoint.
func (v *EndpointValidator) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDEndpoint, got %T", obj)
	}
	return v.admit(ctx, nil, ep)
}

// ValidateUpdate validates an updated KrakenDEndpoint. Only what the update
// changes is judged: an unchanged spec, and unchanged entries and references,
// are never re-validated.
func (v *EndpointValidator) ValidateUpdate(
	ctx context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	if terminatingWithUnchangedSpec(oldObj, newObj) {
		return nil, nil
	}
	ep, ok := newObj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDEndpoint, got %T", newObj)
	}
	old, ok := oldObj.(*v1alpha1.KrakenDEndpoint)
	if !ok {
		return nil, fmt.Errorf("expected KrakenDEndpoint, got %T", oldObj)
	}
	if equality.Semantic.DeepEqual(old.Spec, ep.Spec) {
		return nil, nil
	}
	return v.admit(ctx, old, ep)
}

// ValidateDelete is a no-op for endpoints.
func (v *EndpointValidator) ValidateDelete(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

// admit runs every rule against ep. old is the stored object on an update and
// nil on a create.
func (v *EndpointValidator) admit(
	ctx context.Context, old, ep *v1alpha1.KrakenDEndpoint,
) (admission.Warnings, error) {
	gw, errs, err := v.gatewayFor(ctx, old, ep)
	if err != nil {
		return nil, unavailable(err)
	}
	refErrs, err := v.validatePolicyRefs(ctx, old, ep)
	if err != nil {
		return nil, unavailable(err)
	}
	errs = append(errs, refErrs...)
	stored := old
	if movedGateway(old, ep) {
		stored = nil // another gateway judges every entry afresh
	}
	changed := changedEntries(stored, ep)
	for _, i := range changed {
		errs = append(errs, validateExtraConfigAudience(
			field.NewPath("spec", "endpoints").Index(i).Child("extraConfig"), ep.Spec.Endpoints[i].ExtraConfig)...)
	}
	if gw != nil {
		dupErrs, err := v.validateRouteUniqueness(ctx, ep, changed, gw)
		if err != nil {
			return nil, unavailable(err)
		}
		errs = append(errs, dupErrs...)
	}
	return nil, invalid(kindEndpoint, ep.Name, errs)
}

// gatewayFor returns ep's gateway, or nil when there is none to check
// against. A missing gateway is an error only when the request sets or
// changes gatewayRef.
func (v *EndpointValidator) gatewayFor(
	ctx context.Context, old, ep *v1alpha1.KrakenDEndpoint,
) (*v1alpha1.KrakenDGateway, field.ErrorList, error) {
	key := types.NamespacedName{
		Name: ep.Spec.GatewayRef.Name, Namespace: ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace),
	}
	gw := &v1alpha1.KrakenDGateway{}
	err := v.Get(ctx, key, gw)
	switch {
	case err == nil:
		return gw, nil, nil
	case !apierrors.IsNotFound(err):
		return nil, nil, fmt.Errorf("looking up gateway: %w", err)
	case old != nil && old.Spec.GatewayRef == ep.Spec.GatewayRef:
		return nil, nil, nil
	}
	refPath, refValue := field.NewPath("spec", "gatewayRef", "name"), key.Name
	if key.Namespace != ep.Namespace {
		refPath, refValue = field.NewPath("spec", "gatewayRef", "namespace"), key.Namespace
	}
	return nil, field.ErrorList{field.NotFound(refPath, refValue)}, nil
}

// validatePolicyRefs checks the policies ep references that old does not.
func (v *EndpointValidator) validatePolicyRefs(
	ctx context.Context, old, ep *v1alpha1.KrakenDEndpoint,
) (field.ErrorList, error) {
	known := map[string]bool{}
	if old != nil {
		for _, key := range fieldindex.EndpointPolicyKeys(old) {
			known[key] = true
		}
	}
	var errs field.ErrorList
	for i, entry := range ep.Spec.Endpoints {
		for j, be := range entry.Backends {
			if be.PolicyRef == nil || known[be.PolicyRef.PolicyKey(ep.Namespace)] {
				continue
			}
			p := field.NewPath("spec", "endpoints").Index(i).Child("backends").Index(j).Child("policyRef")
			refErrs, err := v.policyRefError(ctx, p, ep.Namespace, be.PolicyRef)
			if err != nil {
				return nil, err
			}
			errs = append(errs, refErrs...)
		}
	}
	return errs, nil
}

// policyRefError returns the errors for a reference to a policy that is not
// usable, none when it is.
func (v *EndpointValidator) policyRefError(
	ctx context.Context, p *field.Path, namespace string, ref *v1alpha1.PolicyRef,
) (field.ErrorList, error) {
	polNS := ref.ResolvedNamespace(namespace)
	policy := &v1alpha1.KrakenDBackendPolicy{}
	err := v.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: polNS}, policy)
	switch {
	case apierrors.IsNotFound(err):
		if polNS != namespace {
			return field.ErrorList{field.NotFound(p.Child("namespace"), polNS)}, nil
		}
		return field.ErrorList{field.NotFound(p.Child("name"), ref.Name)}, nil
	case err != nil:
		return nil, fmt.Errorf("looking up policy %s/%s: %w", polNS, ref.Name, err)
	}
	return nil, nil
}

// movedGateway reports whether ep resolves to a different gateway than old.
func movedGateway(old, ep *v1alpha1.KrakenDEndpoint) bool {
	if old == nil {
		return false
	}
	oldRef, ref := old.Spec.GatewayRef, ep.Spec.GatewayRef
	return oldRef.Name != ref.Name || oldRef.ResolvedNamespace(old.Namespace) != ref.ResolvedNamespace(ep.Namespace)
}

// changedEntries returns the positions of ep's entries that are new or differ
// from the stored entry with the same endpoint and method. On a create every
// entry has changed.
func changedEntries(old, ep *v1alpha1.KrakenDEndpoint) []int {
	stored := map[string]v1alpha1.EndpointEntry{}
	if old != nil {
		for _, e := range old.Spec.Endpoints {
			stored[e.Method+" "+e.Endpoint] = e
		}
	}
	var changed []int
	for i, e := range ep.Spec.Endpoints {
		if s, ok := stored[e.Method+" "+e.Endpoint]; ok && equality.Semantic.DeepEqual(s, e) {
			continue
		}
		changed = append(changed, i)
	}
	return changed
}

// validateRouteUniqueness rejects each changed entry whose route another
// KrakenDEndpoint on the same gateway already claims.
func (v *EndpointValidator) validateRouteUniqueness(
	ctx context.Context, ep *v1alpha1.KrakenDEndpoint, changed []int, gw *v1alpha1.KrakenDGateway,
) (field.ErrorList, error) {
	var list v1alpha1.KrakenDEndpointList
	if err := v.List(ctx, &list, client.UnsafeDisableDeepCopy,
		client.MatchingFields{fieldindex.EndpointGateway: gw.Namespace + "/" + gw.Name}); err != nil {
		return nil, fmt.Errorf("listing endpoints of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	type claim struct{ owner, endpoint string }
	claims := map[string]claim{}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Namespace == ep.Namespace && other.Name == ep.Name {
			continue
		}
		for _, e := range other.Spec.Endpoints {
			claims[routeKey(e)] = claim{
				owner: "KrakenDEndpoint " + other.Namespace + "/" + other.Name, endpoint: e.Endpoint,
			}
		}
	}
	var errs field.ErrorList
	for _, i := range changed {
		e := ep.Spec.Endpoints[i]
		p := field.NewPath("spec", "endpoints").Index(i)
		if c, ok := claims[routeKey(e)]; ok {
			errs = append(errs, routeClash(p, e, c.endpoint, c.owner))
			continue
		}
		for j, other := range ep.Spec.Endpoints {
			if j != i && routeKey(other) == routeKey(e) {
				errs = append(errs, routeClash(p, e, other.Endpoint, fmt.Sprintf("spec.endpoints[%d]", j)))
				break
			}
		}
	}
	return errs, nil
}

func routeKey(e v1alpha1.EndpointEntry) string {
	return e.Method + " " + renderer.ConflictKey(e.Endpoint)
}

// routeClash reports that e's route is already claimed by otherPath in owner.
func routeClash(p *field.Path, e v1alpha1.EndpointEntry, otherPath, owner string) *field.Error {
	err := field.Duplicate(p, e.Method+" "+e.Endpoint)
	if otherPath == e.Endpoint {
		err.Detail = "already defined by " + owner
		return err
	}
	err.Detail = fmt.Sprintf("has the same route as %s %s in %s: paths that differ only in parameter names "+
		"cannot both be routed. Use the same parameter name, and keep routes that share a parameterized "+
		"prefix in one KrakenDEndpoint so they can be renamed together", e.Method, otherPath, owner)
	return err
}
