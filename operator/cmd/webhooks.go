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

package main

import (
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
)

// registerWebhooks wires the admission webhooks into mgr when enabled, and
// makes the pod ready only once the webhook server accepts connections, so
// admission requests are never routed to a replica that cannot answer them.
// With webhooks disabled nothing asks mgr for its webhook server, so the
// server is never started and, as no certificate directory is handed to it,
// no serving certificate is read: the controllers run, protected only by
// render-time validation.
func registerWebhooks(mgr ctrl.Manager, enabled bool, setup func(ctrl.Manager) error) error {
	if !enabled {
		setupLog.Info("admission webhooks disabled; invalid objects are caught only at render time")
		return nil
	}
	if err := setup(mgr); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		return fmt.Errorf("adding webhook readiness check: %w", err)
	}
	return nil
}

// webhookCertWatchNeeded reports whether the certificate directory is handed
// to the webhook server, which then watches it. It is not with webhooks
// disabled, because a missing certificate or key would stop startup.
func webhookCertWatchNeeded(enabled bool, certPath string) bool {
	return enabled && certPath != ""
}

// defaultOperatorUsername is the Kubernetes username of the pod's
// ServiceAccount, built from the POD_NAMESPACE and POD_SERVICE_ACCOUNT
// variables the manifests set from the downward API, or "" when either is
// unset, which disables the AutoConfig write exemption.
func defaultOperatorUsername() string {
	ns, sa := os.Getenv("POD_NAMESPACE"), os.Getenv("POD_SERVICE_ACCOUNT")
	if ns == "" || sa == "" {
		return ""
	}
	return "system:serviceaccount:" + ns + ":" + sa
}
