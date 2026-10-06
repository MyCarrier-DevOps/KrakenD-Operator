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

package webhook

import (
	"context"
	"net/http"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// reviewingClient answers every SubjectAccessReview with allowed and records
// the reviews it was asked for.
func reviewingClient(allowed bool, reviews *[]authorizationv1.SubjectAccessReview) client.Client {
	return fakeClientBuilderWith(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			sar, ok := obj.(*authorizationv1.SubjectAccessReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			*reviews = append(*reviews, *sar.DeepCopy())
			sar.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed}
			return nil
		},
	})
}

// gatewayWithJob is a gateway whose post-restart Job is enabled and edited by
// mutate.
func gatewayWithJob(mutate func(*v1alpha1.PostRestartJobSpec)) *v1alpha1.KrakenDGateway {
	gw := testGateway()
	gw.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "true"}
	mutate(gw.Spec.PostRestartJob)
	return gw
}

func secretEnvFrom(name string) []corev1.EnvFromSource {
	return []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}
}

// A post-restart Job that runs as another ServiceAccount, or reads a Secret
// into its environment, gets rights the requester may not hold: the requester
// must be able to create pods in the namespace.
func TestGatewayAdmission_PostRestartJobNeedsPodCreateRights(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}
	gw := gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) { p.ServiceAccountName = "namespace-admin" })

	resp := review(t, v, "alice", gw, nil)

	if resp.Allowed || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("response = %+v, want a 403 denial", resp.Result)
	}
}
