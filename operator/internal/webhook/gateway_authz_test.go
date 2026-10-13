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
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
)

// reviewingClient answers every SubjectAccessReview with allowed and records
// the reviews it was asked for. objs are stored objects it serves.
func reviewingClient(
	allowed bool, reviews *[]authorizationv1.SubjectAccessReview, objs ...client.Object,
) client.Client {
	return reviewingClientBy(func(*authorizationv1.ResourceAttributes) bool { return allowed }, reviews, objs...)
}

// reviewingClientBy answers each SubjectAccessReview with allow's verdict on
// its resource attributes and records the reviews it was asked for.
func reviewingClientBy(
	allow func(*authorizationv1.ResourceAttributes) bool,
	reviews *[]authorizationv1.SubjectAccessReview, objs ...client.Object,
) client.Client {
	return fakeClientBuilderWith(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			sar, ok := obj.(*authorizationv1.SubjectAccessReview)
			if !ok {
				return c.Create(ctx, obj, opts...)
			}
			*reviews = append(*reviews, *sar.DeepCopy())
			sar.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allow(sar.Spec.ResourceAttributes)}
			return nil
		},
	}, objs...)
}

// gatewayWithJob is a gateway whose post-restart Job is enabled and edited by
// mutate.
func gatewayWithJob(mutate func(*v1alpha1.PostRestartJobSpec)) *v1alpha1.KrakenDGateway {
	gw := testGateway()
	gw.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "true"}
	mutate(gw.Spec.PostRestartJob)
	return gw
}

// eeGatewayWithLicenseSecret is an Enterprise gateway whose license is read
// from key LICENSE of Secret name.
func eeGatewayWithLicenseSecret(name string) *v1alpha1.KrakenDGateway {
	gw := testGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.License = &v1alpha1.LicenseConfig{SecretRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "LICENSE"}}
	return gw
}

// eeGatewayWithLicenseExternalSecret is an Enterprise gateway whose license is
// pulled by an ExternalSecret from the vault ClusterSecretStore.
func eeGatewayWithLicenseExternalSecret() *v1alpha1.KrakenDGateway {
	gw := testGateway()
	gw.Spec.Edition = v1alpha1.EditionEE
	gw.Spec.License = &v1alpha1.LicenseConfig{ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{
		Enabled:        true,
		SecretStoreRef: v1alpha1.SecretStoreRef{Name: "vault", Kind: "ClusterSecretStore"},
		RemoteRef:      v1alpha1.ExternalRemoteRef{Key: "krakend/license"},
	}}
	return gw
}

// gatewayWithDragonfly is a gateway with the managed Dragonfly enabled, whose
// password is read from key password of Secret passwordSecret when that is
// not empty.
func gatewayWithDragonfly(edition v1alpha1.Edition, passwordSecret string) *v1alpha1.KrakenDGateway {
	gw := testGateway()
	gw.Spec.Edition = edition
	gw.Spec.Dragonfly = &v1alpha1.DragonflySpec{Enabled: true}
	if passwordSecret != "" {
		gw.Spec.Dragonfly.Authentication = &v1alpha1.DragonflyAuthSpec{PasswordFromSecret: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: passwordSecret}, Key: "password"}}
	}
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

func TestGatewayAdmission_PostRestartJobReviewIsOfTheRequesterToCreatePods(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(true, &reviews), Checker: &scriptedChecker{}}
	gw := gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) { p.ServiceAccountName = "namespace-admin" })

	resp := review(t, v, "alice", gw, nil)

	if !resp.Allowed {
		t.Fatalf("denied: %+v", resp.Result)
	}
	if len(reviews) != 1 {
		t.Fatalf("%d reviews, want 1", len(reviews))
	}
	spec, attrs := reviews[0].Spec, reviews[0].Spec.ResourceAttributes
	if spec.User != "alice" || attrs == nil || attrs.Namespace != "default" || attrs.Verb != "create" ||
		attrs.Resource != "pods" || attrs.Group != "" {
		t.Errorf("review = %+v, want alice to create pods in namespace default", spec)
	}
}

