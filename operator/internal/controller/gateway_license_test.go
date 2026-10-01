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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/util/license"
)

// mockLicenseParser returns info, or err, for any license data.
type mockLicenseParser struct {
	info *license.LicenseInfo
	err  error
}

func (p *mockLicenseParser) Parse(_ []byte) (*license.LicenseInfo, error) {
	return p.info, p.err
}

// licensedEEGateway is an EE gateway whose license Secret holds a
// certificate that the returned parser reads as expiring at notAfter.
func licensedEEGateway(notAfter time.Time, fallback bool) (*v1alpha1.KrakenDGateway, *corev1.Secret, *mockLicenseParser) {
	gw := reconciledGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.License = &v1alpha1.LicenseConfig{
		SecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "license"}, Key: "LICENSE",
		},
		FallbackToCE: fallback,
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "license", Namespace: gw.Namespace},
		Data:       map[string][]byte{"LICENSE": []byte("certificate")},
	}
	return gw, secret, &mockLicenseParser{info: &license.LicenseInfo{NotAfter: notAfter}}
}

func TestGatewayReconcile_ExpiredLicenseFallsBackToCEAndSaysSoOnce(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	gatewayWrites := 0
	countGateway := func(obj client.Object) {
		if _, ok := obj.(*v1alpha1.KrakenDGateway); ok {
			gatewayWrites++
		}
	}
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch,
				opts ...client.PatchOption) error {
				countGateway(obj)
				return cl.Patch(ctx, obj, p, opts...)
			},
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				countGateway(obj)
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	var captured *renderer.RenderInput
	r := newTestGatewayReconciler(c,
		&capturingRenderer{delegate: renderOutput("cs"), captured: &captured}, &mockValidator{})
	r.LicenseParser = parser

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if captured == nil || !captured.CEFallback {
		t.Fatal("an expired license with fallbackToCE must render the CE fallback")
	}
	if !meta.IsStatusConditionTrue(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseDegraded) {
		t.Error("LicenseDegraded must be True while falling back")
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseFallbackCE); n != 1 {
		t.Errorf("LicenseFallbackCE events over two reconciles = %d, want 1", n)
	}
	if gatewayWrites != 0 {
		t.Errorf("writes to the gateway object (not its status) = %d, want 0", gatewayWrites)
	}
}

func TestGatewayReconcile_RequeuesAtTheNextLicenseBoundary(t *testing.T) {
	cases := []struct {
		name     string
		notAfter time.Time
		want     time.Duration
	}{
		{"boundary before the recheck interval", testNow.Add(time.Hour + 2*time.Minute), 2 * time.Minute},
		{"boundary after the recheck interval", testNow.Add(40 * 24 * time.Hour), licenseRecheckInterval},
		{"already expired", testNow.Add(-time.Hour), licenseRecheckInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw, secret, parser := licensedEEGateway(tc.notAfter, true)
			c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
			r.LicenseParser = parser

			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if res.RequeueAfter != tc.want {
				t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, tc.want)
			}
		})
	}
}
