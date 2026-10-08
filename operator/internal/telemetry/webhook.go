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

package telemetry

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// TraceWebhookServer wraps srv so each admission request it serves is a
// server span named "admission <path>". The span continues the trace the API
// server propagates (W3C traceparent) when it sends one, and starts a new
// trace otherwise. No HTTP metric is recorded.
func TraceWebhookServer(srv webhook.Server, tp trace.TracerProvider) webhook.Server {
	return tracedWebhookServer{Server: srv, tp: tp}
}

// tracedWebhookServer is the server TraceWebhookServer returns.
type tracedWebhookServer struct {
	webhook.Server
	tp trace.TracerProvider
}

// Register serves hook at path inside a server span.
func (s tracedWebhookServer) Register(path string, hook http.Handler) {
	s.Server.Register(path, otelhttp.NewHandler(hook, "admission "+path,
		otelhttp.WithTracerProvider(s.tp),
		otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
		otelhttp.WithPropagators(propagation.TraceContext{}),
		otelhttp.WithSpanNameFormatter(func(operation string, _ *http.Request) string { return operation }),
	))
}
