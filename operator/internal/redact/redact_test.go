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

package redact_test

import (
	"net/url"
	"testing"

	"github.com/mycarrier-devops/krakend-operator/internal/redact"
)

// URL's contract: what it keeps, what it replaces and what it drops.
func TestURL(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"a plain URL is unchanged", "https://example.com/spec.json", "https://example.com/spec.json"},
		{"userinfo is dropped", "https://user:pw@example.com/spec.json", "https://example.com/spec.json"},
		{
			"each query value is replaced and the keys kept",
			"https://example.com/s.json?token=abc&key=def", "https://example.com/s.json?key=REDACTED&token=REDACTED",
		},
		{"the fragment is dropped", "https://example.com/s.json#/Pet", "https://example.com/s.json"},
		{"an unparseable URL is reduced", "http://host:badport/x?sig=S", "<unparseable URL>"},
		{"a bare query key is replaced", "https://example.com/s.json?SECRETKEY", "https://example.com/s.json?REDACTED"},
		{"a semicolon separates pairs too", "https://example.com/s.json?a=1;b=2", "https://example.com/s.json?a=REDACTED&b=REDACTED"},
		{"a bare key before a semicolon", "https://example.com/s.json?SECRET;a=1", "https://example.com/s.json?REDACTED&a=REDACTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redact.URL(tc.raw)

			if got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if got != "<unparseable URL>" {
				if _, err := url.Parse(got); err != nil {
					t.Errorf("URL(%q) = %q does not parse: %v", tc.raw, got, err)
				}
			}
		})
	}
}

// Parsed gives what URL gives for the same URL.
func TestParsed_MatchesURL(t *testing.T) {
	raw := "https://user:pw@example.com/s.json?token=abc&key=def#frag"
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := redact.Parsed(u), redact.URL(raw); got != want {
		t.Errorf("Parsed = %q, want %q", got, want)
	}
	if u.User == nil || u.Fragment == "" {
		t.Errorf("Parsed changed its argument: %v", u)
	}
}
