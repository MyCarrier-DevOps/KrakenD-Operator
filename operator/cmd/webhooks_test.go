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

package main

import (
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
)

// untouchableManager panics on every Manager method (the embedded interface
// is nil), so a test using it passes only if the code never touches it.
type untouchableManager struct {
	ctrl.Manager
}

func TestRegisterWebhooks_DisabledLeavesManagerUntouched(t *testing.T) {
	setupRan := false
	err := registerWebhooks(untouchableManager{}, false, func(ctrl.Manager) error {
		setupRan = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if setupRan {
		t.Error("webhook setup ran with webhooks disabled")
	}
}
