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
	"net/http"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// autoConfigFetchingWithToken is an AutoConfig that fetches its spec from a URL
// with the bearer token in key token of Secret spec-token.
func autoConfigFetchingWithToken() *v1alpha1.KrakenDAutoConfig {
	return &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
			OpenAPI: v1alpha1.OpenAPISource{
				URL: "https://specs.example/api.json",
				Auth: &v1alpha1.AuthConfig{BearerTokenSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "spec-token"}, Key: "token"}},
			},
			Trigger: v1alpha1.TriggerOnChange,
		},
	}
}

// The operator sends the Secret in spec.openapi.auth as the Authorization
// header to spec.openapi.url, so the requester must be able to read it.
func TestAutoConfigAdmission_BearerTokenSecretNeedsGetOnThatSecret(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &AutoConfigValidator{Client: reviewingClient(false, &reviews, testGateway())}

	resp := review(t, v, "alice", autoConfigFetchingWithToken(), nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || causes[0].Field != "spec.openapi.auth.bearerTokenSecret.name" ||
		causes[0].Type != metav1.CauseTypeForbidden ||
		!strings.Contains(causes[0].Message, "may not get Secret spec-token") {
		t.Errorf("causes = %+v, want one Forbidden cause on the bearer token Secret's name", causes)
	}
	if len(reviews) != 1 {
		t.Errorf("%d reviews, want 1", len(reviews))
	}
}
