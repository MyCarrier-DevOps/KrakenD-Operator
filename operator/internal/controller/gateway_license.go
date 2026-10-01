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
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/util/license"
)

const (
	// licenseRecheckInterval bounds how long an EE gateway goes without a
	// license check when no stage boundary or Secret change is due (P1).
	licenseRecheckInterval = 5 * time.Minute
	// licenseSafetyBuffer is how long before NotAfter the gateway already
	// treats its license as expired: EE processes stop at expiry, so a CE
	// fallback must have rolled out by then.
	licenseSafetyBuffer      = time.Hour
	defaultExpiryWarningDays = 30
)

// license.Window requires its warning to be longer than its safety buffer.
// The shortest warning this file builds is one day (see expiryWarning), so
// this constant stops compiling if the buffer ever reaches that length.
const _ = uint64(24*time.Hour - licenseSafetyBuffer - 1)

// Sentinel errors for license Secret lookup.
var (
	errNoLicenseConfigured = fmt.Errorf("no license configuration found")
	errLicenseKeyNotFound  = fmt.Errorf("license key not found in secret data")
)

// licenseVerdict is what the rest of the reconcile needs from the license.
type licenseVerdict struct {
	// ceFallback: render and run CE instead of EE.
	ceFallback bool
	// requeueAfter is when to look at the license again; 0 for a gateway
	// without an EE license.
	requeueAfter time.Duration
}

// reconcileLicense evaluates an EE gateway's license certificate. It is the
// only writer of the License* conditions, status.licenseExpiry and the
// license expiry metric. Secret changes arrive through the Secret watch, and
// time-driven changes through requeueAfter.
func (r *KrakenDGatewayReconciler) reconcileLicense(ctx context.Context, gw *v1alpha1.KrakenDGateway) licenseVerdict {
	if gw.Spec.Edition != v1alpha1.EditionEE {
		forgetLicense(gw)
		return licenseVerdict{}
	}
	notAfter, err := r.readLicense(ctx, gw)
	if err != nil {
		r.setProblemCondition(gw, metav1.Condition{
			Type:               v1alpha1.ConditionLicenseSecretUnavailable,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gw.Generation,
			Reason:             v1alpha1.ReasonLicenseSecretMissing,
			Message:            err.Error(),
		})
		// The license is unknown: keep the fallback decision recorded last.
		return licenseVerdict{
			ceFallback:   meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded),
			requeueAfter: licenseRecheckInterval,
		}
	}
	now := r.Clock.Now()
	gw.Status.LicenseExpiry = &metav1.Time{Time: notAfter}
	licenseExpirySeconds.WithLabelValues(gw.Namespace, gw.Name).Set(notAfter.Sub(now).Seconds())
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionLicenseSecretUnavailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gw.Generation,
		Reason:             "SecretAvailable",
		Message:            "license secret is available",
	})
	window := license.Window{Warning: expiryWarning(gw), SafetyBuffer: licenseSafetyBuffer}
	return licenseVerdict{
		ceFallback:   r.applyLicenseStage(gw, window.StageAt(notAfter, now), notAfter),
		requeueAfter: nextLicenseCheck(window, notAfter, now),
	}
}

// forgetLicense drops the license state of a gateway that is not EE (for
// instance one switched from EE to CE), so a stale LicenseExpired cannot keep
// it out of Ready.
func forgetLicense(gw *v1alpha1.KrakenDGateway) {
	gw.Status.Conditions = slices.DeleteFunc(gw.Status.Conditions, func(c metav1.Condition) bool {
		switch c.Type {
		case v1alpha1.ConditionLicenseValid, v1alpha1.ConditionLicenseExpired,
			v1alpha1.ConditionLicenseDegraded, v1alpha1.ConditionLicenseSecretUnavailable:
			return true
		}
		return false
	})
	gw.Status.LicenseExpiry = nil
	licenseExpirySeconds.DeleteLabelValues(gw.Namespace, gw.Name)
}

// applyLicenseStage sets LicenseValid, LicenseExpired and LicenseDegraded for
// stage, and reports whether the gateway must fall back to CE.
func (r *KrakenDGatewayReconciler) applyLicenseStage(
	gw *v1alpha1.KrakenDGateway, stage license.Stage, notAfter time.Time,
) bool {
	expiry := notAfter.UTC().Format(time.RFC3339)
	fallback := gw.Spec.License != nil && gw.Spec.License.FallbackToCE
	switch stage {
	case license.StageValid:
		r.recoverLicense(gw)
		r.setLicenseValid(gw, metav1.ConditionTrue, v1alpha1.ReasonLicenseOK, "license valid until "+expiry)
		return false
	case license.StageExpiringSoon:
		r.recoverLicense(gw)
		prev := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionLicenseValid)
		entering := prev == nil || prev.Reason != v1alpha1.ReasonLicenseExpiringSoon
		message := "license expires at " + expiry
		r.setLicenseValid(gw, metav1.ConditionTrue, v1alpha1.ReasonLicenseExpiringSoon, message)
		if entering {
			r.Recorder.Event(gw, corev1.EventTypeWarning, v1alpha1.ReasonLicenseExpiringSoon, message)
		}
		return false
	case license.StagePreExpiry:
		r.expireLicense(gw, v1alpha1.ReasonLicensePreExpiry,
			fmt.Sprintf("license expires at %s, inside the %s safety buffer", expiry, licenseSafetyBuffer), fallback)
		return fallback
	case license.StageExpired:
		r.expireLicense(gw, v1alpha1.ReasonLicenseExpired, "license certificate expired at "+expiry, fallback)
		return fallback
	}
	return false
}

