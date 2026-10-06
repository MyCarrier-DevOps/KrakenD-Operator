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
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
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
