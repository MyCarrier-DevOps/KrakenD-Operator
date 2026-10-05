//go:build integration

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

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// expectInvalid creates obj and requires a 422 whose message contains want.
func expectInvalid(t *testing.T, obj client.Object, want string) {
	t.Helper()
	err := k8sClient.Create(ctx, obj)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), want) {
		t.Errorf("create %s: err = %v, want Invalid containing %q", obj.GetName(), err, want)
	}
}

// The real API server runs these rules, so this proves the generated CRD is
// accepted and enforced. The test cluster runs Kubernetes 1.32, below the
// documented 1.33 floor; the rules used here do not depend on ratcheting, which
// (like optionalOldSelf) is beta and on by default in 1.32 and GA from 1.33.
func TestCRD_EndpointRules(t *testing.T) {
	ns := testNamespace(t)
	ep := func(name string, entries ...v1alpha1.EndpointEntry) *v1alpha1.KrakenDEndpoint {
		return &v1alpha1.KrakenDEndpoint{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}, Endpoints: entries},
		}
	}
	backends := []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}
	entry := func(path string) v1alpha1.EndpointEntry {
		return v1alpha1.EndpointEntry{Endpoint: path, Method: "GET", Backends: backends}
	}

	expectInvalid(t, ep("no-slash", entry("a/b")), "spec.endpoints[0].endpoint")
	// A nil list would marshal as null and fail as a missing value whatever
	// the minimum is, so send an explicitly empty one.
	expectInvalid(t, ep("empty", []v1alpha1.EndpointEntry{}...), "should have at least 1 items")
	expectInvalid(t, ep("dup", entry("/a"), entry("/a")), "Duplicate value")
	expectInvalid(t, ep("no-backends", v1alpha1.EndpointEntry{Endpoint: "/a", Method: "GET",
		Backends: []v1alpha1.BackendSpec{}}), "spec.endpoints[0].backends")
	// No Go duration can hold these, so send them as raw objects.
	for _, field := range []string{"timeout", "cacheTTL"} {
		overflowing := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.krakend.io/v1alpha1", "kind": "KrakenDEndpoint",
			"metadata": map[string]any{"name": "overflow-" + strings.ToLower(field), "namespace": ns},
			"spec": map[string]any{"gatewayRef": map[string]any{"name": "gw"}, "endpoints": []any{map[string]any{
				"endpoint": "/a", "method": "GET", field: "2562048h",
				"backends": []any{map[string]any{"host": []any{"http://svc"}, "urlPattern": "/"}},
			}}},
		}}
		expectInvalid(t, overflowing, "spec.endpoints[0]."+field)
	}
	if err := k8sClient.Create(ctx, ep("valid", entry("/a/{id}"), entry("/files/*"))); err != nil {
		t.Errorf("valid endpoint rejected: %v", err)
	}
}

func TestCRD_GatewayRules(t *testing.T) {
	ns := testNamespace(t)
	gw := func(name string, mutate func(*v1alpha1.KrakenDGatewaySpec)) *v1alpha1.KrakenDGateway {
		g := &v1alpha1.KrakenDGateway{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: v1alpha1.EditionCE},
		}
		mutate(&g.Spec)
		return g
	}
	secretKey := func(name string) *corev1.SecretKeySelector {
		return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "p"}
	}
	expectInvalid(t, gw("ee-unlicensed", func(s *v1alpha1.KrakenDGatewaySpec) { s.Edition = v1alpha1.EditionEE }),
		"edition EE requires")
	expectInvalid(t, gw("ee-unnamed-license", func(s *v1alpha1.KrakenDGatewaySpec) {
		s.Edition = v1alpha1.EditionEE
		s.License = &v1alpha1.LicenseConfig{SecretRef: secretKey("")}
	}), "edition EE requires")
	expectInvalid(t, gw("bad-timeout", func(s *v1alpha1.KrakenDGatewaySpec) { s.Config.Timeout = "3 seconds" }),
		"spec.config.timeout")
	// The rule hardcodes the defaults, so tie them to the ones the operator uses.
	defaults := &v1alpha1.KrakenDGateway{}
	expectInvalid(t, gw("port-clash", func(s *v1alpha1.KrakenDGatewaySpec) {
		s.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true, Port: resources.GatewayPort(defaults)}
	}), "openapi port must differ")
	expectInvalid(t, gw("port-clash-inverse", func(s *v1alpha1.KrakenDGatewaySpec) {
		s.Config.Port = resources.OpenAPIPort(defaults)
		s.OpenAPI = &v1alpha1.OpenAPIExportSpec{Enabled: true}
	}), "openapi port must differ")
	expectInvalid(t, gw("redis-password", func(s *v1alpha1.KrakenDGatewaySpec) {
		s.Redis = &v1alpha1.RedisSpec{ConnectionPool: v1alpha1.RedisConnectionPool{
			Addresses: []string{"redis:6379"}, Password: secretKey("s")}}
	}), "password is not supported yet")
	if err := k8sClient.Create(ctx, gw("valid", func(s *v1alpha1.KrakenDGatewaySpec) { s.Config.Timeout = "3s" })); err != nil {
		t.Errorf("valid gateway rejected: %v", err)
	}
}

