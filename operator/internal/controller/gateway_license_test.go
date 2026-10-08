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
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
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

func TestGatewayReconcile_RenewedLicenseRestoresEE(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(90*24*time.Hour), true)
	for _, typ := range []string{v1alpha1.ConditionLicenseDegraded, v1alpha1.ConditionLicenseExpired} {
		meta.SetStatusCondition(&gw.Status.Conditions,
			metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "LicenseExpired"})
	}
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	var captured *renderer.RenderInput
	r := newTestGatewayReconciler(c,
		&capturingRenderer{delegate: renderOutput("cs"), captured: &captured}, &mockValidator{})
	r.LicenseParser = parser

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if captured.CEFallback {
		t.Error("a renewed license must render EE again") // pinned by StageValid returning false
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseDegraded)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonLicenseRestored {
		t.Errorf("LicenseDegraded = %+v, want False/%s", cond, v1alpha1.ReasonLicenseRestored) // pinned by recoverLicense
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseRestored); n != 1 {
		t.Errorf("LicenseRestored events over two reconciles = %d, want 1", n)
	}
}

func TestGatewayReconcile_MissingLicenseSecretKeepsTheFallbackDecision(t *testing.T) {
	gw, _, parser := licensedEEGateway(testNow, true) // the Secret is not created
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionLicenseDegraded, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonLicenseFallbackCE,
	})
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	var captured *renderer.RenderInput
	r := newTestGatewayReconciler(c,
		&capturingRenderer{delegate: renderOutput("cs"), captured: &captured}, &mockValidator{})
	r.LicenseParser = parser

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if !captured.CEFallback {
		t.Error("with the license unknown, the gateway must keep its last fallback decision")
	}
	if !meta.IsStatusConditionTrue(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseSecretUnavailable) {
		t.Error("LicenseSecretUnavailable must be True")
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseSecretMissing); n != 1 {
		t.Errorf("LicenseSecretMissing events over two reconciles = %d, want 1", n)
	}
}

func TestGatewayReconcile_ExpiringSoonWarnsOnceWhenEnteringTheWindow(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(10*24*time.Hour), true)
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser

	for range 3 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseValid)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonLicenseExpiringSoon {
		t.Errorf("LicenseValid = %+v, want True/%s", cond, v1alpha1.ReasonLicenseExpiringSoon)
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseExpiringSoon); n != 1 {
		t.Errorf("LicenseExpiringSoon events over three reconciles = %d, want 1", n)
	}
}

func TestGatewayReconcile_ExpiredLicenseWithoutFallbackSaysSoOnce(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), false)
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	var captured *renderer.RenderInput
	r := newTestGatewayReconciler(c,
		&capturingRenderer{delegate: renderOutput("cs"), captured: &captured}, &mockValidator{})
	r.LicenseParser = parser

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if captured.CEFallback {
		t.Error("without fallbackToCE the gateway must keep rendering EE")
	}
	got := getGateway(t, c, gw)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionLicenseExpired) {
		t.Error("LicenseExpired must be True")
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseExpiredNoFallback); n != 1 {
		t.Errorf("LicenseExpiredNoFallback events over two reconciles = %d, want 1", n)
	}
}

func TestGatewayReconcile_TerminatingLicensedGatewayGetsNoLicenseSeries(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(90*24*time.Hour), true)
	gw.Namespace = "terminating-licensed"
	secret.Namespace = gw.Namespace
	gw.Finalizers = []string{"test.krakend.io/hold"}
	gw.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser
	m, reg := testMetrics(t)
	r.Metrics = m

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := gatewaySeriesCount(t, reg, gw.Namespace, gw.Name); n != 0 {
		t.Errorf("%d metric series for a terminating licensed gateway, want 0", n)
	}
}

func TestNewGatewayRateLimiter_CapsBackoffAtTheLicenseRecheckInterval(t *testing.T) {
	limiter := newGatewayRateLimiter()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-gw"}}
	if first := limiter.When(req); first != 5*time.Millisecond {
		t.Fatalf("first delay = %v, want 5ms", first)
	}
	var delay time.Duration
	for range 30 {
		delay = limiter.When(req)
	}
	if delay != licenseRecheckInterval {
		t.Fatalf("delay after 30 failures = %v, want the %v cap", delay, licenseRecheckInterval)
	}
}

