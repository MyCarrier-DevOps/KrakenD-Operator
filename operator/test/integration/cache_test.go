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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestManager_CachesNoSecretOrConfigMapContent sees only what the tests that
// ran before it caused, so it guards the suite as a whole rather than one
// scenario.
func TestManager_CachesNoSecretOrConfigMapContent(t *testing.T) {
	ns := testNamespace(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: ns},
		StringData: map[string]string{"token": "s3cret"},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: ns},
		Data:       map[string]string{"spec": "{}"},
	}
	for _, obj := range []client.Object{secret, cm} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	// Read through the manager's client, as the controllers do. A live
	// read sees the objects at once; a cached one would need an informer.
	var gotSecret corev1.Secret
	if err := mgrClient.Get(ctx, client.ObjectKeyFromObject(secret), &gotSecret); err != nil {
		t.Fatalf("get secret through the manager client: %v", err)
	}
	if string(gotSecret.Data["token"]) != "s3cret" {
		t.Errorf("secret data = %q", gotSecret.Data["token"])
	}
	var gotCM corev1.ConfigMap
	if err := mgrClient.Get(ctx, client.ObjectKeyFromObject(cm), &gotCM); err != nil {
		t.Fatalf("get configmap through the manager client: %v", err)
	}

	if seen := suiteCache.records(); len(seen) != 0 {
		t.Errorf("the manager cache was asked for typed Secret/ConfigMap objects: %v", seen)
	}
}
