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
	stderrors "errors"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
)

// configResult is what the config stage hands the infrastructure stage.
type configResult struct {
	// appliedConfigMap is the ConfigMap holding the applied config. It is ""
	// when none does, and the Deployment is then left as it is.
	appliedConfigMap string
	// heldBecause says why appliedConfigMap is "": nil means no ConfigMap
	// exists, otherwise the ConfigMap that does failed verification.
	heldBecause error
	// output is the render the rest of the pass reports on: the applied
	// config when this pass applied or kept it, otherwise the newest render.
	output *renderer.RenderOutput
	// excluded are the endpoints this pass found failing on their own.
	excluded map[types.NamespacedName]configcheck.EndpointVerdict
	// judged says every endpoint was judged this pass.
	judged bool
}

// publishApplied republishes the applied config. Publishing is idempotent,
// so this also restores a config ConfigMap deleted out of band.
func (r *KrakenDGatewayReconciler) publishApplied(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, output *renderer.RenderOutput,
) (configResult, error) {
	if err := r.publishConfig(ctx, gw, output.JSON, output.Checksum); err != nil {
		return configResult{heldBecause: err}, err
	}
	return configResult{appliedConfigMap: resources.ConfigMapName(gw, output.Checksum)}, nil
}

// keepApplied is the outcome of a pass that applies nothing new: the applied
// config stays, served from whichever ConfigMap still holds it.
func (r *KrakenDGatewayReconciler) keepApplied(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, verdictErr error,
) (configResult, error) {
	name, err := r.appliedConfigMapName(ctx, gw)
	return configResult{appliedConfigMap: name, heldBecause: err}, stderrors.Join(verdictErr, err)
}

// appliedConfigMapName returns the ConfigMap holding the applied config
// (status.configChecksum). When that ConfigMap does not exist yet but the
// one an earlier operator version kept under the gateway's own name holds
// exactly the applied config, it is created from that one. This is the
// upgrade path when the first reconcile after an upgrade renders a config
// that is then rejected. "" means no ConfigMap holds the applied config.
func (r *KrakenDGatewayReconciler) appliedConfigMapName(
	ctx context.Context, gw *v1alpha1.KrakenDGateway,
) (string, error) {
	applied := gw.Status.ConfigChecksum
	if applied == "" {
		return "", nil
	}
	name := resources.ConfigMapName(gw, applied)
	// A ConfigMap that is not this gateway's copy but holds the applied
	// config's bytes is an error: the Deployment is held as it is, mounting
	// the right bytes. One that holds other bytes is deleted
	// (verifyExistingConfigMap). This pass has no applied render to publish
	// again; only the legacy ConfigMap below can restore it. Otherwise the
	// Deployment is held, mounting a name that no longer exists, until a
	// render is applied.
	found, err := r.verifyExistingConfigMap(ctx, r.Client, gw, applied)
	if err != nil {
		return "", err
	}
	if found {
		return name, nil
	}
	legacy, err := r.legacyConfig(ctx, gw)
	if err != nil || legacy == nil || hash.SHA256Hex(legacy) != applied {
		return "", err
	}
	if err := r.publishConfig(ctx, gw, legacy, applied); err != nil {
		return "", err
	}
	return name, nil
}

// legacyConfig returns the config in the ConfigMap an earlier operator
// version kept under the gateway's own name, or nil when the gateway controls
// no such ConfigMap.
func (r *KrakenDGatewayReconciler) legacyConfig(ctx context.Context, gw *v1alpha1.KrakenDGateway) ([]byte, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &cm); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(&cm, gw) {
		return nil, nil
	}
	return []byte(cm.Data[resources.ConfigKey]), nil
}

// errAppliedConfigMissing is logged when no ConfigMap holds the applied
// config, so the Deployment is held as it is.
var errAppliedConfigMissing = stderrors.New("no ConfigMap holds the applied config")