func TestGatewayReconcile_PreExpiryLicenseFallsBackToCE(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(30*time.Minute), true)
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	var captured *renderer.RenderInput
	r := newTestGatewayReconciler(c,
		&capturingRenderer{delegate: renderOutput("cs"), captured: &captured}, &mockValidator{})
	r.LicenseParser = parser

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if !captured.CEFallback {
		t.Error("a license inside the safety buffer must render the CE fallback")
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonLicensePreExpiry {
		t.Errorf("LicenseValid = %+v, want False/%s", cond, v1alpha1.ReasonLicensePreExpiry)
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseFallbackCE); n != 1 {
		t.Errorf("LicenseFallbackCE events over two reconciles = %d, want 1", n)
	}
}

func TestGatewayReconcile_ReadsTheExternalSecretConventionSecret(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(90*24*time.Hour), false)
	gw.Spec.License.SecretRef = nil
	gw.Spec.License.ExternalSecret.Enabled = true
	secret.Name = gw.Name + "-license"
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseValid)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonLicenseOK {
		t.Errorf("LicenseValid = %+v, want True/%s", cond, v1alpha1.ReasonLicenseOK)
	}
}

func TestGatewayReconcile_HealthyLicenseIsValid(t *testing.T) {
	notAfter := testNow.Add(90 * 24 * time.Hour)
	gw, secret, parser := licensedEEGateway(notAfter, true)
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionLicenseValid)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonLicenseOK {
		t.Errorf("LicenseValid = %+v, want True/%s", cond, v1alpha1.ReasonLicenseOK)
	}
	if got.Status.LicenseExpiry == nil || !got.Status.LicenseExpiry.Time.Equal(notAfter) {
		t.Errorf("licenseExpiry = %v, want %v", got.Status.LicenseExpiry, notAfter)
	}
}

func TestGatewayReconcile_UnusableLicenseIsReportedAsUnavailable(t *testing.T) {
	cases := map[string]func(gw *v1alpha1.KrakenDGateway, p *mockLicenseParser){
		"the certificate does not parse": func(_ *v1alpha1.KrakenDGateway, p *mockLicenseParser) {
			p.info, p.err = nil, errors.New("not a certificate")
		},
		"no license is configured": func(gw *v1alpha1.KrakenDGateway, _ *mockLicenseParser) { gw.Spec.License = nil },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			gw, secret, parser := licensedEEGateway(testNow.Add(90*24*time.Hour), false)
			spoil(gw, parser)
			c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
			r.LicenseParser = parser

			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseSecretUnavailable)
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonLicenseSecretMissing {
				t.Errorf("LicenseSecretUnavailable = %+v, want True/%s", cond, v1alpha1.ReasonLicenseSecretMissing)
			}
			if res.RequeueAfter != licenseRecheckInterval {
				t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, licenseRecheckInterval)
			}
		})
	}
}

func TestGatewayReconcile_CommunityGatewayHasNoLicenseRequeue(t *testing.T) {
	gw := reconciledGateway()
	gw.Spec.Edition = v1alpha1.EditionCE
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want 0 for a CE gateway", res.RequeueAfter)
	}
}

