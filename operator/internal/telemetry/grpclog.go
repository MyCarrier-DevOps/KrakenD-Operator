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
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/grpclog"
)

// grpcInfoLevel is the verbosity of grpc-go's info records. grpc-go logs a
// connection's every state change at info, and its own default logger drops
// info records, so they are shown only from verbosity 2, above the operator's
// default debug level.
const grpcInfoLevel = 2

// grpcTarget is the logger grpc-go's records go to. InstallLogging sets it;
// until then, while Setup builds the exporters, the records are dropped.
var grpcTarget atomic.Pointer[logr.Logger]

// InstallGRPCLogging makes grpc-go, which the OTLP gRPC exporters run on, log
// through the logger InstallLogging gives it instead of its default logger,
// which writes errors to stderr. grpc-go requires its logger to be set before
// any gRPC call, so cmd calls this first, before the logger exists: records
// logged until InstallLogging has run are dropped. It is process-wide by
// nature.
func InstallGRPCLogging() {
	grpclog.SetLoggerV2(grpcLogger{logger: logr.New(grpcSink{}), exit: os.Exit})
}

// grpcSink is a logr.LogSink that logs through grpcTarget, when it is set.
type grpcSink struct{}

func (grpcSink) Init(logr.RuntimeInfo) {}

func (grpcSink) Enabled(level int) bool {
	target := grpcTarget.Load()
	return target != nil && target.V(level).Enabled()
}

func (grpcSink) Info(level int, msg string, keysAndValues ...any) {
	if target := grpcTarget.Load(); target != nil {
		target.V(level).Info(msg, keysAndValues...)
	}
}

func (grpcSink) Error(err error, msg string, keysAndValues ...any) {
	if target := grpcTarget.Load(); target != nil {
		target.Error(err, msg, keysAndValues...)
	}
}

func (s grpcSink) WithValues(...any) logr.LogSink { return s }
func (s grpcSink) WithName(string) logr.LogSink   { return s }

// grpcLogger is a grpclog.LoggerV2 that logs through a logr.Logger: info at
// grpcInfoLevel, warnings at info, errors as errors. Fatal logs an error,
// then exits with status 1, as grpclog.LoggerV2 requires.
type grpcLogger struct {
	logger logr.Logger
	exit   func(code int)
}

func (g grpcLogger) Info(args ...any)                 { g.info(fmt.Sprint(args...)) }
func (g grpcLogger) Infoln(args ...any)               { g.info(sprintln(args)) }
func (g grpcLogger) Infof(format string, args ...any) { g.info(fmt.Sprintf(format, args...)) }

func (g grpcLogger) Warning(args ...any)                 { g.logger.Info(fmt.Sprint(args...)) }
func (g grpcLogger) Warningln(args ...any)               { g.logger.Info(sprintln(args)) }
func (g grpcLogger) Warningf(format string, args ...any) { g.logger.Info(fmt.Sprintf(format, args...)) }

func (g grpcLogger) Error(args ...any)   { g.logger.Error(nil, fmt.Sprint(args...)) }
func (g grpcLogger) Errorln(args ...any) { g.logger.Error(nil, sprintln(args)) }
func (g grpcLogger) Errorf(format string, args ...any) {
	g.logger.Error(nil, fmt.Sprintf(format, args...))
}

func (g grpcLogger) Fatal(args ...any)                 { g.fatal(fmt.Sprint(args...)) }
func (g grpcLogger) Fatalln(args ...any)               { g.fatal(sprintln(args)) }
func (g grpcLogger) Fatalf(format string, args ...any) { g.fatal(fmt.Sprintf(format, args...)) }

// V reports whether grpc-go's verbosity level l is logged.
func (g grpcLogger) V(l int) bool { return g.logger.V(l).Enabled() }

func (g grpcLogger) info(msg string) { g.logger.V(grpcInfoLevel).Info(msg) }

func (g grpcLogger) fatal(msg string) {
	g.logger.Error(nil, msg)
	g.exit(1)
}

// sprintln formats args as fmt.Sprintln does, without the newline: a record
// is one line already.
func sprintln(args []any) string {
	return strings.TrimSuffix(fmt.Sprintln(args...), "\n")
}