// requesterWithGroupsAndExtra is the identity of a requester whose access
// comes through a group (cluster admins through system:masters) or through
// extra attributes.
var requesterWithGroupsAndExtra = authenticationv1.UserInfo{
	Username: "alice", UID: "u1", Groups: []string{"team-a"},
	Extra: map[string]authenticationv1.ExtraValue{"scopes": {"x"}},
}

// requestAs is a context that carries an admission request made by user.
func requestAs(user authenticationv1.UserInfo) context.Context {
	return admission.NewContextWithRequest(context.Background(),
		admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UserInfo: user}})
}

// A requester whose access comes through a group or through extra attributes
// is reviewed with them: every review forwards the whole identity, not just
// the user name.
func TestGatewayAdmission_ReviewsCarryTheRequestersGroupsAndExtra(t *testing.T) {
	cases := map[string]*v1alpha1.KrakenDGateway{
		"a post-restart Job": gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) {
			p.ServiceAccountName = "namespace-admin"
		}),
		"a license secretRef":         eeGatewayWithLicenseSecret("team-license"),
		"a license ExternalSecret":    eeGatewayWithLicenseExternalSecret(),
		"a Dragonfly password Secret": gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds"),
	}
	for name, gw := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(true, &reviews), Checker: &scriptedChecker{}}

			if _, err := v.ValidateCreate(requestAs(requesterWithGroupsAndExtra), gw); err != nil {
				t.Fatalf("denied: %v", err)
			}

			if len(reviews) != 1 {
				t.Fatalf("%d reviews, want 1", len(reviews))
			}
			spec := reviews[0].Spec
			if !slices.Equal(spec.Groups, []string{"team-a"}) {
				t.Errorf("review groups = %v, want [team-a]", spec.Groups)
			}
			if spec.UID != "u1" {
				t.Errorf("review UID = %q, want u1", spec.UID)
			}
			if !slices.Equal(spec.Extra["scopes"], authorizationv1.ExtraValue{"x"}) {
				t.Errorf("review extra = %v, want scopes=[x]", spec.Extra)
			}
		})
	}
}

func TestGatewayAdmission_PostRestartJobSecretReferencesNeedPodCreateRights(t *testing.T) {
	cases := map[string]func(*v1alpha1.PostRestartJobSpec){
		"envFrom a Secret": func(p *v1alpha1.PostRestartJobSpec) { p.EnvFrom = secretEnvFrom("db-credentials") },
		"env secretKeyRef": func(p *v1alpha1.PostRestartJobSpec) {
			p.Env = []corev1.EnvVar{{Name: "PW", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "db-credentials"}, Key: "password"}}}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

			resp := review(t, v, "alice", gatewayWithJob(mutate), nil)

			if resp.Allowed || resp.Result.Code != http.StatusForbidden {
				t.Errorf("response = %+v, want a 403 denial", resp.Result)
			}
		})
	}
}

// A post-restart Job the requester did not touch is not reviewed again: a
// user who may edit the gateway but not create pods can still scale it.
func TestGatewayAdmission_UnchangedPostRestartJobIsNotReviewedAgain(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}
	borrow := func(p *v1alpha1.PostRestartJobSpec) { p.ServiceAccountName = "namespace-admin" }
	old := gatewayWithJob(borrow)
	edited := gatewayWithJob(borrow)
	edited.Spec.Replicas = ptr.To[int32](3)

	resp := review(t, v, "alice", edited, old)

	if !resp.Allowed || len(reviews) != 0 {
		t.Errorf("response = %+v after %d reviews, want it admitted without a review", resp.Result, len(reviews))
	}
}

