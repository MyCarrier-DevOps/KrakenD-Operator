//go:build integration

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

package integration

import (
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// rootRejecting is a gateway extraConfig the suite's validator rejects: its
// key carries marker, so the gateway root fails on its own and no endpoint
// is to blame.
func rootRejecting(marker string) *runtime.RawExtension {
	return &runtime.RawExtension{Raw: []byte(`{"test` + marker + `":{}}`)}
}

// createRejectedGateway creates a gateway whose root the suite's validator
// rejects (rootRejecting), and waits until the rejection is recorded in
// status.
func createRejectedGateway(t *testing.T, name, marker string) *v1alpha1.KrakenDGateway {
	t.Helper()
	ns := testNamespace(t)
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.9", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{ExtraConfig: rootRejecting(marker)},
		},
	}
	if err := k8sClient.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	eventually(t, func() error {
		var got v1alpha1.KrakenDGateway
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &got); err != nil {
			return err
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionConfigValid)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonGatewayRootInvalid {
			return fmt.Errorf("ConfigValid = %+v, want False/%s", cond, v1alpha1.ReasonGatewayRootInvalid)
		}
		return nil
	})
	return gw
}

func TestGateway_RejectedConfigSettles(t *testing.T) {
	gw := createRejectedGateway(t, "gw-rejected", rejectMarker)

	var settled v1alpha1.KrakenDGateway
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &settled); err != nil {
		t.Fatal(err)
	}
	rejections := suiteValidator.rejections.Load()

	// A hot loop re-validates and rewrites status continuously; a settled
	// gateway does neither while its inputs are unchanged.
	time.Sleep(10 * time.Second)

	var later v1alpha1.KrakenDGateway
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &later); err != nil {
		t.Fatal(err)
	}
	if later.ResourceVersion != settled.ResourceVersion {
		t.Errorf("gateway kept changing after the rejection (resourceVersion %s -> %s)",
			settled.ResourceVersion, later.ResourceVersion)
	}
	if n := suiteValidator.rejections.Load() - rejections; n != 0 {
		t.Errorf("the unchanged rejected config was validated %d more times", n)
	}
}

func TestGateway_OversizedValidationOutputIsRecorded(t *testing.T) {
	gw := createRejectedGateway(t, "gw-rejected-huge", rejectHugeMarker)

	var got v1alpha1.KrakenDGateway
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), &got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionConfigValid)
	if len(cond.Message) > 4096 {
		t.Errorf("ConfigValid message is %d bytes, want at most 4096", len(cond.Message))
	}
}
