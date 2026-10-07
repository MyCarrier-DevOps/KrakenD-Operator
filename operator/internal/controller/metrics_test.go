package controller

import (
	"strings"
	"testing"
)

func TestConfigValidationFailures_HelpSaysItCountsChangesNotReconciles(t *testing.T) {
	help := configValidationFailures.Desc().String()

	if !strings.Contains(help, "counted once per change of what the gateway controller checks, not once per reconcile; ") {
		t.Errorf("Help = %s, want it to say a rejection is counted once per change of what the gateway "+
			"controller checks, not once per reconcile", help)
	}
}
