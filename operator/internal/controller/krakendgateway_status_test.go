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

package controller

import (
	"context"
	"fmt"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
)

// countingValidator returns err from every Validate call and counts the
// calls, i.e. how often krakend check would have run. Its validation copy
// depends on the edition mode the way the real one does for a config with
// an EE-only wildcard endpoint.
type countingValidator struct {
	calls int
	err   error
}

func (v *countingValidator) Validate(context.Context, []byte) error {
	v.calls++
	return v.err
}

func (v *countingValidator) PrepareValidationCopy(jsonData []byte, eeWithoutFallback bool) ([]byte, error) {
	if eeWithoutFallback {
		return append([]byte("ee-validation-copy:"), jsonData...), nil
	}
	return jsonData, nil
}

// rejectedBy returns a validation error shaped like a krakend check
// rejection.
func rejectedBy(output string) *renderer.ValidationError {
	return &renderer.ValidationError{Output: output, Err: fmt.Errorf("exit status 1")}
}

// gatewayStatusWrites builds a fake client seeded with objs that records the
// phase of every gateway status update it receives.
func gatewayStatusWrites(objs ...client.Object) (client.Client, *[]v1alpha1.GatewayPhase) {
	var phases []v1alpha1.GatewayPhase
	c := fakeClientBuilder().
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.KrakenDGateway{}, &v1alpha1.KrakenDEndpoint{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context, c client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if gw, ok := obj.(*v1alpha1.KrakenDGateway); ok && sub == "status" {
					phases = append(phases, gw.Status.Phase)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	return c, &phases
}

func reconcileGateway(t *testing.T, r *KrakenDGatewayReconciler, gw *v1alpha1.KrakenDGateway) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)})
	return err
}

func getGateway(t *testing.T, c client.Client, gw *v1alpha1.KrakenDGateway) *v1alpha1.KrakenDGateway {
	t.Helper()
	var got v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gw), &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

// renderOutput returns a renderer that always renders the same config with
// the given checksum.
func renderOutput(checksum string) *mockRenderer {
	return &mockRenderer{output: &renderer.RenderOutput{
		JSON: []byte(`{"version":3,"checksum":"` + checksum + `"}`), Checksum: checksum, DesiredImage: "img:v1",
	}}
}

func TestGatewayReconcile_SteadyStateWritesNoStatus(t *testing.T) {
	gw := testGateway()
	c, phases := gatewayStatusWrites(gw)
	r := &KrakenDGatewayReconciler{
		Client: c, Scheme: testScheme(), Recorder: fakeRecorder(),
		Renderer: renderOutput("good"), Validator: &mockValidator{},
	}

	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}
	first := len(*phases)
	if err := reconcileGateway(t, r, gw); err != nil {
		t.Fatal(err)
	}

	if extra := (*phases)[first:]; len(extra) != 0 {
		t.Errorf("a reconcile with nothing to change wrote status %d times: %v", len(extra), extra)
	}
	for _, p := range *phases {
		if p == v1alpha1.PhasePending || p == v1alpha1.PhaseRendering || p == v1alpha1.PhaseValidating {
			t.Errorf("transient phase %q was written", p)
		}
	}
}
