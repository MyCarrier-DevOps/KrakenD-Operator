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

package telemetry_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	"github.com/mycarrier-devops/krakend-operator/internal/tracing/tracingtest"
)

// fakeAPIServer answers every request with the same ConfigMap and keeps the
// traceparent header of each request it got.
type fakeAPIServer struct {
	mu           sync.Mutex
	traceparents []string
}

func (s *fakeAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.traceparents = append(s.traceparents, r.Header.Get("Traceparent"))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"namespace":"ns","name":"cm"}}`)
}

// tracedClient returns a client for a fake API server whose config went
// through TraceKubeAPI, and the server.
func tracedClient(t *testing.T, rec *tracingtest.Recorder) (client.Client, *fakeAPIServer) {
	t.Helper()
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	cfg := &rest.Config{Host: srv.URL}
	telemetry.TraceKubeAPI(cfg, rec.Provider())
	// A static mapper: the fake server answers no discovery request.
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	c, err := client.New(cfg, client.Options{Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	return c, api
}

func updateConfigMap(ctx context.Context, c client.Client) error {
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cm"},
	}
	return c.Update(ctx, cm)
}

func TestTraceKubeAPI_RequestUnderASpanIsItsChildAndCarriesTheTrace(t *testing.T) {
	rec := tracingtest.New(t)
	c, api := tracedClient(t, rec)
	ctx, parent := rec.Tracer().Start(context.Background(), "reconcile")

	if err := updateConfigMap(ctx, c); err != nil {
		t.Fatal(err)
	}
	parent.End()

	spans := rec.Ended()
	spans.RequireChild(t, "reconcile", "k8s update configmaps")
	if len(api.traceparents) != 1 || api.traceparents[0] == "" {
		t.Errorf("API server got traceparent headers %q, want one", api.traceparents)
	}
}

func TestTraceKubeAPI_RequestWithoutASpanStartsNoTrace(t *testing.T) {
	rec := tracingtest.New(t)
	c, api := tracedClient(t, rec)

	if err := updateConfigMap(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	if got := rec.Ended(); len(got) != 0 {
		t.Errorf("spans = %s, want none for a request with no span", got)
	}
	if len(api.traceparents) != 1 || api.traceparents[0] != "" {
		t.Errorf("API server got traceparent headers %q, want none", api.traceparents)
	}
}
