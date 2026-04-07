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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/util/license"
)

const (
	defaultCheckInterval      = 5 * time.Minute
	defaultSafetyBuffer       = 1 * time.Hour
	defaultExpiryWarningDays  = 30
	warningRateLimitDuration  = 24 * time.Hour
	licenseCheckAnnotationKey = "gateway.krakend.io/license-check"
)

// LicenseMonitor periodically checks EE gateway license certificates and
// updates gateway status conditions accordingly. It runs as a manager.Runnable
// goroutine, not as a standard controller-runtime reconciler.
type LicenseMonitor struct {
	client.Client
	Recorder      record.EventRecorder
	Clock         clock.WithTicker
	LicenseParser license.LicenseParser
	CheckInterval time.Duration
	SafetyBuffer  time.Duration

	mu              sync.Mutex
	lastWarningSent map[types.NamespacedName]time.Time
}

// NewLicenseMonitor creates a LicenseMonitor with sensible defaults.
func NewLicenseMonitor(
	c client.Client,
	recorder record.EventRecorder,
	clk clock.WithTicker,
	parser license.LicenseParser,
) *LicenseMonitor {
	return &LicenseMonitor{
		Client:          c,
		Recorder:        recorder,
		Clock:           clk,
		LicenseParser:   parser,
		CheckInterval:   defaultCheckInterval,
		SafetyBuffer:    defaultSafetyBuffer,
		lastWarningSent: make(map[types.NamespacedName]time.Time),
	}
}

// Start implements manager.Runnable. It runs the periodic license check loop
// until the context is cancelled.
func (m *LicenseMonitor) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("license-monitor")
	log.Info("starting license monitor", "interval", m.CheckInterval)

	ticker := m.Clock.NewTicker(m.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("license monitor stopped")
			return nil
		case <-ticker.C():
			m.checkAll(ctx)
		}
	}
}

func (m *LicenseMonitor) checkAll(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("license-monitor")

	var gwList v1alpha1.KrakenDGatewayList
	if err := m.List(ctx, &gwList); err != nil {
		log.Error(err, "failed to list gateways")
		return
	}

	for i := range gwList.Items {
		gw := &gwList.Items[i]
		if gw.Spec.Edition != v1alpha1.EditionEE {
			continue
		}
		if err := m.checkGateway(ctx, gw); err != nil {
			log.Error(err, "license check failed", "gateway", gw.Name, "namespace", gw.Namespace)
		}
	}
}

func (m *LicenseMonitor) checkGateway(ctx context.Context, gw *v1alpha1.KrakenDGateway) error {
	log := logf.FromContext(ctx).WithName("license-monitor")

	secretData, err := m.readLicenseSecret(ctx, gw)
	if err != nil {
		return m.handleSecretUnavailable(ctx, gw, err)
	}

	info, err := m.LicenseParser.Parse(secretData)
	if err != nil {
		return m.handleSecretUnavailable(ctx, gw, err)
	}

	now := m.Clock.Now()
	licenseExpirySeconds.WithLabelValues(gw.Namespace, gw.Name).Set(info.NotAfter.Sub(now).Seconds())

	warningDays := defaultExpiryWarningDays
	if gw.Spec.License != nil && gw.Spec.License.ExpiryWarningDays > 0 {
		warningDays = gw.Spec.License.ExpiryWarningDays
	}
	warningThreshold := now.Add(time.Duration(warningDays) * 24 * time.Hour)

	key := types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}

	switch {
	case !info.NotAfter.After(now):
		// License expired
		return m.handleExpired(ctx, gw)

	case !info.NotAfter.After(now.Add(m.SafetyBuffer)):
		// License within safety buffer — treat as pre-expiry
		return m.handleExpired(ctx, gw)

	case !info.NotAfter.After(warningThreshold):
		// License expiring soon
		m.emitWarningRateLimited(gw, key, info.NotAfter)
		return m.handleRecoveryIfNeeded(ctx, gw)

	default:
		// License healthy
		log.V(1).Info("license healthy",
			"gateway", gw.Name,
			"expiry", info.NotAfter.Format(time.RFC3339))
		return m.handleRecoveryIfNeeded(ctx, gw)
	}
}

func (m *LicenseMonitor) readLicenseSecret(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
) ([]byte, error) {
	if gw.Spec.License == nil {
		return nil, errNoLicenseConfigured
	}

	var secretName, secretKey string

	switch {
	case gw.Spec.License.SecretRef != nil:
		secretName = gw.Spec.License.SecretRef.Name
		secretKey = gw.Spec.License.SecretRef.Key
	case gw.Spec.License.ExternalSecret.Enabled:
		// ExternalSecret convention: secret name = "{gateway}-license"
		secretName = gw.Name + "-license"
		secretKey = "LICENSE"
	default:
		return nil, errNoLicenseConfigured
	}

	if secretKey == "" {
		secretKey = "LICENSE"
	}

	var secret corev1.Secret
	if err := m.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: gw.Namespace,
	}, &secret); err != nil {
		return nil, err
	}

	data, ok := secret.Data[secretKey]
	if !ok {
		return nil, errLicenseKeyNotFound
	}
	return data, nil
}

