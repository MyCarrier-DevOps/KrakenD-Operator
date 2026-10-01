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
	ctrl "sigs.k8s.io/controller-runtime"
)

// registerWebhooks wires the admission webhooks into mgr when enabled.
// With webhooks disabled nothing asks mgr for its webhook server, so the
// server is never started and no serving certificate is read: the
// controllers run, protected only by render-time validation.
func registerWebhooks(mgr ctrl.Manager, enabled bool, setup func(ctrl.Manager) error) error {
	if !enabled {
		setupLog.Info("admission webhooks disabled; invalid objects are caught only at render time")
		return nil
	}
	return setup(mgr)
}

// webhookCertWatchNeeded reports whether the webhook certificate watcher
// should be created.
func webhookCertWatchNeeded(_ bool, certPath string) bool {
	return certPath != ""
}