// publishConfig makes sure the immutable ConfigMap for checksum exists and
// holds jsonData. An existing ConfigMap is never updated: its name is its
// content's address.
func (r *KrakenDGatewayReconciler) publishConfig(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, jsonData []byte, checksum string,
) error {
	name := resources.ConfigMapName(gw, checksum)
	if found, err := r.verifyExistingConfigMap(ctx, r.Client, gw, checksum); found || err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gw.Namespace}}
	resources.BuildConfigMap(cm, gw, jsonData, checksum)
	if err := controllerutil.SetControllerReference(gw, cm, r.Scheme); err != nil {
		return fmt.Errorf("setting owner on configmap %s: %w", name, err)
	}
	err := r.Create(ctx, cm)
	switch {
	case err == nil:
		r.verified.remember(client.ObjectKeyFromObject(gw), name, configMapVersion{cm.UID, cm.ResourceVersion})
		return nil
	case errors.IsAlreadyExists(err):
		// A ConfigMap of that name appeared after the existence check above
		// (a lost create race): it may be this controller's own earlier
		// create or someone else's. Verify it through the uncached reader.
		found, err := r.verifyExistingConfigMap(ctx, r.APIReader, gw, checksum)
		if err == nil && !found {
			return fmt.Errorf("configmap %s was deleted after a create found it existing", name)
		}
		return err
	default:
		return fmt.Errorf("creating configmap %s: %w", name, err)
	}
}

// verifyExistingConfigMap looks up the config ConfigMap for checksum through
// reader. found says whether one exists; when it does, err says whether it
// is this gateway's copy of that config. Its owner and checksum annotation
// are read as metadata only; the annotation is stripped from the cached
// metadata, so this read must stay live (client.CacheOptions.DisableFor).
// Both are copyable by anyone who can create ConfigMaps, and the pods load the
// payload whatever the metadata says, so the payload is hashed too, of any
// ConfigMap at that name: one whose krakend.json does not hash to checksum is
// deleted, whoever owns it, and reported as not found, for the caller to create
// again. One that holds the right bytes but is not this gateway's copy is
// reported through err.
func (r *KrakenDGatewayReconciler) verifyExistingConfigMap(
	ctx context.Context, reader client.Reader, gw *v1alpha1.KrakenDGateway, checksum string,
) (found bool, err error) {
	name := resources.ConfigMapName(gw, checksum)
	cm := &metav1.PartialObjectMetadata{}
	cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	if err := reader.Get(ctx, types.NamespacedName{Name: name, Namespace: gw.Namespace}, cm); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting configmap %s: %w", name, err)
	}
	gwKey, version := client.ObjectKeyFromObject(gw), configMapVersion{cm.UID, cm.ResourceVersion}
	if err := verifyConfigMap(cm, gw, checksum); err != nil {
		// What the pods load is the payload, not the metadata: a ConfigMap at
		// the gateway's content address that holds another payload is removed
		// whoever owns it. One that holds this config stays not this gateway's
		// copy (err), but serves the right bytes.
		if !r.verified.has(gwKey, name, version) {
			if found, perr := r.verifyPayload(ctx, reader, gw, name, checksum); perr != nil || !found {
				return found, perr
			}
		}
		return true, err
	}
	if r.verified.has(gwKey, name, version) {
		return true, nil
	}
	return r.verifyPayload(ctx, reader, gw, name, checksum)
}

// verifyPayload hashes the krakend.json of the ConfigMap name and deletes it
// when the hash is not checksum, with a Warning event on the gateway. found is
// false when the ConfigMap is gone or was deleted here.
func (r *KrakenDGatewayReconciler) verifyPayload(
	ctx context.Context, reader client.Reader, gw *v1alpha1.KrakenDGateway, name, checksum string,
) (found bool, err error) {
	full := &corev1.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Name: name, Namespace: gw.Namespace}, full); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting configmap %s: %w", name, err)
	}
	gwKey := client.ObjectKeyFromObject(gw)
	if hash.SHA256Hex([]byte(full.Data[resources.ConfigKey])) == checksum {
		r.verified.remember(gwKey, name, configMapVersion{full.UID, full.ResourceVersion})
		return true, nil
	}
	r.verified.forget(gwKey, name)
	uid := full.UID
	if err := r.Delete(ctx, full, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
		return true, fmt.Errorf("deleting configmap %s whose data does not match its checksum: %w", name, err)
	}
	r.Recorder.Eventf(gw, corev1.EventTypeWarning, v1alpha1.ReasonConfigMapTampered,
		"Deleted configmap %s: its %s does not hash to the checksum its name addresses", name, resources.ConfigKey)
	return false, nil
}