func (m *LicenseMonitor) handleSecretUnavailable(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
	secretErr error,
) error {
	patch := client.MergeFrom(gw.DeepCopy())
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionLicenseSecretUnavailable,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonLicenseSecretMissing,
		Message:            secretErr.Error(),
		ObservedGeneration: gw.Generation,
	})
	if err := m.Status().Patch(ctx, gw, patch); err != nil {
		return err
	}
	m.Recorder.Event(gw, corev1.EventTypeWarning, v1alpha1.ReasonLicenseSecretMissing, secretErr.Error())
	return nil
}

func (m *LicenseMonitor) handleExpired(ctx context.Context, gw *v1alpha1.KrakenDGateway) error {
	fallbackToCE := gw.Spec.License != nil && gw.Spec.License.FallbackToCE

	patch := client.MergeFrom(gw.DeepCopy())
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionLicenseExpired,
		Status:             metav1.ConditionTrue,
		Reason:             "LicenseExpired",
		Message:            "license certificate has expired",
		ObservedGeneration: gw.Generation,
	})
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionLicenseValid,
		Status:             metav1.ConditionFalse,
		Reason:             "LicenseExpired",
		Message:            "license certificate has expired",
		ObservedGeneration: gw.Generation,
	})

	if fallbackToCE {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionLicenseDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             v1alpha1.ReasonLicenseFallbackCE,
			Message:            "license expired, falling back to CE edition",
			ObservedGeneration: gw.Generation,
		})
		gw.Status.Phase = v1alpha1.PhaseDegraded
	} else {
		gw.Status.Phase = v1alpha1.PhaseError
	}

	if err := m.Status().Patch(ctx, gw, patch); err != nil {
		return err
	}

	if fallbackToCE {
		m.Recorder.Event(gw, corev1.EventTypeWarning, v1alpha1.ReasonLicenseFallbackCE,
			"license expired, falling back to CE edition")
	} else {
		m.Recorder.Event(gw, corev1.EventTypeWarning, v1alpha1.ReasonLicenseExpiredNoFallback,
			"license expired and fallbackToCE is not enabled")
	}

	return m.triggerReconcile(ctx, gw)
}

func (m *LicenseMonitor) handleRecoveryIfNeeded(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
) error {
	degradedCond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded)
	expiredCond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionLicenseExpired)

	licenseCausedDegradation := (degradedCond != nil && degradedCond.Status == metav1.ConditionTrue) ||
		(expiredCond != nil && expiredCond.Status == metav1.ConditionTrue)

	if !licenseCausedDegradation {
		return nil
	}

	patch := client.MergeFrom(gw.DeepCopy())
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionLicenseDegraded,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonLicenseRestored,
		Message:            "license is now valid",
		ObservedGeneration: gw.Generation,
	})
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionLicenseExpired,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonLicenseRestored,
		Message:            "license is now valid",
		ObservedGeneration: gw.Generation,
	})
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionLicenseValid,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonLicenseRestored,
		Message:            "license is now valid",
		ObservedGeneration: gw.Generation,
	})

	if err := m.Status().Patch(ctx, gw, patch); err != nil {
		return err
	}

	m.Recorder.Event(gw, corev1.EventTypeNormal, v1alpha1.ReasonLicenseRestored,
		"license restored, recovering from degraded state")
	return m.triggerReconcile(ctx, gw)
}

func (m *LicenseMonitor) emitWarningRateLimited(
	gw *v1alpha1.KrakenDGateway,
	key types.NamespacedName,
	expiry time.Time,
) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.Clock.Now()
	if last, ok := m.lastWarningSent[key]; ok {
		if now.Sub(last) < warningRateLimitDuration {
			return
		}
	}
	m.lastWarningSent[key] = now
	m.Recorder.Eventf(gw, corev1.EventTypeWarning, v1alpha1.ReasonLicenseExpiringSoon,
		"license expires at %s", expiry.Format(time.RFC3339))
}

func (m *LicenseMonitor) triggerReconcile(
	ctx context.Context,
	gw *v1alpha1.KrakenDGateway,
) error {
	patch := client.MergeFrom(gw.DeepCopy())
	if gw.Annotations == nil {
		gw.Annotations = map[string]string{}
	}
	gw.Annotations[licenseCheckAnnotationKey] = m.Clock.Now().Format(time.RFC3339)
	return m.Patch(ctx, gw, patch)
}

// Sentinel errors for license secret lookup.
var (
	errNoLicenseConfigured = fmt.Errorf("no license configuration found")
	errLicenseKeyNotFound  = fmt.Errorf("license key not found in secret data")
)
