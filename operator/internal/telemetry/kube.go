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
	"context"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycarrier-devops/krakend-operator/internal/tracing"
)

// apiRequests parses Kubernetes API request paths into verb, resource and
// object.
var apiRequests = request.RequestInfoFactory{
	APIPrefixes:          sets.NewString("api", "apis"),
	GrouplessAPIPrefixes: sets.NewString("api"),
}

// TraceKubeAPI makes every request a client built from cfg sends under an
// active span a client span of that span, and passes the trace context on to
// the API server. Requests with no span in their context (informer lists and
// watches, leader-election renewals, event writes, metrics authorization) are
// sent as they are, so they start no trace of their own. No HTTP metric is
// recorded.
func TraceKubeAPI(cfg *rest.Config, tp trace.TracerProvider) {
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return otelhttp.NewTransport(kubeAttributes{next: rt},
			otelhttp.WithTracerProvider(tp),
			otelhttp.WithMeterProvider(metricnoop.NewMeterProvider()),
			otelhttp.WithPropagators(propagation.TraceContext{}),
			otelhttp.WithFilter(hasSpan),
			otelhttp.WithSpanNameFormatter(kubeSpanName),
		)
	})
}

// ReadEvents wraps c so each Get and List it serves, from the cache or not,
// adds an event to the active span. Reads the cache serves send no request,
// so they have no client span of their own.
func ReadEvents(c client.Client) client.Client {
	return readEvents{Client: c}
}

// readEvents is the client ReadEvents returns.
type readEvents struct{ client.Client }

// Get reads through the wrapped client and records the read.
func (c readEvents) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	c.event(ctx, "k8s.client.get", obj, semconv.K8SNamespaceName(key.Namespace),
		tracing.KeyName.String(key.Name), attribute.Bool("found", err == nil))
	return err
}

// List reads through the wrapped client and records the read.
func (c readEvents) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := c.Client.List(ctx, list, opts...)
	listOpts := (&client.ListOptions{}).ApplyOptions(opts)
	c.event(ctx, "k8s.client.list", list, semconv.K8SNamespaceName(listOpts.Namespace),
		attribute.Bool("succeeded", err == nil))
	return err
}

func (c readEvents) event(ctx context.Context, name string, obj runtime.Object, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	if gvk, err := c.GroupVersionKindFor(obj); err == nil {
		attrs = append(attrs, tracing.KeyKind.String(gvk.Kind))
	}
	span.AddEvent(name, trace.WithAttributes(attrs...))
}

// kubeAttributes adds the namespace, resource and object name of a
// Kubernetes API request to the client span it is sent under.
type kubeAttributes struct{ next http.RoundTripper }

// RoundTrip annotates the request's span and sends the request on.
func (k kubeAttributes) RoundTrip(r *http.Request) (*http.Response, error) {
	if info, err := apiRequests.NewRequestInfo(r); err == nil && info.IsResourceRequest {
		trace.SpanFromContext(r.Context()).SetAttributes(
			semconv.K8SNamespaceName(info.Namespace),
			tracing.KeyName.String(info.Name),
			attribute.String("k8s.resource", info.Resource),
			attribute.String("k8s.verb", info.Verb))
	}
	return k.next.RoundTrip(r)
}

// hasSpan reports whether r is sent under an active span.
func hasSpan(r *http.Request) bool {
	return trace.SpanContextFromContext(r.Context()).IsValid()
}

// kubeSpanName names a Kubernetes API request by its verb and resource, as
// "k8s update krakendgateways/status".
func kubeSpanName(_ string, r *http.Request) string {
	info, err := apiRequests.NewRequestInfo(r)
	if err != nil || !info.IsResourceRequest {
		return "k8s " + r.Method + " " + r.URL.Path
	}
	resource := info.Resource
	if info.Subresource != "" {
		resource += "/" + info.Subresource
	}
	return "k8s " + info.Verb + " " + resource
}
