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

package license

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func generateTestCert(t *testing.T, cn string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func TestParse_ValidCertificate(t *testing.T) {
	expiry := time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second)
	pemData := generateTestCert(t, "KrakenD EE License", expiry)

	parser := NewX509LicenseParser()
	info, err := parser.Parse(pemData)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if info.Subject != "KrakenD EE License" {
		t.Errorf("Subject = %q, want %q", info.Subject, "KrakenD EE License")
	}
	if !info.NotAfter.Truncate(time.Second).Equal(expiry) {
		t.Errorf("NotAfter = %v, want %v", info.NotAfter.Truncate(time.Second), expiry)
	}
}

func TestParse_ExpiredCertificate(t *testing.T) {
	expiry := time.Now().Add(-24 * time.Hour)
	pemData := generateTestCert(t, "Expired License", expiry)

	parser := NewX509LicenseParser()
	info, err := parser.Parse(pemData)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if info.Subject != "Expired License" {
		t.Errorf("Subject = %q, want %q", info.Subject, "Expired License")
	}
	if !info.NotAfter.Before(time.Now()) {
		t.Error("Expected NotAfter to be in the past for expired cert")
	}
}

func TestParse_NoPEMBlock(t *testing.T) {
	parser := NewX509LicenseParser()
	_, err := parser.Parse([]byte("not a PEM block"))
	if err == nil {
		t.Error("Parse() expected error for non-PEM data, got nil")
	}
}

func TestParse_EmptyInput(t *testing.T) {
	parser := NewX509LicenseParser()
	_, err := parser.Parse([]byte{})
	if err == nil {
		t.Error("Parse() expected error for empty input, got nil")
	}
}

func TestParse_InvalidDER(t *testing.T) {
	invalidPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: []byte("not valid DER"),
	})

	parser := NewX509LicenseParser()
	_, err := parser.Parse(invalidPEM)
	if err == nil {
		t.Error("Parse() expected error for invalid DER, got nil")
	}
}

func TestParse_MultiplePEMBlocks(t *testing.T) {
	expiry := time.Now().Add(30 * 24 * time.Hour)
	pemData := generateTestCert(t, "First Cert", expiry)
	pemData = append(pemData, generateTestCert(t, "Second Cert", expiry)...)

	parser := NewX509LicenseParser()
	info, err := parser.Parse(pemData)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if info.Subject != "First Cert" {
		t.Errorf("Subject = %q, want %q (first PEM block)", info.Subject, "First Cert")
	}
}
