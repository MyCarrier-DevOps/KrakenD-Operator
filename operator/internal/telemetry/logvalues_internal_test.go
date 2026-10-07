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
	"testing"
	"time"
)

// A key/value list with nothing to convert is passed on as it is, without a
// copy.
func TestReadableKeysAndValues_ReusesAListThatNeedsNoConversion(t *testing.T) {
	in := []any{"s", "text", "b", true, "i", 1, "i64", int64(2), "f", 1.5, "d", time.Second}

	out := readableKeysAndValues(in)

	if &out[0] != &in[0] {
		t.Error("a list of plain values was copied")
	}
}
