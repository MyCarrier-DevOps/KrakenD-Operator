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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
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
		// The cache has not seen the ConfigMap that is already there: it may
		// be this controller's own earlier create or someone else's. Verify
		// it live rather than trust it.
		_, err := r.verifyExistingConfigMap(ctx, r.APIReader, gw, checksum)
		return err
	default:
		return fmt.Errorf("creating configmap %s: %w", name, err)
	}
}

// verifyExistingConfigMap looks up the config ConfigMap for checksum through
// reader. found says whether one exists; when it does, err says whether it is
// this gateway's copy of that config.
func (r *KrakenDGatewayReconciler) verifyExistingConfigMap(
	ctx context.Context, reader client.Reader, gw *v1alpha1.KrakenDGateway, checksum string,
) (found bool, err error) {
	name := resources.ConfigMapName(gw, checksum)
	var cm corev1.ConfigMap
	if err := reader.Get(ctx, types.NamespacedName{Name: name, Namespace: gw.Namespace}, &cm); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting configmap %s: %w", name, err)
	}
	return true, verifyConfigMap(&cm, gw, checksum)
}

// verifyConfigMap rejects a ConfigMap that has the content-addressed name for
// checksum but is not this gateway's copy of that config.
func verifyConfigMap(cm *corev1.ConfigMap, gw *v1alpha1.KrakenDGateway, checksum string) error {
	if !metav1.IsControlledBy(cm, gw) {
		return fmt.Errorf("configmap %s/%s exists but is not controlled by gateway %s", cm.Namespace, cm.Name, gw.Name)
	}
	if got := cm.Annotations[resources.PostRestartJobChecksumAnnotation]; got != checksum {
		return fmt.Errorf("configmap %s/%s holds config %s, not %s", cm.Namespace, cm.Name, got, checksum)
	}
	return nil
}

// configMapHistoryLimit is how many of a gateway's config revisions GC keeps
// even when nothing mounts them.
const configMapHistoryLimit = 3

// collectConfigMaps deletes the gateway's config ConfigMaps that nothing can
// still mount. A ConfigMap is kept when any of these holds:
//   - it is inUse (the applied config, which the Deployment template mounts);
//   - it is in the revision history (configMapGCCandidates);
//   - a live ReplicaSet of the gateway's Deployment mounts it.
//
// The ConfigMap an earlier operator version kept under the gateway's own name
// is collected on the same terms, but never counts toward the history.
func (r *KrakenDGatewayReconciler) collectConfigMaps(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, inUse string,
) error {
	var list corev1.ConfigMapList
	if err := r.List(ctx, &list, client.InNamespace(gw.Namespace),
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
		uid := cm.UID
		if err := r.Delete(ctx, cm, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting configmap %s: %w", cm.Name, err)
		}
	}
	return nil
}

// configMapGCCandidates returns the gateway's config ConfigMaps that only a
// live ReplicaSet could still keep: every controlled one except inUse and the
// revision history.
func configMapGCCandidates(gw *v1alpha1.KrakenDGateway, cms []corev1.ConfigMap, inUse string) []corev1.ConfigMap {
	var revisions, candidates []corev1.ConfigMap
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
	slices.SortFunc(revisions, func(a, b corev1.ConfigMap) int {
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
