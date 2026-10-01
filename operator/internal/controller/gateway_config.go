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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
}

// publishApplied republishes the applied config. Publishing is idempotent,
// so this also restores a config ConfigMap deleted out of band.
func (r *KrakenDGatewayReconciler) publishApplied(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, output *renderer.RenderOutput,
) (configResult, error) {
	if err := r.publishConfig(ctx, gw, output.JSON, output.Checksum); err != nil {
		return configResult{}, err
	}
	return configResult{appliedConfigMap: resources.ConfigMapName(gw, output.Checksum)}, nil
}

// keepApplied is the outcome of a pass that applies nothing new: the applied
// config stays, served from whichever ConfigMap still holds it.
func (r *KrakenDGatewayReconciler) keepApplied(
	ctx context.Context, gw *v1alpha1.KrakenDGateway, verdictErr error,
) (configResult, error) {
	name, err := r.appliedConfigMapName(ctx, gw)
	return configResult{appliedConfigMap: name}, stderrors.Join(verdictErr, err)
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
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: gw.Namespace}, &cm)
	if err == nil {
		return name, nil
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("getting configmap %s: %w", name, err)
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
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: gw.Namespace}, &existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("getting configmap %s: %w", name, err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gw.Namespace}}
	resources.BuildConfigMap(cm, gw, jsonData, checksum)
	if err := controllerutil.SetControllerReference(gw, cm, r.Scheme); err != nil {
		return fmt.Errorf("setting owner on configmap %s: %w", name, err)
	}
	// AlreadyExists is a cache that has not seen this controller's own
	// earlier create; the next pass verifies what is there.
	if err := r.Create(ctx, cm); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating configmap %s: %w", name, err)
	}
	return nil
}
