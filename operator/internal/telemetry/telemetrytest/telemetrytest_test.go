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

package telemetrytest_test

import (
	"os"
	"strings"
	"testing"

	"github.com/mycarrier-devops/krakend-operator/internal/telemetry/telemetrytest"
)

// The helper finds the variables itself, so one a later Setup reads needs no
// entry in a list.
func TestClearOTelEnv_UnsetsEveryOTelVariableAndRestoresThemAfterwards(t *testing.T) {
	const name = "OTEL_A_NAME_NO_LIST_KNOWS"
	t.Setenv(name, "set")
	t.Setenv("NOT_OTEL_UNRELATED", "kept")

	t.Run("inside", func(t *testing.T) {
		telemetrytest.ClearOTelEnv(t)

		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "OTEL_") && !strings.HasSuffix(kv, "=") {
				t.Errorf("%q is still set", kv)
			}
		}
		if got := os.Getenv("NOT_OTEL_UNRELATED"); got != "kept" {
			t.Errorf("NOT_OTEL_UNRELATED = %q, want it untouched", got)
		}
	})

	if got := os.Getenv(name); got != "set" {
		t.Errorf("%s = %q after the test, want it restored", name, got)
	}
}