func (r *KrakenDGatewayReconciler) setLicenseValid(
	gw *v1alpha1.KrakenDGateway, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionLicenseValid, Status: status, ObservedGeneration: gw.Generation,
		Reason: reason, Message: message,
	})
}

// expireLicense records an expired (or pre-expiry) license, and the CE
// fallback when fallback is set.
func (r *KrakenDGatewayReconciler) expireLicense(gw *v1alpha1.KrakenDGateway, reason, message string, fallback bool) {
	wasExpired := meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseExpired)
	r.setLicenseValid(gw, metav1.ConditionFalse, reason, message)
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type: v1alpha1.ConditionLicenseExpired, Status: metav1.ConditionTrue, ObservedGeneration: gw.Generation,
		Reason: reason, Message: message,
	})
	switch {
	case fallback:
		r.setProblemCondition(gw, metav1.Condition{
			Type: v1alpha1.ConditionLicenseDegraded, Status: metav1.ConditionTrue, ObservedGeneration: gw.Generation,
			Reason: v1alpha1.ReasonLicenseFallbackCE, Message: "license expired, falling back to CE edition",
		})
	case !wasExpired:
		r.Recorder.Event(gw, corev1.EventTypeWarning, v1alpha1.ReasonLicenseExpiredNoFallback,
			"license expired and fallbackToCE is not enabled")
	}
	if !fallback && meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded) != nil {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionLicenseDegraded, Status: metav1.ConditionFalse, ObservedGeneration: gw.Generation,
			Reason: v1alpha1.ReasonLicenseExpiredNoFallback, Message: "license expired and fallbackToCE is not enabled",
		})
	}
}

// recoverLicense clears an earlier expiry. As before, it acts only when
// LicenseExpired or LicenseDegraded is True.
func (r *KrakenDGatewayReconciler) recoverLicense(gw *v1alpha1.KrakenDGateway) {
	if !meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseExpired) &&
		!meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionLicenseDegraded) {
		return
	}
	for _, t := range []string{v1alpha1.ConditionLicenseDegraded, v1alpha1.ConditionLicenseExpired} {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
			Type: t, Status: metav1.ConditionFalse, ObservedGeneration: gw.Generation,
			Reason: v1alpha1.ReasonLicenseRestored, Message: "license is now valid",
		})
	}
	r.Recorder.Event(gw, corev1.EventTypeNormal, v1alpha1.ReasonLicenseRestored,
		"license restored, recovering from degraded state")
}

// readLicense returns the NotAfter of the gateway's license certificate.
func (r *KrakenDGatewayReconciler) readLicense(ctx context.Context, gw *v1alpha1.KrakenDGateway) (time.Time, error) {
	data, err := r.readLicenseSecret(ctx, gw)
	if err != nil {
		return time.Time{}, err
	}
	info, err := r.LicenseParser.Parse(data)
	if err != nil {
		return time.Time{}, err
	}
	return info.NotAfter, nil
}

// readLicenseSecret returns the license data the gateway's spec points at.
func (r *KrakenDGatewayReconciler) readLicenseSecret(ctx context.Context, gw *v1alpha1.KrakenDGateway) ([]byte, error) {
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
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: gw.Namespace}, &secret); err != nil {
		return nil, err
	}
	data, ok := secret.Data[secretKey]
	if !ok {
		return nil, errLicenseKeyNotFound
	}
	return data, nil
}

// nextLicenseCheck is when to look at the license again: at its next stage
// boundary, but at least every licenseRecheckInterval. NextChange returns 0
// once the license has expired, which means no boundary is left, not "now".
func nextLicenseCheck(w license.Window, notAfter, now time.Time) time.Duration {
	if d := w.NextChange(notAfter, now); d > 0 && d < licenseRecheckInterval {
		return d
	}
	return licenseRecheckInterval
}

// expiryWarning is how long before expiry the license counts as expiring
// soon.
func expiryWarning(gw *v1alpha1.KrakenDGateway) time.Duration {
	days := defaultExpiryWarningDays
	if gw.Spec.License != nil && gw.Spec.License.ExpiryWarningDays > 0 {
		days = gw.Spec.License.ExpiryWarningDays
	}
	return time.Duration(days) * 24 * time.Hour
}