// optionalOldSelf is beta and on by default in the 1.32 test cluster, and GA
// (locked on) from 1.33, the floor.
// A tmpSizeLimit whose exponent is beyond int32 takes the quantity parser
// seconds to evaluate; the API server must refuse it on its pattern first.
func TestCRD_GatewayRefusesAHugeQuantityExponentFast(t *testing.T) {
	ns := testNamespace(t)
	gw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "KrakenDGateway",
		"metadata":   map[string]any{"name": "huge-quantity", "namespace": ns},
		"spec": map[string]any{
			"version": "2.13", "edition": "CE", "config": map[string]any{},
			"postRestartJob": map[string]any{"enabled": true, "script": "x", "tmpSizeLimit": "1e2147483648"},
		},
	}}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := k8sClient.Create(callCtx, gw)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "tmpSizeLimit") {
		t.Errorf("Create() error = %v, want a 422 naming tmpSizeLimit within 5s", err)
	}
}

func TestCRD_GatewayDragonflyPasswordRatchets(t *testing.T) {
	ns := testNamespace(t)
	g := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "df-password", Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: v1alpha1.EditionCE,
			Dragonfly: &v1alpha1.DragonflySpec{Enabled: true, Authentication: &v1alpha1.DragonflyAuthSpec{
				PasswordFromSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "p"}}}},
	}
	if err := k8sClient.Create(ctx, g); err != nil {
		t.Fatalf("a Community gateway with a Dragonfly password was rejected: %v", err)
	}
	// Patch rather than Update: the gateway reconciler writes status between
	// the calls, which would fail an Update with a Conflict before validation.
	replicas := int32(3)
	base := g.DeepCopy()
	g.Spec.Replicas = &replicas
	if err := k8sClient.Patch(ctx, g, client.MergeFrom(base)); err != nil {
		t.Errorf("an unrelated edit of a Community gateway with a Dragonfly password was rejected: %v", err)
	}
	base = g.DeepCopy()
	g.Spec.Edition = v1alpha1.EditionEE
	g.Spec.License = &v1alpha1.LicenseConfig{SecretRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "l"}, Key: "k"}}
	err := k8sClient.Patch(ctx, g, client.MergeFrom(base))
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "passwordFromSecret is not supported yet") {
		t.Errorf("switch to Enterprise: err = %v, want Invalid containing the Dragonfly password message", err)
	}
}

func TestCRD_AutoConfigRules(t *testing.T) {
	ns := testNamespace(t)
	ac := func(name string, mutate func(*v1alpha1.KrakenDAutoConfigSpec)) *v1alpha1.KrakenDAutoConfig {
		a := &v1alpha1.KrakenDAutoConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: v1alpha1.KrakenDAutoConfigSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
				OpenAPI:    v1alpha1.OpenAPISource{URL: "http://svc/openapi.json"},
				Trigger:    v1alpha1.TriggerOnChange,
			},
		}
		mutate(&a.Spec)
		return a
	}
	expectInvalid(t, ac("fast-poll", func(s *v1alpha1.KrakenDAutoConfigSpec) {
		s.Trigger = v1alpha1.TriggerPeriodic
		s.Periodic = &v1alpha1.PeriodicSpec{Interval: metav1.Duration{Duration: 10 * time.Second}}
	}), "at least 30s")
	expectInvalid(t, ac(strings.Repeat("a", 64), func(*v1alpha1.KrakenDAutoConfigSpec) {}), "at most 63 characters")
	expectInvalid(t, ac("typo", func(s *v1alpha1.KrakenDAutoConfigSpec) {
		s.Defaults = &v1alpha1.Defaults{Endpoint: &v1alpha1.EndpointDefaults{OutputEncoding: "jsn"}}
	}), "Unsupported value: \"jsn\"")
	defaulted := ac("defaulted", func(s *v1alpha1.KrakenDAutoConfigSpec) {
		s.AdditionalEndpoints = []v1alpha1.AdditionalEndpoint{{Endpoint: "/health"}}
	})
	if err := k8sClient.Create(ctx, defaulted); err != nil {
		t.Fatalf("valid autoconfig rejected: %v", err)
	}
	var got v1alpha1.KrakenDAutoConfig
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(defaulted), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.AdditionalEndpoints[0].Method != "GET" {
		t.Errorf("additional endpoint method = %q, want the GET default", got.Spec.AdditionalEndpoints[0].Method)
	}
}
