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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

const kindEndpoint = "KrakenDEndpoint"

// EndpointValidator validates KrakenDEndpoint resources.
type EndpointValidator struct {
	client.Client
	Checker ConfigChecker
	// OperatorUsername is the username the operator's own API requests carry.
	// Its writes to endpoints a KrakenDAutoConfig controls skip the render
	// check (the AutoConfig controller checks its whole desired set first);
	// empty disables the exemption.
	OperatorUsername string
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

// admit runs every rule against ep within the admission budget. old is the
// stored object on an update and nil on a create.
func (v *EndpointValidator) admit(
	ctx context.Context, old, ep *v1alpha1.KrakenDEndpoint,
) (admission.Warnings, error) {
	ctx, cancel := context.WithTimeout(ctx, admissionBudget)
	defer cancel()

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
		errs = append(errs, validateEntries(ep, changed, gw)...)
		dupErrs, err := v.validateRouteUniqueness(ctx, ep, stored, changed, gw)
		if err != nil {
			return nil, unavailable(err)
		}
		errs = append(errs, dupErrs...)
	}
	if len(errs) > 0 {
		return nil, invalid(kindEndpoint, ep.Name, errs)
	}
	if gw == nil {
		return nil, nil
	}
	if v.trustedWrite(ctx, ep) {
		logf.FromContext(ctx).V(1).Info("operator write to an AutoConfig endpoint: render check skipped",
			"endpoint", ep.Namespace+"/"+ep.Name)
		return nil, nil
	}
	return v.checkRender(ctx, stored, ep, gw)
}

// trustedWrite reports whether the request is the operator, by its exact
// username, writing an endpoint a KrakenDAutoConfig controls. An empty
// OperatorUsername trusts nobody.
func (v *EndpointValidator) trustedWrite(ctx context.Context, ep *v1alpha1.KrakenDEndpoint) bool {
	if v.OperatorUsername == "" {
		return false
	}
	req, err := admission.RequestFromContext(ctx)
	if err != nil || req.UserInfo.Username != v.OperatorUsername {
		return false
	}
	return autoConfigController(ep)
}

// autoConfigController reports whether ep's controller owner reference is a
// KrakenDAutoConfig. Labels are never trusted for this: anyone can set them.
func autoConfigController(ep *v1alpha1.KrakenDEndpoint) bool {
	ref := metav1.GetControllerOf(ep)
	return ref != nil && ref.Kind == "KrakenDAutoConfig" && ref.APIVersion == v1alpha1.GroupVersion.String()
}

// checkRender renders ep's gateway with ep and rejects the request only when
// that turns a passing config into a failing one. When the gateway already
// fails without ep, ep is judged in isolation: the gateway root plus ep alone,
// against the root plus stored alone, the stored object on an update that
// stays on its gateway and nil otherwise (a create, or a move to another
// gateway), where the baseline is the root by itself.
func (v *EndpointValidator) checkRender(
	ctx context.Context, stored, ep *v1alpha1.KrakenDEndpoint, gw *v1alpha1.KrakenDGateway,
) (admission.Warnings, error) {
	var baseline []v1alpha1.KrakenDEndpoint
	if stored != nil {
		baseline = []v1alpha1.KrakenDEndpoint{*stored}
	}
	candidate := []v1alpha1.KrakenDEndpoint{*ep}
	return ratchetRender(ctx, renderChecks{
		after:     bindCheck(v.Checker.CheckGateway, gw, candidate),
		before:    bindCheck(v.Checker.CheckGateway, gw, nil),
		isoAfter:  bindCheck(v.Checker.CheckIsolated, gw, candidate),
		isoBefore: bindCheck(v.Checker.CheckIsolated, gw, baseline),
	},
		func(verdict configcheck.Verdict) error { return renderDenial(ep, verdict) },
		func(before configcheck.Verdict) string {
			return fmt.Sprintf("gateway %s/%s already fails validation without this change: %s",
				gw.Namespace, gw.Name, before.Summary(warningLimit))
		})
}

// maxEntryCauses is how many entries of the candidate a denial lists as causes
// of their own.
const maxEntryCauses = 20

// renderDenial rejects ep with one cause per entry of ep the verdict blames,
// its findings joined and cut to the warning limit, for the first
// maxEntryCauses entries. Findings about other objects or the gateway root, and
// those of the entries beyond the limit, go on spec.endpoints as a bounded
// summary that counts what it leaves out.
func renderDenial(ep *v1alpha1.KrakenDEndpoint, verdict configcheck.Verdict) error {
	self := types.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
	byEntry := map[int][]configcheck.Finding{}
	var blamed []int
	var others []configcheck.Finding
	for _, f := range verdict.Findings {
		if f.Endpoint != self || f.Index < 0 || f.Index >= len(ep.Spec.Endpoints) {
			others = append(others, f)
			continue
		}
		if _, ok := byEntry[f.Index]; !ok {
			blamed = append(blamed, f.Index)
		}
		byEntry[f.Index] = append(byEntry[f.Index], f)
	}
	slices.Sort(blamed)
	var errs field.ErrorList
	for n, i := range blamed {
		if n == maxEntryCauses {
			for _, rest := range blamed[n:] {
				others = append(others, byEntry[rest]...)
			}
			break
		}
		var messages []string
		for _, f := range byEntry[i] {
			messages = append(messages, f.Message)
		}
		entry := ep.Spec.Endpoints[i]
		errs = append(errs, field.Invalid(field.NewPath("spec", "endpoints").Index(i),
			entry.Method+" "+entry.Endpoint, truncate(strings.Join(messages, "; "), warningLimit)))
	}
	if len(others) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "endpoints"), field.OmitValueType{},
			"with this change the gateway's config fails krakend check: "+
				configcheck.Verdict{Findings: others}.Summary(warningLimit)))
	}
	return invalid(kindEndpoint, ep.Name, errs)
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

