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

package license

import (
	"testing"
	"time"
)

func TestWindow_StagesAndTheirBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	w := Window{Warning: 30 * 24 * time.Hour, SafetyBuffer: time.Hour}
	cases := []struct {
		name       string
		notAfter   time.Time
		wantStage  Stage
		wantChange time.Duration
	}{
		{"valid", now.Add(31 * 24 * time.Hour), StageValid, 24 * time.Hour},
		{"warning starts exactly now", now.Add(30 * 24 * time.Hour), StageExpiringSoon, 30*24*time.Hour - time.Hour},
		{"expiring soon", now.Add(2 * time.Hour), StageExpiringSoon, time.Hour},
		{"inside the safety buffer", now.Add(30 * time.Minute), StagePreExpiry, 30 * time.Minute},
		{"expired", now.Add(-time.Minute), StageExpired, 0},
		{"expires exactly now", now, StageExpired, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.StageAt(tc.notAfter, now); got != tc.wantStage {
				t.Errorf("StageAt = %d, want %d", got, tc.wantStage)
			}
			if got := w.NextChange(tc.notAfter, now); got != tc.wantChange {
				t.Errorf("NextChange = %s, want %s", got, tc.wantChange)
			}
			if tc.wantChange > 0 {
				if next := w.StageAt(tc.notAfter, now.Add(tc.wantChange)); next == tc.wantStage {
					t.Errorf("at now+NextChange the stage is still %d; NextChange must land on a boundary", next)
				}
			}
		})
	}
}
