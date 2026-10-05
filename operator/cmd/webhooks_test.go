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
	"errors"
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

func TestRegisterWebhooks_EnabledRunsSetup(t *testing.T) {
	boom := errors.New("setup failed")
	var got ctrl.Manager
	mgr := untouchableManager{}
	err := registerWebhooks(mgr, true, func(m ctrl.Manager) error {
		got = m
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the setup error", err)
	}
	if got != mgr {
		t.Error("setup did not receive the manager")
	}
}

func TestWebhookCertWatchNeeded(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		certPath string
		want     bool
	}{
		{"enabled with cert path", true, "/certs", true},
		{"enabled without cert path", true, "", false},
		{"disabled with cert path", false, "/certs", false},
		{"disabled without cert path", false, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookCertWatchNeeded(tc.enabled, tc.certPath); got != tc.want {
				t.Errorf("webhookCertWatchNeeded(%v, %q) = %v, want %v", tc.enabled, tc.certPath, got, tc.want)
			}
		})
	}
}

func TestDefaultOperatorUsername(t *testing.T) {
	tests := []struct{ ns, sa, want string }{
		{"krakend-system", "krakend-operator", "system:serviceaccount:krakend-system:krakend-operator"},
	}
	for _, tt := range tests {
		t.Setenv("POD_NAMESPACE", tt.ns)
		t.Setenv("POD_SERVICE_ACCOUNT", tt.sa)
		if got := defaultOperatorUsername(); got != tt.want {
			t.Errorf("POD_NAMESPACE=%q POD_SERVICE_ACCOUNT=%q: got %q, want %q", tt.ns, tt.sa, got, tt.want)
		}
	}
}
