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

import "time"

// Stage is where a license stands in its validity window at an instant.
type Stage int

const (
	// StageValid: expiry is further away than the warning window.
	StageValid Stage = iota
	// StageExpiringSoon: expiry is inside the warning window.
	StageExpiringSoon
	// StagePreExpiry: expiry is inside the safety buffer. EE processes stop at
	// expiry, so the gateway already acts as if the license had expired.
	StagePreExpiry
	// StageExpired: the certificate's NotAfter has passed.
	StageExpired
)

// Window splits a license's lifetime into stages. Warning must be longer than
// SafetyBuffer.
type Window struct {
	Warning      time.Duration
	SafetyBuffer time.Duration
}

// StageAt returns the stage, at now, of a license that expires at notAfter.
func (w Window) StageAt(notAfter, now time.Time) Stage {
	switch {
	case !notAfter.After(now):
		return StageExpired
	case !notAfter.After(now.Add(w.SafetyBuffer)):
		return StagePreExpiry
	case !notAfter.After(now.Add(w.Warning)):
		return StageExpiringSoon
	default:
		return StageValid
	}
}

// NextChange returns how long after now StageAt next returns a different
// stage, or 0 once the license has expired.
func (w Window) NextChange(notAfter, now time.Time) time.Duration {
	for _, boundary := range []time.Time{notAfter.Add(-w.Warning), notAfter.Add(-w.SafetyBuffer), notAfter} {
		if boundary.After(now) {
			return boundary.Sub(now)
		}
	}
	return 0
}