// validatePolicyRefs checks the policies ep references that old does not. The
// ratchet keys on policy identity: a new backend that references a policy the
// stored object already references elsewhere is not re-checked. A referenced
// policy cannot be deleted (its protection finalizer holds it), so the gap
// only matters for stored references that already dangle.
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
	case !policy.DeletionTimestamp.IsZero():
		return field.ErrorList{field.Invalid(p.Child("name"), ref.Name, "policy is being deleted")}, nil
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

// newRoutes returns the changed entries whose route no stored entry has. An
// edit to an entry that already holds its route is not a new claim on it, so
// a stored clash never blocks the owner of the served entry.
func newRoutes(stored, ep *v1alpha1.KrakenDEndpoint, changed []int) []int {
	held := map[string]bool{}
	if stored != nil {
		for _, e := range stored.Spec.Endpoints {
			held[routeKey(e)] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(changed), func(i int) bool {
		return held[routeKey(ep.Spec.Endpoints[i])]
	})
}

// validateRouteUniqueness rejects each listed entry whose route another entry
// on the same gateway already claims: the same method and path, or the same
// method and route shape, meaning paths that differ only in parameter names
// or repeated slashes, which KrakenD's router cannot tell apart. Endpoints
// with ep's controller are exempt: while an AutoConfig renames an operation
// its new endpoint and the old one share a route until the old one is deleted.
// The denial names the claimant the renderer serves. Claims of other
// endpoints are checked only for routes new to the stored object; entries of
// ep itself are compared whenever one changes.
func (v *EndpointValidator) validateRouteUniqueness(
	ctx context.Context, ep, stored *v1alpha1.KrakenDEndpoint, changed []int, gw *v1alpha1.KrakenDGateway,
) (field.ErrorList, error) {
	var list v1alpha1.KrakenDEndpointList
	if err := v.List(ctx, &list, client.UnsafeDisableDeepCopy,
		client.MatchingFields{fieldindex.EndpointGateway: gw.Namespace + "/" + gw.Name}); err != nil {
		return nil, fmt.Errorf("listing endpoints of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	claims := map[string]claim{}
	for i := range list.Items {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("checking routes of gateway %s/%s: %w", gw.Namespace, gw.Name, err)
		}
		other := &list.Items[i]
		if other.Namespace == ep.Namespace && other.Name == ep.Name || sameController(ep, other) {
			continue
		}
		for idx, e := range other.Spec.Endpoints {
			c := claim{
				owner: other.Namespace + "/" + other.Name, endpoint: e.Endpoint,
				created: other.CreationTimestamp.Unix(), index: idx,
			}
			if prev, ok := claims[routeKey(e)]; !ok || c.servedBefore(prev) {
				claims[routeKey(e)] = c
			}
		}
	}
	keys := make([]string, len(ep.Spec.Endpoints))
	entriesByKey := map[string][]int{}
	for i, e := range ep.Spec.Endpoints {
		keys[i] = routeKey(e)
		entriesByKey[keys[i]] = append(entriesByKey[keys[i]], i)
	}
	var errs field.ErrorList
	fresh := newRoutes(stored, ep, changed)
	for _, i := range changed {
		e := ep.Spec.Endpoints[i]
		p := field.NewPath("spec", "endpoints").Index(i)
		if c, ok := claims[keys[i]]; ok && slices.Contains(fresh, i) {
			errs = append(errs, routeClash(p, e, c.endpoint, "KrakenDEndpoint "+c.owner))
			continue
		}
		for _, j := range entriesByKey[keys[i]] {
			if j != i {
				owner := fmt.Sprintf("spec.endpoints[%d]", j)
				errs = append(errs, routeClash(p, e, ep.Spec.Endpoints[j].Endpoint, owner))
				break
			}
		}
	}
	return errs, nil
}

// claim is an entry of another KrakenDEndpoint that holds a route.
type claim struct {
	owner, endpoint string // owner is namespace/name
	created         int64
	index           int
}

// servedBefore reports whether c wins the route over o in the renderer's
// order: the older KrakenDEndpoint, then the lower name, then the earlier
// entry. Admission names the winner, whatever order the informer lists in.
func (c claim) servedBefore(o claim) bool {
	if c.created != o.created {
		return c.created < o.created
	}
	if c.owner != o.owner {
		return c.owner < o.owner
	}
	return c.index < o.index
}

// sameController reports whether a and b have the same controller owner, for
// example two endpoints one KrakenDAutoConfig generated.
func sameController(a, b metav1.Object) bool {
	ca, cb := metav1.GetControllerOf(a), metav1.GetControllerOf(b)
	return ca != nil && cb != nil && ca.UID == cb.UID
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
		"or repeated slashes cannot both be routed. Use the same parameter name, and keep routes that share "+
		"a parameterized prefix in one KrakenDEndpoint so they can be renamed together", e.Method, otherPath, owner)
	return err
}
