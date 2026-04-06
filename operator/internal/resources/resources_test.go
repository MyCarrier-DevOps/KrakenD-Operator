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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/utils/ptr"
)

// === ConfigMap Tests ===

func TestBuildConfigMap(t *testing.T) {
	gw := testGateway()
	cm := &corev1.ConfigMap{}
	jsonData := []byte(`{"version":3}`)
	BuildConfigMap(cm, gw, jsonData)

	if cm.Labels["app.kubernetes.io/name"] != "krakend" {
		t.Error("expected standard labels")
	}
	if cm.Data["krakend.json"] != `{"version":3}` {
		t.Errorf("unexpected config data: %s", cm.Data["krakend.json"])
	}
}

// === ServiceAccount Tests ===

func TestBuildServiceAccount(t *testing.T) {
	gw := testGateway()
	sa := &corev1.ServiceAccount{}
	BuildServiceAccount(sa, gw)
	if sa.Labels["app.kubernetes.io/instance"] != "test-gw" {
		t.Error("expected standard labels")
	}
}

// === Service Tests ===

func TestBuildService_DefaultPort(t *testing.T) {
	gw := testGateway()
	svc := &corev1.Service{}
	BuildService(svc, gw)

	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("expected ClusterIP, got %s", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("expected port 8080, got %d", svc.Spec.Ports[0].Port)
	}
	if svc.Spec.Selector["app.kubernetes.io/instance"] != "test-gw" {
		t.Error("expected selector labels")
	}
}

func TestBuildService_CustomPort(t *testing.T) {
	gw := testGateway()
	gw.Spec.Config.Port = 9090
	svc := &corev1.Service{}
	BuildService(svc, gw)
	if svc.Spec.Ports[0].Port != 9090 {
		t.Errorf("expected port 9090, got %d", svc.Spec.Ports[0].Port)
	}
}

// === PDB Tests ===

func TestBuildPDB(t *testing.T) {
	gw := testGateway()
	pdb := &policyv1.PodDisruptionBudget{}
	BuildPDB(pdb, gw)

	if pdb.Spec.MaxUnavailable.IntVal != 1 {
		t.Errorf("expected maxUnavailable 1, got %d", pdb.Spec.MaxUnavailable.IntVal)
	}
	if pdb.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != "test-gw" {
		t.Error("expected selector labels")
	}
}

// === HPA Tests ===

func TestBuildHPA(t *testing.T) {
	gw := testGateway()
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		MinReplicas: ptr.To(int32(2)),
		MaxReplicas: 10,
		TargetCPU:   ptr.To(int32(80)),
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	BuildHPA(hpa, gw)

	if hpa.Spec.MaxReplicas != 10 {
		t.Errorf("expected maxReplicas 10, got %d", hpa.Spec.MaxReplicas)
	}
	if *hpa.Spec.MinReplicas != 2 {
		t.Errorf("expected minReplicas 2, got %d", *hpa.Spec.MinReplicas)
	}
	if hpa.Spec.ScaleTargetRef.Kind != "Deployment" {
		t.Errorf("expected target kind Deployment, got %s", hpa.Spec.ScaleTargetRef.Kind)
	}
	if hpa.Spec.ScaleTargetRef.Name != "test-gw" {
		t.Errorf("expected target name test-gw, got %s", hpa.Spec.ScaleTargetRef.Name)
	}
	if len(hpa.Spec.Metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(hpa.Spec.Metrics))
	}
	if *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization != 80 {
		t.Errorf("expected target CPU 80, got %d", *hpa.Spec.Metrics[0].Resource.Target.AverageUtilization)
	}
}

func TestBuildHPA_NoCPUTarget(t *testing.T) {
	gw := testGateway()
	gw.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{
		MaxReplicas: 5,
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	BuildHPA(hpa, gw)

	if len(hpa.Spec.Metrics) != 0 {
		t.Errorf("expected no metrics when TargetCPU is nil, got %d", len(hpa.Spec.Metrics))
	}
}
