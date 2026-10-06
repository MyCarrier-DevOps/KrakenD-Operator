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
	"crypto/tls"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// webhookServerOptions builds the webhook server options. The certificate
// location goes to the server rather than into a certificate watcher added to
// the manager: the server starts its own watcher on every replica, while a
// manager-added watcher runs only on the leader and a standby would keep the
// certificate it read at startup. With webhooks disabled no certificate is read.
func webhookServerOptions(
	enabled bool, certPath, certName, certKey string, tlsOpts []func(*tls.Config),
) webhook.Options {
	opts := webhook.Options{TLSOpts: tlsOpts}
	if webhookCertWatchNeeded(enabled, certPath) {
		opts.CertDir, opts.CertName, opts.KeyName = certPath, certName, certKey
	}
	return opts
}
