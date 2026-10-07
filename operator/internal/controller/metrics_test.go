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

// A gateway reconciler given a recorder records into it, not into anything
// global.
func TestGatewayReconcile_RecordsIntoTheInjectedMetrics(t *testing.T) {
	gw := testGateway()
	c, _ := gatewayStatusWrites(gw)
	m, reg := testMetrics(t)
	r := newTestGatewayReconciler(c, renderOutput("cs"), &mockValidator{})
	r.Metrics = m

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if got, _ := metricValue(t, reg, "krakend_operator_config_renders_total"); got != 1 {
		t.Errorf("config_renders_total = %v in the injected recorder, want 1", got)
	}
	if got, ok := metricValue(t, reg, "krakend_operator_endpoints", "namespace", gw.Namespace, "name", gw.Name); !ok || got != 0 {
		t.Errorf("endpoints = %v (present %v), want a 0 series for the gateway", got, ok)
	}
}
