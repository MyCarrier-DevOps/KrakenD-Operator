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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

func TestBuildDragonfly_Full(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-gw", Namespace: "api"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13",
			Edition: v1alpha1.EditionEE,
			Config:  v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled:  true,
				Image:    "docker.dragonflydb.io/dragonflydb/dragonfly:v1.25.2",
				Replicas: ptr.To[int32](2),
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						"cpu":    resource.MustParse("250m"),
						"memory": resource.MustParse("512Mi"),
					},
					Limits: corev1.ResourceList{
						"cpu":    resource.MustParse("1"),
						"memory": resource.MustParse("2Gi"),
					},
				},
				Snapshot: &v1alpha1.DragonflySnapshotSpec{
					Cron: "*/30 * * * *",
					PersistentVolumeClaimSpec: &corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								"storage": resource.MustParse("10Gi"),
							},
						},
					},
				},
				Authentication: &v1alpha1.DragonflyAuthSpec{
					PasswordFromSecret: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "dragonfly-auth"},
						Key:                  "password",
					},
				},
				Args: []string{"--maxmemory", "2gb"},
			},
		},
	}

	df := &unstructured.Unstructured{Object: map[string]interface{}{}}
	BuildDragonfly(df, gw)

	if df.GetKind() != "Dragonfly" {
		t.Errorf("expected kind Dragonfly, got %s", df.GetKind())
	}
	if df.GroupVersionKind().Group != "dragonflydb.io" {
		t.Errorf("expected group dragonflydb.io, got %s", df.GroupVersionKind().Group)
	}

	labels := df.GetLabels()
	if labels["app.kubernetes.io/name"] != "dragonfly" {
		t.Error("expected dragonfly label, not krakend")
	}
	if labels["app.kubernetes.io/instance"] != "prod-gw-dragonfly" {
		t.Errorf("expected prod-gw-dragonfly instance label, got %s", labels["app.kubernetes.io/instance"])
	}

	spec, ok := df.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("expected spec map")
	}
	if spec["replicas"] != int64(2) {
		t.Errorf("expected 2 replicas, got %v", spec["replicas"])
	}
	if spec["image"] != "docker.dragonflydb.io/dragonflydb/dragonfly:v1.25.2" {
		t.Errorf("wrong image: %v", spec["image"])
	}

	res, _ := spec["resources"].(map[string]interface{})
	requests, _ := res["requests"].(map[string]interface{})
	if requests["cpu"] != "250m" {
		t.Errorf("expected cpu 250m, got %v", requests["cpu"])
	}

	snapshot, _ := spec["snapshot"].(map[string]interface{})
	if snapshot["cron"] != "*/30 * * * *" {
		t.Errorf("wrong cron: %v", snapshot["cron"])
	}

	pvcSpec, _ := snapshot["persistentVolumeClaimSpec"].(map[string]interface{})
	modes, _ := pvcSpec["accessModes"].([]interface{})
	if len(modes) != 1 || modes[0] != "ReadWriteOnce" {
		t.Errorf("wrong access modes: %v", modes)
	}

	auth, _ := spec["authentication"].(map[string]interface{})
	pwSecret, _ := auth["passwordFromSecret"].(map[string]interface{})
	if pwSecret["name"] != "dragonfly-auth" {
		t.Errorf("wrong auth secret name: %v", pwSecret["name"])
	}

	args, _ := spec["args"].([]interface{})
	if len(args) != 2 || args[0] != "--maxmemory" {
		t.Errorf("wrong args: %v", args)
	}
}

func TestBuildDragonfly_Minimal(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version:   "2.13",
			Edition:   v1alpha1.EditionEE,
			Config:    v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{Enabled: true},
		},
	}

	df := &unstructured.Unstructured{Object: map[string]interface{}{}}
	BuildDragonfly(df, gw)

	spec, ok := df.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("expected spec map")
	}
	if _, exists := spec["replicas"]; exists {
		t.Error("minimal spec should not have replicas")
	}
	if _, exists := spec["image"]; exists {
		t.Error("minimal spec should not have image")
	}
}

func TestDragonflyName(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw"},
	}
	if name := DragonflyName(gw); name != "my-gw-dragonfly" {
		t.Errorf("expected my-gw-dragonfly, got %s", name)
	}
}

func TestDragonflyServiceDNS(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "api"},
	}
	expected := "my-gw-dragonfly.api.svc.cluster.local:6379"
	if dns := DragonflyServiceDNS(gw); dns != expected {
		t.Errorf("expected %s, got %s", expected, dns)
	}
}

func TestDragonflyGVR(t *testing.T) {
	gvr := DragonflyGVR()
	if gvr.Group != "dragonflydb.io" || gvr.Version != "v1alpha1" || gvr.Resource != "dragonflies" {
		t.Errorf("unexpected GVR: %v", gvr)
	}
}

