/*
Copyright 2026.

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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/util/license"
)

// mockLicenseParser implements license.LicenseParser for testing.
type mockLicenseParser struct {
	info *license.LicenseInfo
	err  error
}

func (p *mockLicenseParser) Parse(_ []byte) (*license.LicenseInfo, error) {
	return p.info, p.err
}

func newEEGateway(name, namespace string) *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13",
			Edition: v1alpha1.EditionEE,
			Config:  v1alpha1.GatewayConfig{},
			License: &v1alpha1.LicenseConfig{
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "my-license"},
					Key:                  "LICENSE",
				},
				FallbackToCE:      true,
				ExpiryWarningDays: 30,
			},
		},
	}
}

func newLicenseSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"LICENSE": []byte("fake-license-data"),
		},
	}
}

func TestLicenseMonitor_CheckAll_SkipsCE(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	ceGateway := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "ce-gw", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: v1alpha1.EditionCE, Config: v1alpha1.GatewayConfig{}},
	}

	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: now.Add(365 * 24 * time.Hour)},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(ceGateway).
		WithStatusSubresource(ceGateway).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())
	// CE gateway should be skipped — no errors, no status changes
}

func TestLicenseMonitor_HealthyLicense(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(365 * 24 * time.Hour) // 1 year out

	gw := newEEGateway("test-gw", "default")
	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	// Healthy license should only set LicenseSecretUnavailable=False
	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	unavailCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseSecretUnavailable)
	if unavailCond == nil || unavailCond.Status != metav1.ConditionFalse {
		t.Error("expected LicenseSecretUnavailable=False for healthy license")
	}
}

func TestLicenseMonitor_ExpiredWithFallback(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(-24 * time.Hour) // Expired yesterday

	gw := newEEGateway("test-gw", "default")
	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if updated.Status.Phase != v1alpha1.PhaseDegraded {
		t.Errorf("expected phase Degraded, got %s", updated.Status.Phase)
	}

	degraded := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Error("expected LicenseDegraded=True")
	}

	expired := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseExpired)
	if expired == nil || expired.Status != metav1.ConditionTrue {
		t.Error("expected LicenseExpired=True")
	}

	// Should have the reconcile annotation
	if updated.Annotations == nil || updated.Annotations[licenseCheckAnnotationKey] == "" {
		t.Error("expected license-check annotation to be set")
	}
}

func TestLicenseMonitor_ExpiredNoFallback(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(-24 * time.Hour)

	gw := newEEGateway("test-gw", "default")
	gw.Spec.License.FallbackToCE = false
	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if updated.Status.Phase != v1alpha1.PhaseError {
		t.Errorf("expected phase Error, got %s", updated.Status.Phase)
	}
}

func TestLicenseMonitor_WithinSafetyBuffer(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Expiry within safety buffer (1 hour) — should be treated like expired
	expiry := now.Add(30 * time.Minute)

	gw := newEEGateway("test-gw", "default")
	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Within safety buffer = expired path
	if updated.Status.Phase != v1alpha1.PhaseDegraded {
		t.Errorf("expected phase Degraded (safety buffer), got %s", updated.Status.Phase)
	}
}

func TestLicenseMonitor_ExpiringSoon_Warning(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Expiry within warning days (30) but outside safety buffer
	expiry := now.Add(15 * 24 * time.Hour) // 15 days

	gw := newEEGateway("test-gw", "default")
	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)
	rec := fakeRecorder()

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        rec,
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	// Should have emitted a warning event
	select {
	case event := <-rec.Events:
		if event == "" {
			t.Error("expected non-empty event")
		}
	default:
		t.Error("expected warning event for expiring license")
	}
}

func TestLicenseMonitor_WarningRateLimited(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(15 * 24 * time.Hour)

	gw := newEEGateway("test-gw", "default")
	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)
	rec := fakeRecorder()

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        rec,
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	// First check — should emit
	monitor.checkAll(context.Background())
	select {
	case <-rec.Events:
		// expected
	default:
		t.Fatal("expected first warning event")
	}

	// Second check (same time) — rate limited
	monitor.checkAll(context.Background())
	select {
	case <-rec.Events:
		t.Error("expected rate-limited second event to be suppressed")
	default:
		// expected: no event
	}

	// Advance 25 hours — should emit again
	fakeClock.SetTime(now.Add(25 * time.Hour))
	monitor.checkAll(context.Background())
	select {
	case <-rec.Events:
		// expected after rate limit window
	default:
		t.Error("expected warning event after rate limit window")
	}
}

func TestLicenseMonitor_SecretMissing(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	gw := newEEGateway("test-gw", "default")
	// No secret created
	parser := &mockLicenseParser{}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseSecretUnavailable)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Error("expected LicenseSecretUnavailable=True")
	}
}

func TestLicenseMonitor_ParseError(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	gw := newEEGateway("test-gw", "default")
	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		err: fmt.Errorf("invalid PEM"),
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseSecretUnavailable)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Error("expected LicenseSecretUnavailable=True for parse error")
	}
}

func TestLicenseMonitor_Recovery(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(365 * 24 * time.Hour) // Healthy

	gw := newEEGateway("test-gw", "default")
	// Simulate previously degraded state
	gw.Status.Phase = v1alpha1.PhaseDegraded
	gw.Status.Conditions = []metav1.Condition{
		{
			Type:   v1alpha1.ConditionLicenseDegraded,
			Status: metav1.ConditionTrue,
			Reason: v1alpha1.ReasonLicenseFallbackCE,
		},
		{
			Type:   v1alpha1.ConditionLicenseExpired,
			Status: metav1.ConditionTrue,
			Reason: "LicenseExpired",
		},
	}

	secret := newLicenseSecret("my-license", "default")
	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	degraded := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Error("expected LicenseDegraded=False after recovery")
	}

	valid := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseValid)
	if valid == nil || valid.Status != metav1.ConditionTrue {
		t.Error("expected LicenseValid=True after recovery")
	}

	// Should have the reconcile annotation to trigger gateway controller
	if updated.Annotations == nil || updated.Annotations[licenseCheckAnnotationKey] == "" {
		t.Error("expected license-check annotation after recovery")
	}
}

func TestLicenseMonitor_ExternalSecretConvention(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(365 * 24 * time.Hour)

	gw := newEEGateway("test-gw", "default")
	// Use ExternalSecret convention instead of SecretRef
	gw.Spec.License.SecretRef = nil
	gw.Spec.License.ExternalSecret = v1alpha1.ExternalSecretLicenseConfig{
		Enabled: true,
	}

	// ExternalSecret convention: secret named "{gw-name}-license"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-license",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"LICENSE": []byte("fake-license-data"),
		},
	}

	parser := &mockLicenseParser{
		info: &license.LicenseInfo{NotAfter: expiry, Subject: "test"},
	}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw, secret).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	// Should work fine with external secret convention
	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	unavailCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseSecretUnavailable)
	if unavailCond == nil || unavailCond.Status != metav1.ConditionFalse {
		t.Error("expected LicenseSecretUnavailable=False for healthy external secret")
	}
}

func TestLicenseMonitor_NoLicenseConfig(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	gw := newEEGateway("test-gw", "default")
	gw.Spec.License = nil // No license configured
	parser := &mockLicenseParser{}

	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).
		WithObjects(gw).
		WithStatusSubresource(gw).
		Build()
	fakeClock := clocktesting.NewFakeClock(now)

	monitor := &LicenseMonitor{
		Client:          c,
		Recorder:        fakeRecorder(),
		Clock:           fakeClock,
		LicenseParser:   parser,
		CheckInterval:   5 * time.Minute,
		SafetyBuffer:    1 * time.Hour,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}

	monitor.checkAll(context.Background())

	var updated v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gw", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha1.ConditionLicenseSecretUnavailable)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Error("expected LicenseSecretUnavailable=True when no license configured")
	}
}

func TestNewLicenseMonitor(t *testing.T) {
	scheme := testScheme()
	c := fakeClientBuilder().WithScheme(scheme).Build()
	fakeClock := clocktesting.NewFakeClock(time.Now())
	parser := &mockLicenseParser{}

	monitor := NewLicenseMonitor(c, fakeRecorder(), fakeClock, parser)

	if monitor.CheckInterval != defaultCheckInterval {
		t.Errorf("expected default check interval %v, got %v", defaultCheckInterval, monitor.CheckInterval)
	}
	if monitor.SafetyBuffer != defaultSafetyBuffer {
		t.Errorf("expected default safety buffer %v, got %v", defaultSafetyBuffer, monitor.SafetyBuffer)
	}
	if monitor.lastWarningSent == nil {
		t.Error("expected lastWarningSent map to be initialized")
	}
}
