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
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

// clientSpans wraps the fetcher's transport. It is a stub: it only calls next.
type clientSpans struct {
	next   http.RoundTripper
	tracer trace.Tracer
}

// RoundTrip calls next.
func (c clientSpans) RoundTrip(r *http.Request) (*http.Response, error) {
	return c.next.RoundTrip(r)
}