// An update that turns an existing gateway's Job into one that borrows rights,
// or switches it to another borrowed account, is reviewed like a create: the
// requester must be able to create pods.
func TestGatewayAdmission_UpdateThatIntroducesABorrowingJobNeedsPodCreateRights(t *testing.T) {
	cases := map[string]*v1alpha1.KrakenDGateway{
		"no Job before":        testGateway(),
		"a default Job before": gatewayWithJob(func(*v1alpha1.PostRestartJobSpec) {}),
		"a disabled borrowing Job before": gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) {
			p.Enabled, p.ServiceAccountName = false, "namespace-admin"
		}),
		"another borrowed ServiceAccount before": gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) {
			p.ServiceAccountName = "a"
		}),
	}
	for name, old := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}
			edited := gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) { p.ServiceAccountName = "namespace-admin" })

			resp := review(t, v, "alice", edited, old)

			if resp.Allowed || resp.Result.Code != http.StatusForbidden || len(reviews) != 1 {
				t.Errorf("response = %+v after %d reviews, want a 403 denial after one review", resp.Result, len(reviews))
			}
		})
	}
}

// A Job that gets nothing the gateway Deployment does not already have needs no
// review, even from a requester who may not create pods.
func TestGatewayAdmission_DefaultPostRestartJobNeedsNoReview(t *testing.T) {
	disabled := gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) {
		p.Enabled, p.ServiceAccountName, p.EnvFrom = false, "namespace-admin", secretEnvFrom("db-credentials")
	})
	cases := map[string]*v1alpha1.KrakenDGateway{
		"no ServiceAccount, no Secrets": gatewayWithJob(func(*v1alpha1.PostRestartJobSpec) {}),
		"the gateway's own ServiceAccount": gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) {
			p.ServiceAccountName = "gw"
		}),
		"a ConfigMap envFrom": gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) {
			p.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}}}
		}),
		"a disabled Job": disabled,
		"no Job":         testGateway(),
	}
	for name, gw := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

			resp := review(t, v, "alice", gw, nil)

			if !resp.Allowed || len(reviews) != 0 {
				t.Errorf("response = %+v after %d reviews, want it admitted without a review", resp.Result, len(reviews))
			}
		})
	}
}

// A review that cannot be made is not a verdict: a transient 500.
func TestGatewayAdmission_PostRestartJobReviewFailureIs500(t *testing.T) {
	c := fakeClientBuilderWith(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errors.New("the server could not process the review")
		},
	})
	gw := gatewayWithJob(func(p *v1alpha1.PostRestartJobSpec) { p.ServiceAccountName = "namespace-admin" })

	resp := review(t, &GatewayValidator{Client: c, Checker: &scriptedChecker{}}, "alice", gw, nil)

	if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
		t.Errorf("response = %+v, want 500", resp.Result)
	}
}

// A post-restart Job that relaxes the operator's default security context is
// reviewed like one that borrows a ServiceAccount: the requester must be able
// to create pods. Anything outside the allow-list counts, so a setting the
// list does not name is reviewed too.
func TestGatewayAdmission_PostRestartJobSecurityRelaxationNeedsPodCreateRights(t *testing.T) {
	root := &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](0), RunAsNonRoot: ptr.To(false)}
	cases := map[string]func(*v1alpha1.PostRestartJobSpec){
		"privileged root container": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true), AllowPrivilegeEscalation: ptr.To(true)}
			p.PodSecurityContext = root
		},
		"root with SYS_ADMIN added": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}
			p.PodSecurityContext = root
		},
		"a drop list without ALL": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"NET_RAW"}}}
		},
		"AppArmor unconfined field": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}}
		},
		"AppArmor unconfined annotation": func(p *v1alpha1.PostRestartJobSpec) {
			p.PodAnnotations = map[string]string{
				corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "post-restart": "unconfined"}
		},
		"seccomp unconfined at pod scope": func(p *v1alpha1.PostRestartJobSpec) {
			p.PodSecurityContext = &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}}
		},
		"a field outside the allow-list (procMount)": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{ProcMount: ptr.To(corev1.UnmaskedProcMount)}
		},
		"sysctls": func(p *v1alpha1.PostRestartJobSpec) {
			p.PodSecurityContext = &corev1.PodSecurityContext{
				Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_forward", Value: "1"}}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

			resp := review(t, v, "alice", gatewayWithJob(mutate), nil)

			if resp.Allowed || resp.Result.Code != http.StatusForbidden || len(reviews) != 1 {
				t.Errorf("response = %+v after %d reviews, want a 403 denial after one review", resp.Result, len(reviews))
			}
		})
	}
}