func TestGatewayReconcile_CommunityGatewayDropsItsStaleLicenseState(t *testing.T) {
	gw := reconciledGateway()
	gw.Namespace = "ce-switched"
	gw.Spec.Edition = v1alpha1.EditionCE
	gw.Status.LicenseExpiry = &metav1.Time{Time: testNow.Add(-time.Hour)}
	for _, typ := range []string{
		v1alpha1.ConditionLicenseValid, v1alpha1.ConditionLicenseExpired,
		v1alpha1.ConditionLicenseDegraded, v1alpha1.ConditionLicenseSecretUnavailable,
	} {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type: typ, Status: metav1.ConditionTrue, Reason: "LicenseExpired",
		})
	}
	m, reg := testMetrics(t)
	m.SetLicenseExpiry(client.ObjectKeyFromObject(gw), -time.Hour)
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.Metrics = m

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := getGateway(t, c, gw)
	for _, cond := range got.Status.Conditions {
		if strings.HasPrefix(cond.Type, "License") {
			t.Errorf("CE gateway still has condition %s=%s", cond.Type, cond.Status)
		}
	}
	if got.Status.LicenseExpiry != nil {
		t.Errorf("licenseExpiry = %v, want nil", got.Status.LicenseExpiry)
	}
	if got.Status.Phase == v1alpha1.PhaseError {
		t.Errorf("phase = %s, a stale LicenseExpired must not keep a CE gateway in Error", got.Status.Phase)
	}
	if _, ok := metricValue(t, reg, "krakend_operator_license_expiry_seconds",
		"namespace", gw.Namespace, "name", gw.Name); ok {
		t.Error("the license_expiry_seconds series of a CE gateway must be removed")
	}
}

func TestGatewayReconcile_DisablingFallbackOnAnExpiredLicenseSaysSoOnce(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	stored := getGateway(t, c, gw)
	stored.Spec.License.FallbackToCE = false
	if err := c.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseExpiredNoFallback); n != 1 {
		t.Errorf("LicenseExpiredNoFallback events after disabling the fallback = %d, want 1", n)
	}
}

func TestGatewayReconcile_UnreadableLicenseMakesLicenseValidUnknown(t *testing.T) {
	gw, _, parser := licensedEEGateway(testNow, false) // the Secret is not created
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionLicenseValid, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonLicenseOK,
	})
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseValid)
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != v1alpha1.ReasonLicenseSecretMissing {
		t.Errorf("LicenseValid = %+v, want Unknown/%s", cond, v1alpha1.ReasonLicenseSecretMissing)
	}
}

func TestGatewayReconcile_UnreadableLicenseStillFallsBackAtTheSafetyBuffer(t *testing.T) {
	gw, _, parser := licensedEEGateway(testNow, true) // the Secret is not created
	gw.Status.LicenseExpiry = &metav1.Time{Time: testNow.Add(30 * time.Minute)}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	var captured *renderer.RenderInput
	r := newTestGatewayReconciler(c,
		&capturingRenderer{delegate: renderOutput("cs"), captured: &captured}, &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !captured.CEFallback {
		t.Error("a last known expiry inside the safety buffer must render the CE fallback")
	}
	if !meta.IsStatusConditionTrue(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseDegraded) {
		t.Error("LicenseDegraded must be True while falling back")
	}
}

func TestGatewayReconcile_UnreadableLicenseInsideTheBufferDoesNotFlipLicenseValid(t *testing.T) {
	gw, _, parser := licensedEEGateway(testNow, true) // the Secret is not created
	gw.Status.LicenseExpiry = &metav1.Time{Time: testNow.Add(30 * time.Minute)}
	seeded := metav1.NewTime(testNow.Add(-time.Hour))
	gw.Status.Conditions = []metav1.Condition{{
		Type: v1alpha1.ConditionLicenseValid, Status: metav1.ConditionFalse,
		Reason: v1alpha1.ReasonLicensePreExpiry, LastTransitionTime: seeded,
	}}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser

	for range 2 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseValid)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonLicensePreExpiry {
		t.Fatalf("LicenseValid = %+v, want False/%s", cond, v1alpha1.ReasonLicensePreExpiry)
	}
	if !cond.LastTransitionTime.Time.Equal(seeded.Time) {
		t.Errorf("LastTransitionTime = %v, want it unchanged at %v", cond.LastTransitionTime, seeded)
	}
}

