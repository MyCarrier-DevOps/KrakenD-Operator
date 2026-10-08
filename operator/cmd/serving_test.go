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
	"os"
	"path/filepath"
	"strings"
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

func TestMetricsServerOptions_SecureServingIsAuthenticated(t *testing.T) {
	secure := metricsServerOptions(":8443", true, "", "tls.crt", "tls.key", nil)
	if secure.FilterProvider == nil {
		t.Error("secure metrics have no authn/authz filter")
	}
	insecure := metricsServerOptions(":8080", false, "", "tls.crt", "tls.key", nil)
	if insecure.FilterProvider != nil || insecure.SecureServing {
		t.Error("insecure metrics must not carry the filter or serve TLS")
	}
	if insecure.CertDir != "" {
		t.Errorf("CertDir = %q, want none without a cert path", insecure.CertDir)
	}
}

func TestCheckServingFiles_MissingKeyIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := checkServingFiles(dir, "tls.crt", "tls.key")

	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "tls.key")) {
		t.Errorf("err = %v, want one naming the missing key file", err)
	}
}

func TestCheckServingFiles_BothPresentIsNil(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.crt", "a.key"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := checkServingFiles(dir, "a.crt", "a.key"); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestCheckServingFiles_MissingCertIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := checkServingFiles(dir, "tls.crt", "tls.key")

	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "tls.crt")) {
		t.Errorf("err = %v, want one naming the missing cert file", err)
	}
}