// verifyConfigMap rejects a ConfigMap that has the content-addressed name for
// checksum but is not this gateway's copy of that config.
func verifyConfigMap(cm metav1.Object, gw *v1alpha1.KrakenDGateway, checksum string) error {
	if !metav1.IsControlledBy(cm, gw) {
		return fmt.Errorf(
			"configmap %s/%s exists but is not controlled by gateway %s", cm.GetNamespace(), cm.GetName(), gw.Name)
	}
	if got := cm.GetAnnotations()[resources.PostRestartJobChecksumAnnotation]; got != checksum {
		return fmt.Errorf("configmap %s/%s holds config %s, not %s", cm.GetNamespace(), cm.GetName(), got, checksum)
	}
	return nil
}

// configMapHistoryLimit is how many of a gateway's config revisions GC keeps
// even when nothing mounts them.
const configMapHistoryLimit = 3

// collectConfigMaps deletes the gateway's config ConfigMaps that nothing can
// still mount. It lists them as metadata only. A ConfigMap is kept when any
// of these holds:
//   - it is inUse (the applied config, which the Deployment template mounts);
//   - it is in the revision history (configMapGCCandidates);
//   - a live ReplicaSet of the gateway's Deployment mounts it.
//
// The ConfigMap an earlier operator version kept under the gateway's own name
// is collected on the same terms, but never counts toward the history.
func (r *KrakenDGatewayReconciler) collectConfigMaps(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, inUse string,
) error {
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMapList"))
	if err := r.List(ctx, list, client.InNamespace(gw.Namespace),
		client.MatchingLabels(resources.SelectorLabels(gw))); err != nil {
		return fmt.Errorf("listing config configmaps: %w", err)
	}
	candidates := configMapGCCandidates(gw, list.Items, inUse)
	if len(candidates) == 0 {
		return nil
	}
	mounted, err := r.liveReplicaSetConfigMaps(ctx, gw)
	if err != nil {
		return err
	}
	for i := range candidates {
		cm := &candidates[i]
		if mounted[cm.Name] {
			continue
		}
		cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
		uid := cm.UID
		if err := r.Delete(ctx, cm, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting configmap %s: %w", cm.Name, err)
		}
	}
	return nil
}

// configMapGCCandidates returns the gateway's config ConfigMaps that only a
// live ReplicaSet could still keep: every controlled one except inUse and the
// revision history.
func configMapGCCandidates(
	gw *v1alpha1.KrakenDGateway, cms []metav1.PartialObjectMetadata, inUse string,
) []metav1.PartialObjectMetadata {
	var revisions, candidates []metav1.PartialObjectMetadata
	for i := range cms {
		cm := cms[i]
		if !metav1.IsControlledBy(&cm, gw) {
			continue
		}
		switch {
		case cm.Labels[resources.ConfigRevisionLabel] != "":
			revisions = append(revisions, cm)
		case cm.Name == gw.Name && cm.Name != inUse:
			candidates = append(candidates, cm)
		}
	}
	slices.SortFunc(revisions, func(a, b metav1.PartialObjectMetadata) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c // newest first
		}
		return cmp.Compare(a.Name, b.Name)
	})
	// The history is configMapHistoryLimit revisions: inUse, however old,
	// plus the most recently created others.
	kept := 1
	for i := range revisions {
		if revisions[i].Name == inUse {
			continue
		}
		if kept < configMapHistoryLimit {
			kept++
			continue
		}
		candidates = append(candidates, revisions[i])
	}
	return candidates
}

// liveReplicaSetConfigMaps returns the config ConfigMaps mounted by the
// ReplicaSets of the gateway's Deployment that have or want pods. A
// ReplicaSet scaled to zero keeps nothing alive: a gateway is rolled back
// through its CRs, and the operator reverts `kubectl rollout undo`. It reads
// through APIReader, so ReplicaSets are never cached cluster-wide.
func (r *KrakenDGatewayReconciler) liveReplicaSetConfigMaps(
	ctx context.Context, gw *v1alpha1.KrakenDGateway,
) (map[string]bool, error) {
	var list appsv1.ReplicaSetList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(gw.Namespace),
		client.MatchingLabels(resources.SelectorLabels(gw))); err != nil {
		return nil, fmt.Errorf("listing replicasets: %w", err)
	}
	mounted := map[string]bool{}
	for i := range list.Items {
		rs := &list.Items[i]
		owner := metav1.GetControllerOf(rs)
		if owner == nil || owner.Kind != "Deployment" || owner.Name != gw.Name {
			continue
		}
		if ptr.Deref(rs.Spec.Replicas, 1) == 0 && rs.Status.Replicas == 0 {
			continue
		}
		if name := resources.MountedConfigMapName(&rs.Spec.Template.Spec); name != "" {
			mounted[name] = true
		}
	}
	return mounted, nil
}

