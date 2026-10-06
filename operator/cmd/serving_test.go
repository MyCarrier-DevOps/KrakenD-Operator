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
	"testing"
)

// The server's own certificate watcher runs on every replica, while a watcher
// added to the manager runs only on the leader, so the options must hand the
// certificate location to the server and leave GetCertificate unset.
func TestWebhookServerOptions_CertPathGoesToServer(t *testing.T) {
	opts := webhookServerOptions(true, "/certs", "a.crt", "a.key", nil)

	if opts.CertDir != "/certs" || opts.CertName != "a.crt" || opts.KeyName != "a.key" {
		t.Errorf("cert location = %q %q %q, want /certs a.crt a.key", opts.CertDir, opts.CertName, opts.KeyName)
	}
	cfg := &tls.Config{}
	for _, o := range opts.TLSOpts {
		o(cfg)
	}
	if cfg.GetCertificate != nil {
		t.Error("TLSOpts set GetCertificate, which bypasses the server's own certificate watcher")
	}
}

func TestWebhookServerOptions_DisabledReadsNoCertificate(t *testing.T) {
	opts := webhookServerOptions(false, "/certs", "a.crt", "a.key", nil)

	if opts.CertDir != "" || opts.CertName != "" || opts.KeyName != "" {
		t.Errorf("cert location = %q %q %q, want none with webhooks disabled", opts.CertDir, opts.CertName, opts.KeyName)
	}
}

func TestMetricsServerOptions_CertPathGoesToServer(t *testing.T) {
	opts := metricsServerOptions(":8443", true, "/m", "m.crt", "m.key", nil)

	if opts.CertDir != "/m" || opts.CertName != "m.crt" || opts.KeyName != "m.key" {
		t.Errorf("cert location = %q %q %q, want /m m.crt m.key", opts.CertDir, opts.CertName, opts.KeyName)
	}
	cfg := &tls.Config{}
	for _, o := range opts.TLSOpts {
		o(cfg)
	}
	if cfg.GetCertificate != nil {
		t.Error("TLSOpts set GetCertificate, which bypasses the server's own certificate watcher")
	}
}
