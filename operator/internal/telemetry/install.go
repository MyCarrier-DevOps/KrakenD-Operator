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
	"log"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// InstallLogging makes every logger in the process log through logger:
// controller-runtime's (and so the manager's, the controllers', the webhook
// server's and leader election's), client-go's klog, the standard library's
// log package (net/http servers report TLS handshake errors through it), and
// the OpenTelemetry SDK's own error handler and internal logger, which log
// through diagnostics. These are process-wide by nature; nothing else in the
// operator reads a global logger.
func InstallLogging(logger, diagnostics logr.Logger) {
	ctrl.SetLogger(logger)
	klog.SetLoggerWithOptions(logger.WithName("klog"), klog.ContextualLogger(true))
	log.SetFlags(0)
	log.SetOutput(stdlibWriter{logger: logger.WithName("stdlib")})
	otel.SetLogger(diagnostics)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(newErrorHandler(diagnostics)))
}

// stdlibWriter logs each write of the standard library's log package as a
// record.
type stdlibWriter struct{ logger logr.Logger }

// Write logs p, one record per call, without its trailing newline.
func (w stdlibWriter) Write(p []byte) (int, error) {
	w.logger.Info(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}