func TestBuildVirtualService(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-gw", Namespace: "api"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13",
			Edition: v1alpha1.EditionEE,
			Config:  v1alpha1.GatewayConfig{Port: 9090},
			Istio: &v1alpha1.IstioSpec{
				Enabled:  true,
				Hosts:    []string{"api.example.com"},
				Gateways: []string{"istio-system/main-gateway"},
			},
		},
	}

	vs := &unstructured.Unstructured{Object: map[string]interface{}{}}
	BuildVirtualService(vs, gw)

	if vs.GetKind() != "VirtualService" {
		t.Errorf("expected kind VirtualService, got %s", vs.GetKind())
	}
	if vs.GroupVersionKind().Group != "networking.istio.io" {
		t.Errorf("expected networking.istio.io, got %s", vs.GroupVersionKind().Group)
	}

	labels := vs.GetLabels()
	if labels["app.kubernetes.io/name"] != "krakend" {
		t.Error("VS should use StandardLabels (name=krakend)")
	}

	spec, _ := vs.Object["spec"].(map[string]interface{})
	hosts, _ := spec["hosts"].([]interface{})
	if len(hosts) != 1 || hosts[0] != "api.example.com" {
		t.Errorf("wrong hosts: %v", hosts)
	}

	gateways, _ := spec["gateways"].([]interface{})
	if len(gateways) != 1 || gateways[0] != "istio-system/main-gateway" {
		t.Errorf("wrong gateways: %v", gateways)
	}

	httpRoutes, _ := spec["http"].([]interface{})
	if len(httpRoutes) != 1 {
		t.Fatal("expected 1 HTTP route")
	}
	route, _ := httpRoutes[0].(map[string]interface{})
	routes, _ := route["route"].([]interface{})
	dest, _ := routes[0].(map[string]interface{})
	destination, _ := dest["destination"].(map[string]interface{})
	if destination["host"] != "prod-gw.api.svc.cluster.local" {
		t.Errorf("wrong destination host: %v", destination["host"])
	}
	portMap, _ := destination["port"].(map[string]interface{})
	if portMap["number"] != int64(9090) {
		t.Errorf("expected port 9090, got %v", portMap["number"])
	}
}

func TestBuildVirtualService_DefaultPort(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13",
			Config:  v1alpha1.GatewayConfig{},
			Istio: &v1alpha1.IstioSpec{
				Enabled: true,
				Hosts:   []string{"api.example.com"},
			},
		},
	}

	vs := &unstructured.Unstructured{Object: map[string]interface{}{}}
	BuildVirtualService(vs, gw)

	spec, _ := vs.Object["spec"].(map[string]interface{})
	httpRoutes, _ := spec["http"].([]interface{})
	route, _ := httpRoutes[0].(map[string]interface{})
	routes, _ := route["route"].([]interface{})
	dest, _ := routes[0].(map[string]interface{})
	destination, _ := dest["destination"].(map[string]interface{})
	portMap, _ := destination["port"].(map[string]interface{})
	if portMap["number"] != int64(8080) {
		t.Errorf("expected default port 8080, got %v", portMap["number"])
	}
}

func TestVirtualServiceGVR(t *testing.T) {
	gvr := VirtualServiceGVR()
	if gvr.Group != "networking.istio.io" || gvr.Version != "v1" || gvr.Resource != "virtualservices" {
		t.Errorf("unexpected GVR: %v", gvr)
	}
}

func TestBuildExternalSecret(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-gw", Namespace: "api"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13",
			Edition: v1alpha1.EditionEE,
			Config:  v1alpha1.GatewayConfig{},
			License: &v1alpha1.LicenseConfig{
				ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{
					Enabled: true,
					SecretStoreRef: v1alpha1.SecretStoreRef{
						Name: "vault-backend",
						Kind: "ClusterSecretStore",
					},
					RemoteRef: v1alpha1.ExternalRemoteRef{
						Key:      "secret/data/krakend/license",
						Property: "license",
					},
				},
			},
		},
	}

	es := &unstructured.Unstructured{Object: map[string]interface{}{}}
	BuildExternalSecret(es, gw)

	if es.GetKind() != "ExternalSecret" {
		t.Errorf("expected kind ExternalSecret, got %s", es.GetKind())
	}
	if es.GroupVersionKind().Group != "external-secrets.io" {
		t.Errorf("expected external-secrets.io, got %s", es.GroupVersionKind().Group)
	}

	labels := es.GetLabels()
	if labels["app.kubernetes.io/name"] != "krakend" {
		t.Error("ES should use StandardLabels")
	}

	spec, _ := es.Object["spec"].(map[string]interface{})
	if spec["refreshInterval"] != "1h" {
		t.Errorf("expected 1h refresh, got %v", spec["refreshInterval"])
	}

	storeRef, _ := spec["secretStoreRef"].(map[string]interface{})
	if storeRef["name"] != "vault-backend" {
		t.Errorf("wrong store name: %v", storeRef["name"])
	}
	if storeRef["kind"] != "ClusterSecretStore" {
		t.Errorf("wrong store kind: %v", storeRef["kind"])
	}

	target, _ := spec["target"].(map[string]interface{})
	if target["name"] != "prod-gw-license" {
		t.Errorf("wrong target name: %v", target["name"])
	}

	data, _ := spec["data"].([]interface{})
	if len(data) != 1 {
		t.Fatal("expected 1 data entry")
	}
	entry, _ := data[0].(map[string]interface{})
	remoteRef, _ := entry["remoteRef"].(map[string]interface{})
	if remoteRef["key"] != "secret/data/krakend/license" {
		t.Errorf("wrong remote key: %v", remoteRef["key"])
	}
	if remoteRef["property"] != "license" {
		t.Errorf("wrong remote property: %v", remoteRef["property"])
	}
}

func TestExternalSecretName(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw"},
	}
	if name := ExternalSecretName(gw); name != "my-gw-license" {
		t.Errorf("expected my-gw-license, got %s", name)
	}
}

func TestExternalSecretGVR(t *testing.T) {
	gvr := ExternalSecretGVR()
	if gvr.Group != "external-secrets.io" || gvr.Version != "v1" || gvr.Resource != "externalsecrets" {
		t.Errorf("unexpected GVR: %v", gvr)
	}
}