// Settings on the allow-list grant no privilege, so they need no review, root
// included (it is an acknowledged choice). Neither do podLabels and podAnnotations
// other than the AppArmor one.
func TestGatewayAdmission_PostRestartJobAllowListedSecurityContextNeedsNoReview(t *testing.T) {
	cases := map[string]func(*v1alpha1.PostRestartJobSpec){
		"acknowledged root": func(p *v1alpha1.PostRestartJobSpec) {
			p.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](0), RunAsNonRoot: ptr.To(false)}
		},
		"restated hardening": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(false),
				Capabilities:    &corev1.Capabilities{Drop: []corev1.Capability{"ALL", "NET_RAW"}},
				SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault},
			}
			p.PodSecurityContext = &corev1.PodSecurityContext{
				FSGroup: ptr.To[int64](2000), SupplementalGroups: []int64{3000}}
		},
		"neutral restatements: privileged false, procMount Default": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{
				Privileged: ptr.To(false), ProcMount: ptr.To(corev1.DefaultProcMount)}
		},
		"the operator's own default restated": func(p *v1alpha1.PostRestartJobSpec) {
			p.SecurityContext = &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
				Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
			p.PodSecurityContext = &corev1.PodSecurityContext{
				RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](1000),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
		},
		"runtime/default annotation, other annotations and labels": func(p *v1alpha1.PostRestartJobSpec) {
			p.PodAnnotations = map[string]string{
				corev1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix + "post-restart": "runtime/default",
				"sidecar.istio.io/inject": "false"}
			p.PodLabels = map[string]string{"team": "a"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

			resp := review(t, v, "alice", gatewayWithJob(mutate), nil)

			if !resp.Allowed || len(reviews) != 0 {
				t.Errorf("response = %+v after %d reviews, want it admitted without a review", resp.Result, len(reviews))
			}
		})
	}
}

// The gateway mounts spec.license.secretRef into the container that runs the
// writer's image, so the requester must be able to read that Secret. The denial
// is a 422 on the field.
func TestGatewayAdmission_LicenseSecretRefNeedsGetOnThatSecret(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", eeGatewayWithLicenseSecret("team-license"), nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || causes[0].Field != "spec.license.secretRef.name" ||
		causes[0].Type != metav1.CauseTypeForbidden ||
		!strings.Contains(causes[0].Message, "may not get Secret team-license") {
		t.Errorf("causes = %+v, want one Forbidden cause on spec.license.secretRef.name naming the Secret", causes)
	}
	if len(reviews) != 1 {
		t.Errorf("%d reviews, want 1", len(reviews))
	}
}

func TestGatewayAdmission_LicenseSecretRefReviewIsOfTheRequesterToGetThatSecret(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(true, &reviews), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", eeGatewayWithLicenseSecret("team-license"), nil)

	if !resp.Allowed {
		t.Fatalf("denied: %+v", resp.Result)
	}
	if len(reviews) != 1 {
		t.Fatalf("%d reviews, want 1", len(reviews))
	}
	spec, attrs := reviews[0].Spec, reviews[0].Spec.ResourceAttributes
	if spec.User != "alice" || attrs == nil || attrs.Namespace != "default" || attrs.Verb != "get" ||
		attrs.Resource != "secrets" || attrs.Group != "" || attrs.Name != "team-license" {
		t.Errorf("review = %+v, want alice to get Secret team-license in namespace default", spec)
	}
}