// neverApplied reports whether no config has ever been applied to gw. The
// cached gateway's empty checksum may predate the first apply, so an empty one
// is confirmed with an uncached read. When that read fails it reports false,
// the safe answer, with the error.
func (r *KrakenDGatewayReconciler) neverApplied(
	ctx context.Context, gw *v1alpha1.KrakenDGateway,
) (bool, error) {
	if gw.Status.ConfigChecksum != "" {
		return false, nil
	}
	var live v1alpha1.KrakenDGateway
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(gw), &live); err != nil {
		return false, fmt.Errorf("confirming gateway %s has no applied config: %w", client.ObjectKeyFromObject(gw), err)
	}
	return live.Status.ConfigChecksum == "", nil
}

// isExclusion reports whether cond leaves its endpoint out for failing
// validation on its own.
func isExclusion(cond *metav1.Condition) bool {
	return cond != nil && cond.Status == metav1.ConditionFalse &&
		(cond.Reason == v1alpha1.ReasonEndpointInvalid || cond.Reason == v1alpha1.ReasonPolicyInvalid)
}

// recordExclusions settles the endpoints' Accepted verdicts on a pass whose
// render is not the applied config:
//   - an endpoint that fails on its own (cfg.excluded) gets Accepted=False
//     with its reason, worded for a config not yet applied, keeping its live
//     status.conflicts;
//   - while no config has ever been applied (neverApplied), every other
//     endpoint loses any Accepted, for example one an earlier gateway of the
//     same name left: there is no applied render to keep;
//   - when every endpoint was judged (cfg.judged), one that now passes loses
//     an exclusion it still carries, so its Ready is derived afresh;
//   - every other endpoint keeps the verdict of the applied render.
//
// A removal is checked against the live condition, so a stale endpoint list
// cannot remove a verdict it did not see. writeEndpointAccepted writes only
// on change.
func (r *KrakenDGatewayReconciler) recordExclusions(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, endpoints []v1alpha1.KrakenDEndpoint,
	cfg configResult, neverApplied bool,
) error {
	var errs []error
	for i := range endpoints {
		ep := &endpoints[i]
		cur := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted)
		var (
			a         acceptance
			removable func(*metav1.Condition) bool
		)
		v, excluded := cfg.excluded[client.ObjectKeyFromObject(ep)]
		switch {
		case excluded:
			a = acceptance{condition: exclusionCondition(gw, ep, v, false), keepConflicts: true}
		case neverApplied && cur != nil:
			a = acceptance{}
		case cfg.judged && isExclusion(cur):
			a, removable = acceptance{keepConflicts: true}, isExclusion
		default:
			continue
		}
		if err := r.writeEndpointAccepted(ctx, ep, a, removable); err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

// renderVerdicts is what one render says about its endpoints.
type renderVerdicts struct {
	conflicted map[types.NamespacedName]struct{}
	lost       map[types.NamespacedName][]renderer.EntryConflict
	unresolved map[types.NamespacedName]struct{}
	stripped   map[types.NamespacedName][]renderer.StrippedEEFeature
	// excluded are the endpoints the render leaves out because they fail
	// validation on their own, with why.
	excluded map[types.NamespacedName]configcheck.EndpointVerdict
}

// newRenderVerdicts indexes output, and the endpoints left out of it for
// failing on their own, for per-endpoint lookups.
func newRenderVerdicts(output *renderer.RenderOutput,
	excluded map[types.NamespacedName]configcheck.EndpointVerdict) renderVerdicts {
	return renderVerdicts{
		conflicted: namespacedNameSet(output.ConflictedEndpoints),
		lost:       output.EntryConflicts,
		unresolved: namespacedNameSet(output.InvalidEndpoints),
		stripped:   strippedByEndpoint(output.StrippedEEFeatures),
		excluded:   excluded,
	}
}

// exclusionDetailLimit bounds the part of an exclusion's message that comes
// from the endpoint's verdict, leaving room for the rest within
// maxConditionMessageBytes.
const exclusionDetailLimit = maxConditionMessageBytes - 256

// exclusionCondition is the Accepted verdict on ep, which fails validation
// on its own (v). applied says the render that leaves ep out is the
// gateway's applied config, which serves the other endpoints; otherwise the
// gateway still serves its last applied config, and ep is left out when the
// gateway next applies one.
func exclusionCondition(gw *v1alpha1.KrakenDGateway, ep *v1alpha1.KrakenDEndpoint,
	v configcheck.EndpointVerdict, applied bool) *metav1.Condition {
	where := fmt.Sprintf("Not served by gateway %s/%s, which serves its other endpoints", gw.Namespace, gw.Name)
	if !applied {
		where = fmt.Sprintf("Will not be served when gateway %s/%s next applies its config", gw.Namespace, gw.Name)
	}
	return &metav1.Condition{
		Type:               v1alpha1.ConditionAccepted,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: ep.Generation,
		Reason:             v.Reason,
		Message:            truncateMessage(where + ": this endpoint " + v.Message(exclusionDetailLimit)),
	}
}

// strippedByEndpoint groups a CE-fallback render's removed features by the
// KrakenDEndpoint they came from. Gateway-level features are left out.
func strippedByEndpoint(features []renderer.StrippedEEFeature) map[types.NamespacedName][]renderer.StrippedEEFeature {
	out := map[types.NamespacedName][]renderer.StrippedEEFeature{}
	for _, f := range features {
		if f.Source != (types.NamespacedName{}) {
			out[f.Source] = append(out[f.Source], f)
		}
	}
	return out
}

// strippedList renders removed features for a condition message, one per
// line, so a truncated message keeps whole entries and counts the rest.
func strippedList(features []renderer.StrippedEEFeature) string {
	parts := make([]string, 0, len(features))
	for _, f := range features {
		parts = append(parts, f.String())
	}
	return strings.Join(parts, "\n")
}

// acceptance is the gateway's verdict on one endpoint: its Accepted condition
// (nil removes it) and the entries it does not serve.
type acceptance struct {
	condition *metav1.Condition
	conflicts []v1alpha1.EndpointConflict
	// keepConflicts leaves the live status.conflicts as it is and ignores
	// conflicts: a pass whose render is not the applied one has no verdict on them.
	keepConflicts bool
}

// entryCount is the number of distinct (endpoint, method) entries of ep.
func entryCount(ep *v1alpha1.KrakenDEndpoint) int {
	seen := map[[2]string]struct{}{}
	for _, e := range ep.Spec.Endpoints {
		seen[[2]string{e.Endpoint, e.Method}] = struct{}{}
	}
	return len(seen)
}

// endpointConflicts converts a render's lost entries to their status form;
// none gives nil.
func endpointConflicts(lost []renderer.EntryConflict) []v1alpha1.EndpointConflict {
	if len(lost) == 0 {
		return nil
	}
	out := make([]v1alpha1.EndpointConflict, 0, len(lost))
	for _, l := range lost {
		out = append(out, v1alpha1.EndpointConflict{Endpoint: l.Endpoint, Method: l.Method, Winner: l.Winner.String()})
	}
	return out
}

// configKey identifies a verdict: krakend check is deterministic for a
// rendered document and the edition it is checked as.
type configKey struct {
	checksum string
	edition  v1alpha1.Edition
}

// appliedKey is the key of the config the gateway serves. A status written
// before configEdition existed recorded no edition. That config was deployed
// with the image of the edition the gateway renders for now, so it counts as
// that. A render that differs between editions has a different checksum and
// is validated anew.
func appliedKey(gw *v1alpha1.KrakenDGateway, current v1alpha1.Edition) configKey {
	edition := gw.Status.ConfigEdition
	if edition == "" {
		edition = adoptedEdition(gw, current)
	}
	return configKey{checksum: gw.Status.ConfigChecksum, edition: edition}
}

// adoptedEdition is the edition a status without configEdition was validated
// for. An EE gateway's active image tells it when it names one edition only;
// otherwise (no image, a custom one, or the same for both) it is the edition
// the gateway renders for now.
func adoptedEdition(gw *v1alpha1.KrakenDGateway, current v1alpha1.Edition) v1alpha1.Edition {
	if gw.Spec.Edition != v1alpha1.EditionEE {
		return current
	}
	ee, ce := renderer.ResolveImage(gw, false), renderer.ResolveImage(gw, true)
	switch active := gw.Status.ActiveImage; {
	case ee == ce:
		return current
	case active == ee:
		return v1alpha1.EditionEE
	case active == ce:
		return v1alpha1.EditionCE
	}
	return current
}

// isApplied reports whether output, rendered for edition, is the gateway's
// applied config.
func isApplied(gw *v1alpha1.KrakenDGateway, output *renderer.RenderOutput, edition v1alpha1.Edition) bool {
	return appliedKey(gw, edition) == configKey{checksum: output.Checksum, edition: edition}
}

// appliedFallback reports whether the applied config is the CE-fallback
// render of an EE gateway. The image follows the applied config's edition,
// never the edition the license asks for next: CE pods must never load a
// config validated only as EE.
func appliedFallback(gw *v1alpha1.KrakenDGateway, current v1alpha1.Edition) bool {
	return gw.Spec.Edition == v1alpha1.EditionEE && appliedKey(gw, current).edition == v1alpha1.EditionCE
}

// appliedImage is the image the gateway runs. While the edition rendered now
// differs from the one the applied config was validated for (spec.edition was
// changed, or a license fallback started), that is the image deployed with the
// applied config: spec.image is read before the edition, so the spec cannot
// say which one it was, and version or image changes wait until a render is
// validated for the new edition. Otherwise it follows the spec.
func appliedImage(gw *v1alpha1.KrakenDGateway, current v1alpha1.Edition) string {
	if gw.Status.ActiveImage != "" && appliedKey(gw, current).edition != current {
		return gw.Status.ActiveImage
	}
	return renderer.ResolveImage(gw, appliedFallback(gw, current))
}

// openAPIFallbackNote is the CEFallbackApplied entry for spec.openapi: the CE
// binary has no openapi command, so a fallback Deployment runs without the
// OpenAPI export init container and the openapi-serve sidecar.
const openAPIFallbackNote = "gateway: spec.openapi export and the openapi-serve sidecar (until EE returns)"

// reconcileCEFallbackCondition reports the CE-fallback render the gateway
// serves and what it removed. It describes the applied config: while a newer
// render is not applied it is left as it was, and it is removed when the
// applied config is not a fallback render.
func (r *KrakenDGatewayReconciler) reconcileCEFallbackCondition(
	gw *v1alpha1.KrakenDGateway, output *renderer.RenderOutput, edition v1alpha1.Edition,
) {
	switch {
	case !appliedFallback(gw, edition):
		meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionCEFallbackApplied)
	case isApplied(gw, output, edition):
		list, n := strippedList(output.StrippedEEFeatures), len(output.StrippedEEFeatures)
		if gw.Spec.OpenAPI != nil && gw.Spec.OpenAPI.Enabled {
			// First, so truncation never cuts it.
			list, n = strings.TrimSuffix(openAPIFallbackNote+"\n"+list, "\n"), n+1
		}
		msg := "Running KrakenD CE in license fallback; the config uses no Enterprise-only features"
		if n > 0 {
			msg = fmt.Sprintf("Running KrakenD CE in license fallback; removed %d Enterprise-only feature(s):\n%s",
				n, list)
		}
		r.setProblemCondition(gw, metav1.Condition{
			Type:               v1alpha1.ConditionCEFallbackApplied,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonEEFeaturesStripped,
			Message:            truncateMessage(msg),
		})
	}
}

