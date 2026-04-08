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

package resources

import (
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testGateway() *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.7.0",
			Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
		},
	}
}

func TestStandardLabels(t *testing.T) {
	gw := testGateway()
	labels := StandardLabels(gw)
	if labels["app.kubernetes.io/name"] != "krakend" {
		t.Error("expected name krakend")
	}
	if labels["app.kubernetes.io/instance"] != "test-gw" {
		t.Errorf("expected instance test-gw, got %s", labels["app.kubernetes.io/instance"])
	}
	if labels["app.kubernetes.io/version"] != "2.7.0" {
		t.Errorf("expected version 2.7.0, got %s", labels["app.kubernetes.io/version"])
	}
	if labels["app.kubernetes.io/component"] != "gateway" {
		t.Error("expected component gateway")
	}
	if labels["app.kubernetes.io/managed-by"] != "krakend-operator" {
		t.Error("expected managed-by krakend-operator")
	}
}

func TestSelectorLabels(t *testing.T) {
	gw := testGateway()
	labels := SelectorLabels(gw)
	if len(labels) != 2 {
		t.Errorf("expected 2 selector labels, got %d", len(labels))
	}
	if labels["app.kubernetes.io/instance"] != "test-gw" {
		t.Error("expected instance test-gw")
	}
}

func TestDragonflyLabels(t *testing.T) {
	gw := testGateway()
	labels := DragonflyLabels(gw)
	if labels["app.kubernetes.io/name"] != "dragonfly" {
		t.Error("expected name dragonfly")
	}
	if labels["app.kubernetes.io/instance"] != "test-gw-dragonfly" {
		t.Errorf("expected instance test-gw-dragonfly, got %s", labels["app.kubernetes.io/instance"])
	}
	if _, ok := labels["app.kubernetes.io/component"]; ok {
		t.Error("dragonfly labels should not have component")
	}
	if _, ok := labels["app.kubernetes.io/version"]; ok {
		t.Error("dragonfly labels should not have version")
	}
}
