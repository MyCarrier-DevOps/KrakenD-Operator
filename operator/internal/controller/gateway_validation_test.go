package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

func TestEndpointAccepted_AnExcludedEndpointIsNotServedForItsOwnReason(t *testing.T) {
	gw := testGateway()
	bad := testEndpoint("bad", "/b")
	bad.Generation = 3
	tests := []struct {
		name    string
		verdict configcheck.EndpointVerdict
		says    string
	}{
		{"its own fault", configcheck.EndpointVerdict{Reason: v1alpha1.ReasonEndpointInvalid,
			Output: "- at '/endpoints/0/backend/0/host/0': bad host"}, "bad host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rv := newRenderVerdicts(&renderer.RenderOutput{},
				map[types.NamespacedName]configcheck.EndpointVerdict{client.ObjectKeyFromObject(bad): tt.verdict})

			a := endpointAccepted(gw, bad, rv)

			c := a.condition
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != tt.verdict.Reason || c.ObservedGeneration != 3 ||
				!strings.HasPrefix(c.Message, "Not served by gateway default/test-gw, which serves its other endpoints: ") ||
				!strings.Contains(c.Message, tt.says) {
				t.Errorf("Accepted = %+v, want False/%s at generation 3, not served by default/test-gw, saying %q",
					c, tt.verdict.Reason, tt.says)
			}
			if a.conflicts != nil || a.keepConflicts {
				t.Errorf("acceptance = %+v, want status.conflicts cleared: nothing of it is served", a)
			}
		})
	}
}