// setPluginsResolved reports whether every plugin ConfigMap the gateway
// mounts exists. The condition exists only while the gateway has ConfigMap
// plugin sources.
func (r *KrakenDGatewayReconciler) setPluginsResolved(gw *v1alpha1.KrakenDGateway, missing []string) {
	hasConfigMapSources := false
	if gw.Spec.Plugins != nil {
		for _, src := range gw.Spec.Plugins.Sources {
			hasConfigMapSources = hasConfigMapSources || src.ConfigMapRef != nil
		}
	}
	switch {
	case !hasConfigMapSources:
		meta.RemoveStatusCondition(&gw.Status.Conditions, v1alpha1.ConditionPluginsResolved)
	case len(missing) > 0:
		r.setConditionWithEvent(gw, metav1.Condition{
			Type:               v1alpha1.ConditionPluginsResolved,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonConfigMapNotFound,
			Message: fmt.Sprintf("plugin ConfigMap(s) %s not found in namespace %s; "+
				"the Deployment is held until they exist", strings.Join(missing, ", "), gw.Namespace),
		})
	default:
		r.setConditionWithEvent(gw, metav1.Condition{
			Type:               v1alpha1.ConditionPluginsResolved,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonConfigMapsFound,
			Message:            "every plugin ConfigMap exists",
		})
	}
}
