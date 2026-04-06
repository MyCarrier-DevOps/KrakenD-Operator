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

// Package license provides X.509 license certificate parsing for KrakenD EE.
package license

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"
)

// LicenseParser parses KrakenD EE license certificates.
type LicenseParser interface {
	Parse(data []byte) (*LicenseInfo, error)
}

// LicenseInfo holds the parsed license certificate metadata.
type LicenseInfo struct {
	NotAfter time.Time
	Subject  string
}

type x509LicenseParser struct{}

// NewX509LicenseParser returns a LicenseParser that parses PEM-encoded X.509 certificates.
func NewX509LicenseParser() LicenseParser {
	return &x509LicenseParser{}
}

func (p *x509LicenseParser) Parse(data []byte) (*LicenseInfo, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in license data")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing X.509 certificate: %w", err)
	}
	return &LicenseInfo{
		NotAfter: cert.NotAfter,
		Subject:  cert.Subject.CommonName,
	}, nil
}
