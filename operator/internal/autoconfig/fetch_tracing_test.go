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
	"net/http"
	"net/http/httptest"
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