func TestGatewayReconcile_UnreadableExpiredLicenseHonoursDisabledFallback(t *testing.T) {
	gw, _, parser := licensedEEGateway(testNow, false) // the Secret is not created
	gw.Status.LicenseExpiry = &metav1.Time{Time: testNow.Add(-time.Hour)}
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionLicenseDegraded, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonLicenseFallbackCE,
	})
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	var captured *renderer.RenderInput
	r := newTestGatewayReconciler(c,
		&capturingRenderer{delegate: renderOutput("cs"), captured: &captured}, &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if captured.CEFallback {
		t.Error("with fallbackToCE off, the pass that records LicenseDegraded=False must not render CE")
	}
	cond := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionLicenseDegraded)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("LicenseDegraded = %+v, want False", cond)
	}
}

func TestGatewayReconcile_UnreadableLicenseWatchesTheKnownExpiry(t *testing.T) {
	gw, _, parser := licensedEEGateway(testNow, true) // the Secret is not created
	known := testNow.Add(licenseSafetyBuffer + 2*time.Minute)
	gw.Status.LicenseExpiry = &metav1.Time{Time: known}
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser
	m, reg := testMetrics(t)
	r.Metrics = m

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 2*time.Minute {
		t.Errorf("RequeueAfter = %s, want the 2m until the known expiry enters the safety buffer", res.RequeueAfter)
	}
	got, _ := metricValue(t, reg, "krakend_operator_license_expiry_seconds", "namespace", gw.Namespace, "name", gw.Name)
	if want := known.Sub(testNow).Seconds(); got != want {
		t.Errorf("license_expiry_seconds = %v, want %v from the known expiry", got, want)
	}
}

func TestGatewayReconcile_UnreadableLicenseWithoutKnownExpiryDropsTheGauge(t *testing.T) {
	gw, _, parser := licensedEEGateway(testNow, true) // the Secret is not created
	m, reg := testMetrics(t)
	m.SetLicenseExpiry(client.ObjectKeyFromObject(gw), 42*time.Second)
	c := fakeClientBuilder().WithObjects(gw).WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser
	r.Metrics = m

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := metricValue(t, reg, "krakend_operator_license_expiry_seconds",
		"namespace", gw.Namespace, "name", gw.Name); ok {
		t.Error("a license that was never read must leave no license_expiry_seconds series")
	}
}

// settledLicensedGateway is an EE gateway with a valid license, reconciled
// until its Deployment carries the license in the Secret and has finished
// rolling out, together with the pieces a test needs to change the license.
type settledLicensedGateway struct {
	c      client.Client
	r      *KrakenDGatewayReconciler
	gw     *v1alpha1.KrakenDGateway
	secret *corev1.Secret
	parser *mockLicenseParser
	// cached, while set, is what every read of the Deployment returns, as an
	// informer cache does until it has seen the controller's own update.
	cached *appsv1.Deployment
}

func settleLicensedGateway(t *testing.T, tweaks ...func(gw *v1alpha1.KrakenDGateway)) *settledLicensedGateway {
	t.Helper()
	gw, secret, parser := licensedEEGateway(testNow.Add(90*24*time.Hour), false)
	for _, tweak := range tweaks {
		tweak(gw)
	}
	s := &settledLicensedGateway{gw: gw, secret: secret, parser: parser}
	s.c = fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).
		WithInterceptorFuncs(withGenerationBumps(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if dep, ok := obj.(*appsv1.Deployment); ok && s.cached != nil {
					s.cached.DeepCopyInto(dep)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})).Build()
	c := s.c
	s.r = newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	s.r.LicenseParser = parser
	r := s.r
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var dep appsv1.Deployment
	getObject(t, c, gw, gw.Name, &dep)
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
	}
	if err := c.Status().Update(context.Background(), &dep); err != nil {
		t.Fatal(err)
	}
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return s
}

