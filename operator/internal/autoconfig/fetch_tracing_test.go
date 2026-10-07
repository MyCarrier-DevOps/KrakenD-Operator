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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// The spec host is a third party: it gets no trace header, and no span keeps
// the URL's credentials.
func TestClientSpans_RecordTheRedactedURLAndPropagateNothing(t *testing.T) {
	rec := tracingtest.New(t)
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := &http.Client{Transport: clientSpans{next: http.DefaultTransport, tracer: rec.Tracer()}}
	ctx, parent := rec.Tracer().Start(context.Background(), "autoconfig.fetch")
	u := strings.Replace(srv.URL, "http://", "http://user:secret@", 1) + "/spec.json?token=abc123"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	parent.End()

	spans := rec.Ended()
	spans.RequireChild(t, "autoconfig.fetch", "HTTP GET")
	for _, kv := range spans.One(t, "HTTP GET").Attributes() {
		if v := kv.Value.String(); strings.Contains(v, "secret") || strings.Contains(v, "abc123") {
			t.Errorf("attribute %s = %q records a credential", kv.Key, v)
		}
	}
	if tp := headers.Get("Traceparent"); tp != "" {
		t.Errorf("the spec host got traceparent %q, want none", tp)
	}
}

// A failed fetch's error, which becomes a status message and a span event,
// names the URL without its credentials.
func TestFetcher_ErrorsCarryNoCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	f := &httpFetcher{
		strictClient:  &http.Client{Transport: http.DefaultTransport, Timeout: fetchTimeout},
		lenientClient: &http.Client{Transport: http.DefaultTransport, Timeout: fetchTimeout},
	}
	withSecrets := func(base string) string {
		return strings.Replace(base, "http://", "http://user:secret@", 1) + "/spec.json?token=abc123"
	}
	unreachable := "http://127.0.0.1:1"

	for _, raw := range []string{withSecrets(srv.URL), withSecrets(unreachable)} {
		_, err := f.Fetch(context.Background(), FetchSource{URL: raw})
		if err == nil {
			t.Fatalf("Fetch(%s) succeeded", raw)
		}
		if msg := err.Error(); strings.Contains(msg, "secret") || strings.Contains(msg, "abc123") {
			t.Errorf("error %q carries a credential", msg)
		}
		if _, ok := errorsAsURLError(err); ok {
			t.Errorf("error %q still wraps a *url.Error", err)
		}
	}
}

func errorsAsURLError(err error) (*url.Error, bool) {
	var uerr *url.Error
	return uerr, errors.As(err, &uerr)
}

// An unparseable URL's error does not repeat the URL either.
func TestFetcher_AParseErrorCarriesNoCredentials(t *testing.T) {
	f := &httpFetcher{}

	_, err := f.Fetch(context.Background(), FetchSource{URL: "http://user:pw@host:badport/spec.json?token=secret"})

	if err == nil {
		t.Fatal("Fetch succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, "pw@") || strings.Contains(msg, "secret") {
		t.Errorf("error %q carries a credential", msg)
	}
}

func TestNewFetcher_FetchIsASpanAboveItsHTTPRequest(t *testing.T) {
	rec := tracingtest.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openapi":"3.0.0"}`))
	}))
	defer srv.Close()
	f := NewFetcher(fakeClient(), rec.Tracer()).(*httpFetcher)
	// The SSRF guard refuses loopback: keep the traced wrapper, swap what it wraps.
	f.lenientClient.Transport = clientSpans{next: http.DefaultTransport, tracer: rec.Tracer()}

	if _, err := f.Fetch(context.Background(), FetchSource{URL: srv.URL + "/spec.json?token=abc123", AllowClusterLocal: true}); err != nil {
		t.Fatal(err)
	}

	spans := rec.Ended()
	spans.RequireChild(t, "autoconfig.fetch", "HTTP GET")
	if _, ok := NewFetcher(fakeClient(), nil).(*httpFetcher).strictClient.Transport.(clientSpans); !ok {
		t.Error("the strict client's transport is not traced")
	}
}

// requireNoSecretInSpans fails the test if any attribute, status description
// or event attribute of the recorded spans contains one of secrets.
func requireNoSecretInSpans(t *testing.T, spans tracingtest.Spans, secrets ...string) {
	t.Helper()
	check := func(where, v string) {
		for _, secret := range secrets {
			if strings.Contains(v, secret) {
				t.Errorf("%s = %q records %q", where, v, secret)
			}
		}
	}
	for _, span := range spans {
		for _, kv := range span.Attributes() {
			check(span.Name()+" attribute "+string(kv.Key), kv.Value.Emit())
		}
		check(span.Name()+" status", span.Status().Description)
		for _, ev := range span.Events() {
			for _, kv := range ev.Attributes {
				check(span.Name()+" event attribute "+string(kv.Key), kv.Value.Emit())
			}
		}
	}
}

// A URL in the opaque form (no "//") parses with its credentials in Opaque:
// they are recorded nowhere, however it reaches a fetch.
func TestFetch_AnOpaqueURLLeaksNoCredentials(t *testing.T) {
	const opaque = "https:user:pw@schemas.example.com/spec.json?token=abc"
	rec := tracingtest.New(t)
	f := NewFetcher(fakeClient(), rec.Tracer())

	_, err := f.Fetch(context.Background(), FetchSource{URL: opaque})

	if err == nil {
		t.Fatal("Fetch succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, "user:pw") || strings.Contains(msg, "abc") {
		t.Errorf("error %q carries a credential", msg)
	}
	requireNoSecretInSpans(t, rec.Ended(), "user:pw", "abc")
}

// A URL without a host is refused before any request is made.
func TestFetch_AURLWithoutAHostIsRefusedEarly(t *testing.T) {
	rec := tracingtest.New(t)
	f := NewFetcher(fakeClient(), rec.Tracer())

	_, err := f.Fetch(context.Background(), FetchSource{URL: "https:user:pw@schemas.example.com/spec.json"})

	if err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("Fetch error = %v, want a refusal naming the missing host", err)
	}
	if n := len(rec.Ended().Named("HTTP GET")); n != 0 {
		t.Errorf("%d HTTP GET spans, want none: the request is never made", n)
	}
}

// A redirect whose Location cannot be parsed fails the fetch without repeating
// the Location, which can carry a signature in its query.
func TestFetch_AnUnparseableRedirectLocationLeaksNothing(t *testing.T) {
	rec := tracingtest.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://host:badport/x?sig=LOCSECRET")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	f := NewFetcher(fakeClient(), rec.Tracer()).(*httpFetcher)
	f.lenientClient.Transport = clientSpans{next: http.DefaultTransport, tracer: rec.Tracer()}

	_, err := f.Fetch(context.Background(), FetchSource{URL: srv.URL + "/spec.json", AllowClusterLocal: true})

	if err == nil {
		t.Fatal("Fetch succeeded")
	}
	if strings.Contains(err.Error(), "LOCSECRET") {
		t.Errorf("error %q carries the Location's query", err)
	}
	requireNoSecretInSpans(t, rec.Ended(), "LOCSECRET")
}
