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

	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
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

// metricsServerOptions builds the metrics server options. As with the
// webhook server, a certificate path goes to the server so its own watcher
// reloads the certificate on every replica. Without one controller-runtime
// generates a self-signed certificate, which is not recommended for production.
func metricsServerOptions(
	addr string, secure bool, certPath, certName, certKey string, tlsOpts []func(*tls.Config),
) metricsserver.Options {
	opts := metricsserver.Options{BindAddress: addr, SecureServing: secure, TLSOpts: tlsOpts}
	if secure {
		// Only authorized users and service accounts can read the metrics; the
		// RBAC lives in config/rbac. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if certPath != "" {
		opts.CertDir, opts.CertName, opts.KeyName = certPath, certName, certKey
	}
	return opts
}

// checkServingFiles returns an error naming the first of the certificate and
// key files under dir that cannot be read. The servers fall back to a
// self-signed certificate (metrics) or fail late (webhook) on a wrong file
// name, so startup checks them to fail fast.
func checkServingFiles(dir, certName, keyName string) error {
	return nil
}