// A Secret reference the requester did not touch is not reviewed again: a user
// who may edit the gateway but not read its Secrets can still tune it.
func TestGatewayAdmission_UnchangedSecretReferencesAreNotReviewedAgain(t *testing.T) {
	edit := func(mutate func(*v1alpha1.KrakenDGateway)) struct{ old, edited *v1alpha1.KrakenDGateway } {
		edited := eeGatewayWithLicenseSecret("team-license")
		mutate(edited)
		return struct{ old, edited *v1alpha1.KrakenDGateway }{eeGatewayWithLicenseSecret("team-license"), edited}
	}
	cases := map[string]struct{ old, edited *v1alpha1.KrakenDGateway }{
		"license secretRef, replicas changed": edit(func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Replicas = ptr.To[int32](3)
		}),
		"license secretRef, images changed": edit(func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.Image, gw.Spec.CEImage = "registry.example/krakend-ee:2.13.1", "registry.example/krakend:2.13.1"
		}),
		"license secretRef, fallback and expiry settings changed": edit(func(gw *v1alpha1.KrakenDGateway) {
			gw.Spec.License.FallbackToCE, gw.Spec.License.ExpiryWarningDays = true, 14
		}),
		"license ExternalSecret, replicas changed": {
			old: eeGatewayWithLicenseExternalSecret(),
			edited: func() *v1alpha1.KrakenDGateway {
				gw := eeGatewayWithLicenseExternalSecret()
				gw.Spec.Replicas = ptr.To[int32](3)
				return gw
			}(),
		},
		"license ExternalSecret disabled": {
			old: eeGatewayWithLicenseExternalSecret(),
			edited: func() *v1alpha1.KrakenDGateway {
				gw := eeGatewayWithLicenseExternalSecret()
				gw.Spec.License.ExternalSecret.Enabled = false
				return gw
			}(),
		},
		"Dragonfly password, image and replicas changed": {
			old: gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds"),
			edited: func() *v1alpha1.KrakenDGateway {
				gw := gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds")
				gw.Spec.Dragonfly.Image, gw.Spec.Dragonfly.Replicas = "registry.example/dragonfly:1.2", ptr.To[int32](2)
				return gw
			}(),
		},
		"Dragonfly disabled": {
			old: gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds"),
			edited: func() *v1alpha1.KrakenDGateway {
				gw := gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds")
				gw.Spec.Dragonfly.Enabled = false
				return gw
			}(),
		},
		"license removed": {
			old: eeGatewayWithLicenseSecret("team-license"),
			edited: func() *v1alpha1.KrakenDGateway {
				gw := eeGatewayWithLicenseSecret("team-license")
				gw.Spec.License = nil
				return gw
			}(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

			resp := review(t, v, "alice", tc.edited, tc.old)

			if !resp.Allowed || len(reviews) != 0 {
				t.Errorf("response = %+v after %d reviews, want it admitted without a review", resp.Result, len(reviews))
			}
		})
	}
}