// reconcileWhileCacheLags reconciles with every Deployment read returning
// the Deployment as it stands now, so the rollout the pass starts does not
// look finished.
func (s *settledLicensedGateway) reconcileWhileCacheLags(t *testing.T) {
	t.Helper()
	s.cached = s.deployment(t)
	defer func() { s.cached = nil }()
	if err := reconcileGateway(t, s.r, s.gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// setLicenseBytes replaces the license in the Secret.
func (s *settledLicensedGateway) setLicenseBytes(t *testing.T, data string) {
	t.Helper()
	var stored corev1.Secret
	getObject(t, s.c, s.gw, s.secret.Name, &stored)
	stored.Data["LICENSE"] = []byte(data)
	if err := s.c.Update(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
}

// deployment returns the gateway Deployment as stored.
func (s *settledLicensedGateway) deployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	getObject(t, s.c, s.gw, s.gw.Name, &dep)
	return &dep
}

func TestGatewayReconcile_RenewedLicenseBytesRollTheDeployment(t *testing.T) {
	s := settleLicensedGateway(t)
	s.setLicenseBytes(t, "renewed certificate")

	s.reconcileWhileCacheLags(t)
	want := hash.SHA256Hex([]byte("renewed certificate"))
	if got := s.deployment(t).Spec.Template.Annotations[resources.LicenseChecksumAnnotation]; got != want {
		t.Errorf("license annotation = %q, want %q", got, want)
	}
	stored := getGateway(t, s.c, s.gw)
	progressing := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue || progressing.Reason != "DeploymentUpdated" {
		t.Errorf("Progressing = %+v, want True/DeploymentUpdated", progressing)
	}
	if meta.IsStatusConditionTrue(stored.Status.Conditions, v1alpha1.ConditionReady) {
		t.Error("Ready must not be True while the pods roll to the renewed license")
	}
}

func TestGatewayReconcile_UnchangedLicenseBytesRollNothing(t *testing.T) {
	s := settleLicensedGateway(t)
	before := s.deployment(t)
	m, reg := testMetrics(t)
	s.r.Metrics = m

	s.reconcileWhileCacheLags(t)

	if after := s.deployment(t); after.ResourceVersion != before.ResourceVersion {
		t.Errorf("Deployment resourceVersion %s -> %s; unchanged license bytes must write nothing",
			before.ResourceVersion, after.ResourceVersion)
	}
	stored := getGateway(t, s.c, s.gw)
	if progressing := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionProgressing); progressing == nil ||
		progressing.Reason == "DeploymentUpdated" {
		t.Errorf("Progressing = %+v, want no DeploymentUpdated for an unchanged license", progressing)
	}
	if got, _ := metricValue(t, reg, "krakend_operator_rolling_restarts_total"); got != 0 {
		t.Errorf("rollingRestarts rose to %v for unchanged license bytes", got)
	}
}

func TestGatewayReconcile_UnreadableLicenseKeepsTheDeployedChecksum(t *testing.T) {
	s := settleLicensedGateway(t)
	deployed := s.deployment(t)
	want := deployed.Spec.Template.Annotations[resources.LicenseChecksumAnnotation]
	if want == "" {
		t.Fatal("the settled Deployment carries no license checksum")
	}
	if err := s.c.Delete(context.Background(), s.secret); err != nil {
		t.Fatal(err)
	}

	s.reconcileWhileCacheLags(t)

	after := s.deployment(t)
	if got := after.Spec.Template.Annotations[resources.LicenseChecksumAnnotation]; got != want {
		t.Errorf("license annotation = %q, want the deployed %q kept", got, want)
	}
	if after.ResourceVersion != deployed.ResourceVersion {
		t.Error("an unreadable license must not touch the Deployment")
	}
	stored := getGateway(t, s.c, s.gw)
	if cond := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionLicenseValid); cond == nil ||
		cond.Status != metav1.ConditionUnknown {
		t.Errorf("LicenseValid = %+v, want Unknown", cond)
	}
	if progressing := meta.FindStatusCondition(stored.Status.Conditions, v1alpha1.ConditionProgressing); progressing == nil ||
		progressing.Reason == "DeploymentUpdated" {
		t.Errorf("Progressing = %+v, want no DeploymentUpdated", progressing)
	}
}

