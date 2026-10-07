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

package autoconfig

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// redactedValue replaces each query value in a URL the operator records.
const redactedValue = "REDACTED"

// RedactURL returns raw without its user information, with each query value
// replaced by REDACTED (a bare key, as in "?token", included) and without its
// fragment, for errors, logs and spans: an OpenAPI URL can carry credentials
// in any of them. A URL that does not parse, or that has no "//" after its
// scheme (the opaque form, whose credentials parse as part of the path), is
// reduced to "<unparseable URL>".
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return unparseableURL
	}
	return redact(u)
}

// unparseableURL stands for a URL that cannot be shown safely.
const unparseableURL = "<unparseable URL>"

func redact(u *url.URL) string {
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

// withoutURL returns err without the *url.Error wrapper net/http and
// url.Parse add, whose text repeats the URL with its credentials.
func withoutURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// clientSpans is an http.RoundTripper that makes each request a client span
// of the span in its context. It records the method, the redacted URL, the
// server and the status code, and adds no header: OpenAPI documents are
// served by third parties, so the trace context is not propagated to them.
type clientSpans struct {
	next   http.RoundTripper
	tracer trace.Tracer
}

// RoundTrip sends r inside a client span.
func (c clientSpans) RoundTrip(r *http.Request) (resp *http.Response, err error) {
	ctx, span := tracing.Start(r.Context(), c.tracer, "HTTP "+r.Method, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.HTTPRequestMethodKey.String(r.Method), semconv.URLFull(redact(r.URL)),
			semconv.ServerAddress(r.URL.Hostname())))
	defer func() {
		if resp != nil {
			span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
			if resp.StatusCode >= http.StatusBadRequest {
				span.SetStatus(codes.Error, "")
			}
		}
		tracing.End(span, err)
	}()
	if port, convErr := strconv.Atoi(r.URL.Port()); convErr == nil {
		span.SetAttributes(semconv.ServerPort(port))
	}
	resp, err = c.next.RoundTrip(r.WithContext(ctx))
	if err != nil {
		return resp, err
	}
	return refuseUnparseableRedirect(r, resp)
}

// refuseUnparseableRedirect returns resp unless it redirects to a Location
// that cannot be parsed: net/http's error for that repeats the Location, whose
// query can carry a signature, so the redirect is refused here without it.
func refuseUnparseableRedirect(r *http.Request, resp *http.Response) (*http.Response, error) {
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return resp, nil
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return resp, nil
	}
	if _, err := r.URL.Parse(location); err != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			logf.FromContext(r.Context()).V(1).Info("failed to close response body", "error", cerr)
		}
		return nil, errors.New("redirect has an unparseable Location header")
	}
	return resp, nil
}
