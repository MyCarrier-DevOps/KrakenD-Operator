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
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
)

// grpclog.LoggerV2 requires Fatal not to return: grpc-go calls it where it
// cannot go on. Each Fatal method logs an error, then exits with status 1.
func TestGRPCLogger_FatalLogsThenExits(t *testing.T) {
	for name, fatal := range map[string]func(grpcLogger){
		"Fatal":   func(g grpcLogger) { g.Fatal("cannot go on") },
		"Fatalln": func(g grpcLogger) { g.Fatalln("cannot", "go on") },
		"Fatalf":  func(g grpcLogger) { g.Fatalf("cannot %s", "go on") },
	} {
		t.Run(name, func(t *testing.T) {
			var logged []string
			code := 0
			g := grpcLogger{
				logger: funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{}),
				exit:   func(c int) { code = c },
			}

			fatal(g)

			if len(logged) != 1 || !strings.Contains(logged[0], `"msg"="cannot go on"`) || code != 1 {
				t.Errorf("logged %q and exited with %d, want the message logged, then exit status 1", logged, code)
			}
		})
	}
}