// An update that adds or changes a Secret reference is reviewed like a create:
// the requester must have the right the reference now needs.
func TestGatewayAdmission_UpdateThatChangesASecretReferenceIsReviewed(t *testing.T) {
	withLicense := func(mutate func(*v1alpha1.KrakenDGateway)) *v1alpha1.KrakenDGateway {
		gw := eeGatewayWithLicenseSecret("team-license")
		mutate(gw)
		return gw
	}
	withStore := func(mutate func(*v1alpha1.ExternalSecretLicenseConfig)) *v1alpha1.KrakenDGateway {
		gw := eeGatewayWithLicenseExternalSecret()
		mutate(&gw.Spec.License.ExternalSecret)
		return gw
	}
	noLicense := testGateway()
	noLicense.Spec.Edition = v1alpha1.EditionEE
	dragonflyOff := gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds")
	dragonflyOff.Spec.Dragonfly.Enabled = false
	cases := map[string]struct{ old, edited *v1alpha1.KrakenDGateway }{
		"Dragonfly enabled with a stored password": {dragonflyOff, gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds")},
		"another Dragonfly password Secret": {gatewayWithDragonfly(v1alpha1.EditionEE, "other-creds"),
			gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds")},
		"a password added to a stored Dragonfly": {gatewayWithDragonfly(v1alpha1.EditionCE, ""),
			func() *v1alpha1.KrakenDGateway {
				gw := gatewayWithDragonfly(v1alpha1.EditionCE, "db-creds")
				gw.Spec.Dragonfly.Image = "evil:1"
				return gw
			}()},
		"no license before": {noLicense, eeGatewayWithLicenseSecret("team-license")},
		"another Secret": {eeGatewayWithLicenseSecret("other-license"),
			eeGatewayWithLicenseSecret("team-license")},
		"another key": {withLicense(func(gw *v1alpha1.KrakenDGateway) { gw.Spec.License.SecretRef.Key = "license.jwt" }),
			eeGatewayWithLicenseSecret("team-license")},
		"an empty license": {withLicense(func(gw *v1alpha1.KrakenDGateway) { gw.Spec.License = &v1alpha1.LicenseConfig{} }),
			eeGatewayWithLicenseSecret("team-license")},
		"an ExternalSecret before": {eeGatewayWithLicenseExternalSecret(), eeGatewayWithLicenseSecret("team-license")},
		"a secretRef before":       {eeGatewayWithLicenseSecret("team-license"), eeGatewayWithLicenseExternalSecret()},
		"ExternalSecret enabled": {
			withStore(func(es *v1alpha1.ExternalSecretLicenseConfig) { es.Enabled = false }),
			eeGatewayWithLicenseExternalSecret()},
		"another store": {
			withStore(func(es *v1alpha1.ExternalSecretLicenseConfig) { es.SecretStoreRef.Name = "other" }),
			eeGatewayWithLicenseExternalSecret()},
		"another store kind": {
			withStore(func(es *v1alpha1.ExternalSecretLicenseConfig) { es.SecretStoreRef.Kind = "SecretStore" }),
			eeGatewayWithLicenseExternalSecret()},
		"another remote key": {
			withStore(func(es *v1alpha1.ExternalSecretLicenseConfig) { es.RemoteRef.Key = "other/license" }),
			eeGatewayWithLicenseExternalSecret()},
		"another remote property": {
			withStore(func(es *v1alpha1.ExternalSecretLicenseConfig) { es.RemoteRef.Property = "license" }),
			eeGatewayWithLicenseExternalSecret()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var reviews []authorizationv1.SubjectAccessReview
			v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

			resp := review(t, v, "alice", tc.edited, tc.old)

			if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity || len(reviews) != 1 {
				t.Errorf("response = %+v after %d reviews, want a 422 denial after one review", resp.Result, len(reviews))
			}
		})
	}
}

// The operator creates the license ExternalSecret, which pulls a key of the
// writer's choice from a store, so the requester must be able to create
// ExternalSecrets in the namespace.
func TestGatewayAdmission_ExternalSecretLicenseNeedsCreateExternalSecrets(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", eeGatewayWithLicenseExternalSecret(), nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || causes[0].Field != "spec.license.externalSecret" ||
		causes[0].Type != metav1.CauseTypeForbidden ||
		!strings.Contains(causes[0].Message, "may not create externalsecrets.external-secrets.io") {
		t.Errorf("causes = %+v, want one Forbidden cause on spec.license.externalSecret", causes)
	}
	if len(reviews) != 1 {
		t.Errorf("%d reviews, want 1", len(reviews))
	}
}

func TestGatewayAdmission_ExternalSecretLicenseReviewIsOfTheRequesterToCreateExternalSecrets(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(true, &reviews), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", eeGatewayWithLicenseExternalSecret(), nil)

	if !resp.Allowed {
		t.Fatalf("denied: %+v", resp.Result)
	}
	if len(reviews) != 1 {
		t.Fatalf("%d reviews, want 1", len(reviews))
	}
	spec, attrs := reviews[0].Spec, reviews[0].Spec.ResourceAttributes
	if spec.User != "alice" || attrs == nil || attrs.Namespace != "default" || attrs.Verb != "create" ||
		attrs.Group != "external-secrets.io" || attrs.Resource != "externalsecrets" || attrs.Name != "" {
		t.Errorf("review = %+v, want alice to create externalsecrets.external-secrets.io in namespace default", spec)
	}
}

// The Dragonfly operator puts spec.dragonfly.authentication.passwordFromSecret
// into the environment of the container that runs the writer's Dragonfly image,
// so the requester must be able to read that Secret.
func TestGatewayAdmission_DragonflyPasswordSecretNeedsGetOnThatSecret(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(false, &reviews), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds"), nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || causes[0].Field != "spec.dragonfly.authentication.passwordFromSecret.name" ||
		causes[0].Type != metav1.CauseTypeForbidden ||
		!strings.Contains(causes[0].Message, "may not get Secret db-creds") {
		t.Errorf("causes = %+v, want one Forbidden cause on the password Secret's name", causes)
	}
	if len(reviews) != 1 {
		t.Errorf("%d reviews, want 1", len(reviews))
	}
}

func TestGatewayAdmission_DragonflyPasswordReviewIsOfTheRequesterToGetThatSecret(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	v := &GatewayValidator{Client: reviewingClient(true, &reviews), Checker: &scriptedChecker{}}

	resp := review(t, v, "alice", gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds"), nil)

	if !resp.Allowed {
		t.Fatalf("denied: %+v", resp.Result)
	}
	if len(reviews) != 1 {
		t.Fatalf("%d reviews, want 1", len(reviews))
	}
	spec, attrs := reviews[0].Spec, reviews[0].Spec.ResourceAttributes
	if spec.User != "alice" || attrs == nil || attrs.Namespace != "default" || attrs.Verb != "get" ||
		attrs.Resource != "secrets" || attrs.Group != "" || attrs.Name != "db-creds" {
		t.Errorf("review = %+v, want alice to get Secret db-creds in namespace default", spec)
	}
}

// A review that cannot be made is not a verdict: a transient 500, for every
// Secret reference as for the post-restart Job.
func TestGatewayAdmission_SecretReferenceReviewFailureIs500(t *testing.T) {
	cases := map[string]*v1alpha1.KrakenDGateway{
		"a license secretRef":         eeGatewayWithLicenseSecret("team-license"),
		"a license ExternalSecret":    eeGatewayWithLicenseExternalSecret(),
		"a Dragonfly password Secret": gatewayWithDragonfly(v1alpha1.EditionEE, "db-creds"),
	}
	for name, gw := range cases {
		t.Run(name, func(t *testing.T) {
			c := fakeClientBuilderWith(interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					return errors.New("the server could not process the review")
				},
			})

			resp := review(t, &GatewayValidator{Client: c, Checker: &scriptedChecker{}}, "alice", gw, nil)

			if resp.Allowed || resp.Result.Code != http.StatusInternalServerError {
				t.Errorf("response = %+v, want 500", resp.Result)
			}
		})
	}
}

// Both license sources are reviewed, whether or not the CRD allows naming
// both: the operator creates the ExternalSecret whenever it is enabled, so a
// requester who may read only the secretRef Secret is refused on the other.
func TestGatewayAdmission_BothLicenseSourcesAreReviewed(t *testing.T) {
	var reviews []authorizationv1.SubjectAccessReview
	onlySecret := func(a *authorizationv1.ResourceAttributes) bool { return a.Resource == "secrets" }
	v := &GatewayValidator{Client: reviewingClientBy(onlySecret, &reviews), Checker: &scriptedChecker{}}
	gw := eeGatewayWithLicenseExternalSecret()
	gw.Spec.License.SecretRef = eeGatewayWithLicenseSecret("public-license").Spec.License.SecretRef

	resp := review(t, v, "alice", gw, nil)

	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response = %+v, want a 422 denial", resp.Result)
	}
	causes := resp.Result.Details.Causes
	if len(causes) != 1 || causes[0].Field != "spec.license.externalSecret" {
		t.Errorf("causes = %+v, want one cause on spec.license.externalSecret", causes)
	}
	if len(reviews) != 2 {
		t.Errorf("%d reviews, want 2", len(reviews))
	}
}
