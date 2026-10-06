/*
Copyright 2026.

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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	"github.com/mycarrier-devops/krakend-operator/internal/util/hash"
)

// What the pods load is the payload, not the metadata. Whoever forged the
// ConfigMap at the applied config's name, and however it looks, the Deployment
// ends up mounting the config that name addresses.
func TestGatewayReconcile_AForgedAppliedConfigMapIsReplacedWhateverItsMetadata(t *testing.T) {
	const config = `{"version":3,"name":"genuine"}`
	checksum := hash.SHA256Hex([]byte(config))
	forgeries := map[string]func(cm *corev1.ConfigMap){
		"owner reference removed": func(cm *corev1.ConfigMap) { cm.OwnerReferences = nil },
		"checksum annotation changed": func(cm *corev1.ConfigMap) {
			cm.Annotations[resources.PostRestartJobChecksumAnnotation] = "something-else"
		},
	}
	for name, forge := range forgeries {
		t.Run(name, func(t *testing.T) {
			gw := servingGateway(checksum, convergedImage)
			forged := publishedConfigMap(t, gw, `{"version":3,"name":"forged"}`, checksum)
			forge(forged)
			c := fakeClientBuilder().WithObjects(gw, settledDeployment(gw, checksum), forged).
				WithStatusSubresource(gw).Build()
			r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

			for range 3 {
				if err := reconcileGateway(t, r, gw); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			}

			mounted := mountedConfig(t, c, gw)
			var cm corev1.ConfigMap
			getObject(t, c, gw, mounted, &cm)
			if got := cm.Data[resources.ConfigKey]; got != config {
				t.Errorf("the Deployment mounts %s, which holds %q; want the config its name addresses", mounted, got)
			}
			cv := meta.FindStatusCondition(getGateway(t, c, gw).Status.Conditions, v1alpha1.ConditionConfigValid)
			if cv == nil || cv.Status != metav1.ConditionTrue || cv.Reason != v1alpha1.ReasonConfigApplied {
				t.Errorf("ConfigValid = %+v, want True/ConfigApplied once the ConfigMap holds the config", cv)
			}
		})
	}
}

// A ConfigMap deleted for holding another payload leaves a trace: on the applied
// path the pass otherwise ends ConfigApplied. It is one Warning on the gateway,
// naming the ConfigMap, and later passes do not repeat it.
func TestGatewayReconcile_DeletingAForgedConfigMapWarnsOnTheGateway(t *testing.T) {
	const config = `{"version":3,"name":"genuine"}`
	checksum := hash.SHA256Hex([]byte(config))
	gw := servingGateway(checksum, convergedImage)
	forged := publishedConfigMap(t, gw, `{"version":3,"name":"forged"}`, checksum)
	c := fakeClientBuilder().WithObjects(gw, settledDeployment(gw, checksum), forged).
		WithStatusSubresource(gw).Build()
	r := newTestGatewayReconciler(c, renderOf(config), &mockValidator{})

	for range 3 {
		if err := reconcileGateway(t, r, gw); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	var tampered []string
	for _, e := range drainEvents(r.Recorder.(*record.FakeRecorder)) {
		if strings.Contains(e, " "+v1alpha1.ReasonConfigMapTampered+" ") {
			tampered = append(tampered, e)
		}
	}
	if len(tampered) != 1 {
		t.Fatalf("%s events over three passes = %v, want exactly 1", v1alpha1.ReasonConfigMapTampered, tampered)
	}
	if !strings.HasPrefix(tampered[0], corev1.EventTypeWarning+" ") ||
		!strings.Contains(tampered[0], forged.Name) {
		t.Errorf("event = %q, want a Warning naming the deleted ConfigMap %s", tampered[0], forged.Name)
	}
}
