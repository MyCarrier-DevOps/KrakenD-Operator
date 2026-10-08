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

// Package redact removes what a URL carries that must not reach a log, a span
// or an error message.
package redact

import (
	"net/url"
	"slices"
	"strings"
)

// redactedValue replaces each query value in a URL the operator records.
const redactedValue = "REDACTED"

// unparseableURL stands for a URL that cannot be shown safely.
const unparseableURL = "<unparseable URL>"

// URL returns raw without its user information, with each query value
// replaced by REDACTED (a bare key, as in "?token", included) and without its
// fragment, for errors, logs and spans: a URL can carry credentials in any of
// them. A URL that does not parse, or that has no "//" after its scheme (the
// opaque form, whose credentials parse as part of the path), is reduced to
// "<unparseable URL>".
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return unparseableURL
	}
	return Parsed(u)
}

// Parsed is URL for a URL that is already parsed.
func Parsed(u *url.URL) string {
	if u.Opaque != "" {
		return unparseableURL
	}
	r := *u
	r.User, r.Fragment, r.RawFragment = nil, "", ""
	r.RawQuery = redactQuery(r.RawQuery)
	return r.String()
}

// redactQuery replaces the value of each "key=value" pair of query with
// REDACTED, keeping its key, and a bare key, which is a token as much as a
// value is, with REDACTED. Pairs are separated by "&" or ";" and come back
// separated by "&", sorted, without repeats.
func redactQuery(query string) string {
	var pairs []string
	for _, pair := range strings.FieldsFunc(query, func(r rune) bool { return r == '&' || r == ';' }) {
		if key, _, hasValue := strings.Cut(pair, "="); hasValue {
			pairs = append(pairs, key+"="+redactedValue)
		} else {
			pairs = append(pairs, redactedValue)
		}
	}
	slices.Sort(pairs)
	return strings.Join(slices.Compact(pairs), "&")
}
