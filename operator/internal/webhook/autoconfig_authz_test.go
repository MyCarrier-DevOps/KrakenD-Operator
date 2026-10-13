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
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

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

func TestAutoConfigAdmission_BasicAuthSecretNeedsGetOnThatSecret(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &AutoConfigValidator{Client: reviewingClient(false, &reviews, testGateway())}
	ac := autoConfigFetchingWithToken()
	ac.Spec.OpenAPI.Auth = &v1alpha1.AuthConfig{BasicAuthSecret: &v1alpha1.BasicAuthSecretRef{Name: "spec-login"}}

	resp := review(t, v, "alice", ac, nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || causes[0].Field != "spec.openapi.auth.basicAuthSecret.name" ||
		!strings.Contains(causes[0].Message, "may not get Secret spec-login") {
		t.Errorf("causes = %+v, want one Forbidden cause on the basic auth Secret's name", causes)
	}
	if len(reviews) != 1 {
		t.Errorf("%d reviews, want 1", len(reviews))
	}
}

// The review is of the requester, with their whole identity, to get the named
// Secret in the AutoConfig's namespace.
func TestAutoConfigAdmission_AuthReviewIsOfTheRequesterToGetThatSecret(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &AutoConfigValidator{Client: reviewingClient(true, &reviews, testGateway())}

	if _, err := v.ValidateCreate(requestAs(requesterWithGroupsAndExtra), autoConfigFetchingWithToken()); err != nil {
		t.Fatalf("denied: %v", err)
	}

	if len(reviews) != 1 {
		t.Fatalf("%d reviews, want 1", len(reviews))
	}
	spec, attrs := reviews[0].Spec, reviews[0].Spec.ResourceAttributes
	if spec.User != "alice" || attrs == nil || attrs.Namespace != "default" || attrs.Verb != "get" ||
		attrs.Resource != "secrets" || attrs.Group != "" || attrs.Name != "spec-token" {
		t.Errorf("review = %+v, want alice to get Secret spec-token in namespace default", spec)
	}
	if !slices.Equal(spec.Groups, []string{"team-a"}) || spec.UID != "u1" ||
		!slices.Equal(spec.Extra["scopes"], authorizationv1.ExtraValue{"x"}) {
		t.Errorf("review identity = %+v, want groups, UID and extra forwarded", spec)
	}
}

// Where the token goes is part of what the requester must be allowed to do: a
// changed spec.openapi with auth set is reviewed like a create, so a stored
// token cannot be sent to a new URL by someone who could not read it.
func TestAutoConfigAdmission_URLChangeWithStoredAuthIsReviewed(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &AutoConfigValidator{Client: reviewingClient(false, &reviews, testGateway())}
	edited := autoConfigFetchingWithToken()
	edited.Spec.OpenAPI.URL = "https://attacker.example/api.json"

	resp := review(t, v, "alice", edited, autoConfigFetchingWithToken())

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity || len(reviews) != 1 {
		t.Errorf("response = %+v after %d reviews, want a 422 denial after one review", resp.Result, len(reviews))
	}
}

// A change to spec.openapi that moves where a stored Secret goes is reviewed:
// widening the reachable hosts, or switching from a ConfigMap, which sends no
// auth, to a URL, which does.
func TestAutoConfigAdmission_SourceChangesThatSendAStoredSecretAreReviewed(t *testing.T) {
	widened := autoConfigFetchingWithToken()
	widened.Spec.OpenAPI.AllowClusterLocal = true
	fromConfigMap := autoConfigFetchingWithToken()
	fromConfigMap.Spec.OpenAPI.URL = ""
	fromConfigMap.Spec.OpenAPI.ConfigMapRef = &v1alpha1.ConfigMapKeyRef{Name: "spec", Key: "api.json"}
	cases := map[string]struct{ old, edited *v1alpha1.KrakenDAutoConfig }{
		"cluster-local hosts allowed": {old: autoConfigFetchingWithToken(), edited: widened},
		"ConfigMap source before":     {old: fromConfigMap, edited: autoConfigFetchingWithToken()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &AutoConfigValidator{Client: reviewingClient(false, &reviews, testGateway())}

			resp := review(t, v, "alice", tc.edited, tc.old)

			if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity || len(reviews) != 1 {
				t.Errorf("response = %+v after %d reviews, want a 422 denial after one review", resp.Result, len(reviews))
			}
		})
	}
}

// A source the requester did not touch is not reviewed again, and a ConfigMap
// source, which never sends auth, is not reviewed at all.
func TestAutoConfigAdmission_UnchangedOrConfigMapSourceIsNotReviewed(t *testing.T) {
	periodic := autoConfigFetchingWithToken()
	periodic.Spec.Trigger = v1alpha1.TriggerPeriodic
	periodic.Spec.Periodic = &v1alpha1.PeriodicSpec{Interval: metav1.Duration{Duration: time.Minute}}
	configMap := autoConfigFetchingWithToken()
	configMap.Spec.OpenAPI.URL = ""
	configMap.Spec.OpenAPI.ConfigMapRef = &v1alpha1.ConfigMapKeyRef{Name: "spec", Key: "api.json"}
	cases := map[string]struct{ old, edited *v1alpha1.KrakenDAutoConfig }{
		"only the trigger changed": {old: autoConfigFetchingWithToken(), edited: periodic},
		"a new ConfigMap source":   {old: nil, edited: configMap},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &AutoConfigValidator{Client: reviewingClient(false, &reviews, testGateway())}

			var old runtime.Object // nil on a create
			if tc.old != nil {
				old = tc.old
			}

			resp := review(t, v, "alice", tc.edited, old)

			if !resp.Allowed || len(reviews) != 0 {
				t.Errorf("response = %+v after %d reviews, want it admitted without a review", resp.Result, len(reviews))
			}
		})
	}
}
