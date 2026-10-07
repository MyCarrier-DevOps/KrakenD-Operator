//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// excludedEndpointsGauge reads the operator's excluded-endpoints gauge for a
// gateway, by reason. The suite runs the manager in this process, so its
// registry is the one the operator writes.
func excludedEndpointsGauge(namespace, gateway string) (map[string]float64, error) {
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		return nil, err
	}
	series := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "krakend_operator_gateway_excluded_endpoints" {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["namespace"] == namespace && labels["gateway"] == gateway {
				series[labels["reason"]] = m.GetGauge().GetValue()
			}
		}
	}
	return series, nil
}

// appliedConfig is the config the gateway key applies.
func appliedConfig(key client.ObjectKey) (*v1alpha1.KrakenDGateway, string, error) {
	var gw v1alpha1.KrakenDGateway
	if err := k8sClient.Get(ctx, key, &gw); err != nil {
		return nil, "", err
	}
	if gw.Status.ConfigChecksum == "" {
		return &gw, "", fmt.Errorf("no applied config yet")
	}
	var cm corev1.ConfigMap
	name := types.NamespacedName{Namespace: key.Namespace, Name: resources.ConfigMapName(&gw, gw.Status.ConfigChecksum)}
	if err := k8sClient.Get(ctx, name, &cm); err != nil {
		return &gw, "", err
	}
	return &gw, cm.Data[resources.ConfigKey], nil
}

func TestExclusion_AnInvalidEndpointIsExcludedAndTheRestApplied(t *testing.T) {
	ns := testNamespace(t)
	gw := createGateway(t, ns, "gw-exclude")
	good := createEndpoint(t, ns, "good", gw.Name, "/good")
	bad := createEndpoint(t, ns, "bad", gw.Name, rejectMarker)

	eventually(t, func() error {
		got, applied, err := appliedConfig(gw)
		if err != nil {
			return err
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionConfigValid) {
			return fmt.Errorf("ConfigValid not True: %+v", got.Status.Conditions)
		}
		if !strings.Contains(applied, `"/good"`) || strings.Contains(applied, rejectMarker) {
			return fmt.Errorf("applied config does not serve /good alone:\n%s", applied)
		}
		ex := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionEndpointsExcluded)
		if ex == nil || ex.Status != metav1.ConditionTrue || !strings.Contains(ex.Message, ns+"/bad") {
			return fmt.Errorf("EndpointsExcluded = %+v, want True naming %s/bad", ex, ns)
		}
		return nil
	})
	eventually(t, func() error {
		ep, err := getEndpoint(bad)
		if err != nil {
			return err
		}
		if err := expectCondition(ep, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonEndpointInvalid); err != nil {
			return err
		}
		if c := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted); !strings.Contains(c.Message,
			"rejected by the integration test validator") {
			return fmt.Errorf("bad's message %q does not quote its own output", c.Message)
		}
		sibling, err := getEndpoint(good)
		if err != nil {
			return err
		}
		return expectCondition(sibling, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	})
	eventually(t, func() error {
		if n, err := eventCount(ns, gw.Name, v1alpha1.ReasonInvalidEndpointsExcluded); err != nil || n < 1 {
			return fmt.Errorf("%d %s events (%v), want at least one", n, v1alpha1.ReasonInvalidEndpointsExcluded, err)
		}
		series, err := excludedEndpointsGauge(ns, gw.Name)
		if err != nil || series[v1alpha1.ReasonEndpointInvalid] != 1 {
			return fmt.Errorf("gauge = %v (%v), want EndpointInvalid=1", series, err)
		}
		return nil
	})

	// The owner fixes it: it is served, and every signal clears.
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		ep, err := getEndpoint(bad)
		if err != nil {
			return err
		}
		ep.Spec.Endpoints[0].Endpoint = "/fixed"
		ep.Spec.Endpoints[0].Backends[0].URLPattern = "/fixed"
		return k8sClient.Update(ctx, ep)
	})
	if err != nil {
		t.Fatalf("fixing the endpoint: %v", err)
	}
	eventually(t, func() error {
		got, applied, err := appliedConfig(gw)
		if err != nil {
			return err
		}
		if !strings.Contains(applied, `"/fixed"`) {
			return fmt.Errorf("the fixed endpoint is not applied")
		}
		if !strings.Contains(applied, `"/good"`) {
			return fmt.Errorf("the good endpoint is still applied")
		}
		if c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionEndpointsExcluded); c != nil {
			return fmt.Errorf("EndpointsExcluded = %+v, want it removed", c)
		}
		if series, err := excludedEndpointsGauge(ns, gw.Name); err != nil || len(series) != 0 {
			return fmt.Errorf("gauge = %v (%v), want no series", series, err)
		}
		ep, err := getEndpoint(bad)
		if err != nil {
			return err
		}
		if err := expectCondition(ep, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted); err != nil {
			return err
		}
		goodEp, err := getEndpoint(good)
		if err != nil {
			return err
		}
		return expectCondition(goodEp, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	})
}

func TestExclusion_AnEndpointOfAnInvalidPolicyIsExcludedByName(t *testing.T) {
	ns := testNamespace(t)
	gw := createGateway(t, ns, "gw-policy-exclude")
	policy := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "refused", Namespace: ns},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{Raw: rootRejecting(rejectMarker)},
	}
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	user := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "uses-refused", Namespace: ns},
		Spec: v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: gw.Name},
			Endpoints: []v1alpha1.EndpointEntry{{Endpoint: "/uses", Method: "GET", Backends: []v1alpha1.BackendSpec{{
				Host: []string{"http://svc:8080"}, URLPattern: "/uses", PolicyRef: &v1alpha1.PolicyRef{Name: "refused"},
			}}}}},
	}
	if err := k8sClient.Create(ctx, user); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	sibling := createEndpoint(t, ns, "sibling", gw.Name, "/sibling")

	eventually(t, func() error {
		ep, err := getEndpoint(client.ObjectKeyFromObject(user))
		if err != nil {
			return err
		}
		if err := expectCondition(ep, v1alpha1.ConditionAccepted, metav1.ConditionFalse, v1alpha1.ReasonPolicyInvalid); err != nil {
			return err
		}
		c := meta.FindStatusCondition(ep.Status.Conditions, v1alpha1.ConditionAccepted)
		if !strings.Contains(c.Message, ns+"/refused") || strings.Contains(c.Message, "rejected by the integration test validator") {
			return fmt.Errorf("message %q must name %s/refused and quote none of its output", c.Message, ns)
		}
		other, err := getEndpoint(sibling)
		if err != nil {
			return err
		}
		return expectCondition(other, v1alpha1.ConditionAccepted, metav1.ConditionTrue, v1alpha1.ReasonAccepted)
	})
	eventually(t, func() error {
		got, applied, err := appliedConfig(gw)
		if err != nil {
			return err
		}
		if !strings.Contains(applied, `"/sibling"`) {
			return fmt.Errorf("applied config does not serve /sibling:\n%s", applied)
		}
		ex := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionEndpointsExcluded)
		if ex == nil || ex.Status != metav1.ConditionTrue || !strings.Contains(ex.Message, ns+"/uses-refused") {
			return fmt.Errorf("EndpointsExcluded = %+v, want True naming %s/uses-refused", ex, ns)
		}
		series, err := excludedEndpointsGauge(ns, gw.Name)
		if err != nil || len(series) != 1 || series[v1alpha1.ReasonPolicyInvalid] != 1 {
			return fmt.Errorf("gauge = %v (%v), want PolicyInvalid=1", series, err)
		}
		return nil
	})
}