func TestGatewayReconcile_RenewedLicenseDoesNotRerunThePostRestartJob(t *testing.T) {
	s := settleLicensedGateway(t, func(gw *v1alpha1.KrakenDGateway) {
		gw.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo done"}
	})
	var jobs batchv1.JobList
	if err := s.c.List(context.Background(), &jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("jobs after settling = %d (%v), want the one post-restart Job", len(jobs.Items), err)
	}
	ranFor := getGateway(t, s.c, s.gw).Status.LastPostRestartJobChecksum
	s.setLicenseBytes(t, "renewed certificate")

	s.reconcileWhileCacheLags(t)
	if err := reconcileGateway(t, s.r, s.gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if err := s.c.List(context.Background(), &jobs); err != nil || len(jobs.Items) != 1 {
		t.Errorf("jobs after the license changed = %d (%v), want still 1", len(jobs.Items), err)
	}
	if got := getGateway(t, s.c, s.gw).Status.LastPostRestartJobChecksum; got != ranFor {
		t.Errorf("lastPostRestartJobChecksum = %q, want %q: a license change must not re-run the Job", got, ranFor)
	}
}

func TestGatewayReconcile_LicenseChecksumFollowsTheMountedLicense(t *testing.T) {
	cases := map[string]struct {
		tweak   func(gw *v1alpha1.KrakenDGateway, p *mockLicenseParser)
		present bool
	}{
		"CE fallback still mounts the license": {
			tweak:   func(_ *v1alpha1.KrakenDGateway, p *mockLicenseParser) { p.info.NotAfter = testNow.Add(-time.Minute) },
			present: true,
		},
		"community edition mounts none": {
			tweak:   func(gw *v1alpha1.KrakenDGateway, _ *mockLicenseParser) { gw.Spec.Edition = v1alpha1.EditionCE },
			present: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gw, secret, parser := licensedEEGateway(testNow.Add(90*24*time.Hour), true)
			tc.tweak(gw, parser)
			c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
			r.LicenseParser = parser

			if err := reconcileGateway(t, r, gw); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var dep appsv1.Deployment
			getObject(t, c, gw, gw.Name, &dep)
			want := ""
			if tc.present {
				want = hash.SHA256Hex(secret.Data["LICENSE"])
			}
			got, ok := dep.Spec.Template.Annotations[resources.LicenseChecksumAnnotation]
			if ok != tc.present || got != want {
				t.Errorf("license annotation = %q (present %v), want %q (present %v)", got, ok, want, tc.present)
			}
		})
	}
}

func TestGatewayReconcile_FallbackToggleKeepsTheLicenseChecksum(t *testing.T) {
	s := settleLicensedGateway(t, func(gw *v1alpha1.KrakenDGateway) { gw.Spec.License.FallbackToCE = true })
	before := s.deployment(t).Spec.Template.Annotations[resources.LicenseChecksumAnnotation]
	if before == "" {
		t.Fatal("the settled Deployment carries no license checksum")
	}
	s.parser.info.NotAfter = testNow.Add(30 * time.Minute) // inside the safety buffer: the fallback starts

	s.reconcileWhileCacheLags(t)

	if got := s.deployment(t).Spec.Template.Annotations[resources.LicenseChecksumAnnotation]; got != before {
		t.Errorf("license annotation = %q across the fallback, want it unchanged at %q", got, before)
	}
	if !meta.IsStatusConditionTrue(getGateway(t, s.c, s.gw).Status.Conditions, v1alpha1.ConditionLicenseDegraded) {
		t.Error("the fallback did not start, so the test proves nothing")
	}
}

func TestGatewayReconcile_FailedDeploymentReadFiresNoLicenseEvents(t *testing.T) {
	gw, secret, parser := licensedEEGateway(testNow.Add(-time.Minute), true)
	c := fakeClientBuilder().WithObjects(gw, secret).WithStatusSubresource(gw).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return errors.New("cache not synced")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.LicenseParser = parser

	if err := reconcileGateway(t, r, gw); err == nil {
		t.Fatal("a failed Deployment read must fail the reconcile")
	}
	if n := eventsWithReason(r.Recorder.(*record.FakeRecorder), v1alpha1.ReasonLicenseFallbackCE); n != 0 {
		t.Errorf("LicenseFallbackCE events = %d, want 0: the status that records the transition is not written", n)
	}
}
