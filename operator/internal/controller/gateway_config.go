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
	"sort"
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
	// rejections are the endpoints krakend check blamed for the current
	// render, with their findings.
	rejections map[types.NamespacedName]string
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
	// A ConfigMap that is not this gateway's copy is not served: the
	// Deployment is held instead.
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
// reader, as metadata only: the rendered config can be large and its content
// is addressed by the name. found says whether one exists; when it does, err
// says whether it is this gateway's copy of that config. The checksum
// annotation it reads is stripped from the cached metadata, so this read must
// stay live (client.CacheOptions.DisableFor).
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
	return true, verifyConfigMap(cm, gw, checksum)
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

// rejectionsByEndpoint groups krakend check findings by the KrakenDEndpoint
// they name. Findings that name no endpoint are reported on the gateway only.
// One message that blames several entries of an endpoint is one line naming
// all of them.
func rejectionsByEndpoint(findings []configcheck.Finding) map[types.NamespacedName]string {
	type blame struct {
		message string
		entries []int
	}
	blames := map[types.NamespacedName][]*blame{}
	for _, f := range findings {
		if f.Endpoint == (types.NamespacedName{}) {
			continue
		}
		list := blames[f.Endpoint]
		i := slices.IndexFunc(list, func(b *blame) bool { return b.message == f.Message })
		if i < 0 {
			list = append(list, &blame{message: f.Message})
			blames[f.Endpoint] = list
			i = len(list) - 1
		}
		if f.Index >= 0 && !slices.Contains(list[i].entries, f.Index) {
			list[i].entries = append(list[i].entries, f.Index)
		}
	}
	out := make(map[types.NamespacedName]string, len(blames))
	for nn, list := range blames {
		lines := make([]string, 0, len(list))
		for _, b := range list {
			slices.Sort(b.entries)
			where := make([]string, 0, len(b.entries))
			for _, i := range b.entries {
				where = append(where, fmt.Sprintf("spec.endpoints[%d]", i))
			}
			if len(where) > 0 {
				lines = append(lines, strings.Join(where, ", ")+": "+b.message)
			} else {
				lines = append(lines, b.message)
			}
		}
		out[nn] = truncateMessage("The gateway's newest config was rejected by krakend check and not applied; " +
			"findings naming this endpoint: " + strings.Join(lines, "; "))
	}
	return out
}

// rejectionMessage is ConfigValid's message on rejection: the summary, then
// one line per finding, so truncateMessage keeps whole findings.
func rejectionMessage(findings []configcheck.Finding) string {
	lines := make([]string, 0, len(findings)+1)
	lines = append(lines, rejectionSummary(findings))
	for _, f := range findings {
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n")
}

// rejectionSummary is the first line of ConfigValid's message on rejection. It
// names the KrakenDEndpoints krakend check blamed.
func rejectionSummary(findings []configcheck.Finding) string {
	seen := map[types.NamespacedName]bool{}
	var names []string
	unattributed := 0
	for _, f := range findings {
		switch {
		case f.Endpoint == (types.NamespacedName{}):
			unattributed++
		case !seen[f.Endpoint]:
			seen[f.Endpoint] = true
			names = append(names, f.Endpoint.String())
		}
	}
	sort.Strings(names)
	switch {
	case len(names) == 0:
		return "Rejected by krakend check; no finding names a KrakenDEndpoint (gateway settings, plugins or policies)."
	case unattributed == 0:
		return fmt.Sprintf("Rejected by krakend check; findings name KrakenDEndpoint(s) %s.", strings.Join(names, ", "))
	default:
		return fmt.Sprintf(
			"Rejected by krakend check; findings name KrakenDEndpoint(s) %s; %d finding(s) name no endpoint.",
			strings.Join(names, ", "), unattributed)
	}
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

// isConfigRejected reports whether cond is a GatewayConfigRejected verdict.
func isConfigRejected(cond *metav1.Condition) bool {
	return cond != nil && cond.Reason == v1alpha1.ReasonGatewayConfigRejected
}

// recordRejections settles the endpoints' Accepted verdicts on a pass whose
// render is not the applied config. An endpoint the current findings name gets
// Accepted=False/GatewayConfigRejected. An endpoint carrying
// GatewayConfigRejected that no finding names any longer has it removed, so
// its Ready is derived afresh. Every other endpoint keeps the verdict of the
// applied render, unless no config has ever been applied (neverApplied): then
// there is no applied render to keep, and any Accepted left by an earlier
// gateway of the same name is removed. A removal is checked against the live
// condition, so a stale endpoint list cannot remove a verdict it did not see.
// writeEndpointAccepted writes only on change, so a remembered rejection
// writes nothing.
func (r *KrakenDGatewayReconciler) recordRejections(
	ctx context.Context,
	endpoints []v1alpha1.KrakenDEndpoint,
	rejections map[types.NamespacedName]string,
	neverApplied bool,
) error {
	removable := isConfigRejected
	if neverApplied {
		removable = nil
	}
	var errs []error
	for i := range endpoints {
		ep := &endpoints[i]
		var want *metav1.Condition
		if msg, ok := rejections[client.ObjectKeyFromObject(ep)]; ok {
			want = &metav1.Condition{
				Type:               v1alpha1.ConditionAccepted,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: ep.Generation,
				Reason:             v1alpha1.ReasonGatewayConfigRejected,
				Message:            msg,
			}
		} else if cur := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted); cur == nil ||
			(!neverApplied && !isConfigRejected(cur)) {
			continue
		}
		// Without an applied render there are no served conflicts to keep.
		a := acceptance{condition: want, keepConflicts: !neverApplied}
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
}

// newRenderVerdicts indexes output for per-endpoint lookups.
func newRenderVerdicts(output *renderer.RenderOutput) renderVerdicts {
	return renderVerdicts{
		conflicted: namespacedNameSet(output.ConflictedEndpoints),
		lost:       output.EntryConflicts,
		unresolved: namespacedNameSet(output.InvalidEndpoints),
		stripped:   strippedByEndpoint(output.StrippedEEFeatures),
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
