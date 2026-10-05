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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/fieldindex"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

// fakeClient builds a fake client with both endpoint field indexes registered.
func fakeClient(objs ...client.Object) client.Client {
	return fakeClientBuilderWith(interceptor.Funcs{}, objs...)
}

// fakeClientBuilderWith is fakeClient with interceptors.
func fakeClientBuilderWith(funcs interceptor.Funcs, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointGateway, fieldindex.EndpointGatewayKeys).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, fieldindex.EndpointPolicy, fieldindex.EndpointPolicyKeys).
		WithInterceptorFuncs(funcs).
		Build()
}

func TestGatewayValidator_ValidEE(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionEE,
			Config: v1alpha1.GatewayConfig{},
			License: &v1alpha1.LicenseConfig{
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "lic"},
					Key:                  "LICENSE",
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestGatewayValidator_OpenAPIPortValid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
			OpenAPI: &v1alpha1.OpenAPIExportSpec{Enabled: true, Port: 8090},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func gwWithProbes(liveness, readiness *corev1.Probe, sidecarImage string) *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			OpenAPI: &v1alpha1.OpenAPIExportSpec{
				Enabled: true, Port: 8090, SidecarImage: sidecarImage,
				LivenessProbe: liveness, ReadinessProbe: readiness,
			},
		},
	}
}

func tcpHandler() corev1.ProbeHandler {
	return corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8090)}}
}

// Every state here is accepted by the CRD schema and refused by the API server
// on the rendered Deployment, where the failure is silent -- or is structurally
// impossible against the default busybox sidecar.
func TestGatewayValidator_OpenAPIProbeRejected(t *testing.T) {
	tgpsZero := int64(0)
	tgpsOK := int64(30)
	cases := []struct {
		name      string
		liveness  *corev1.Probe
		readiness *corev1.Probe
		image     string
		wantField string
	}{
		{"no handler", &corev1.Probe{}, nil, "", "livenessProbe"},
		{"two handlers", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8090)},
			Exec:      &corev1.ExecAction{Command: []string{"true"}},
		}}, nil, "", "livenessProbe"},
		{"successThreshold 2", &corev1.Probe{ProbeHandler: tcpHandler(), SuccessThreshold: 2}, nil, "", "successThreshold"},
		{"negative period", &corev1.Probe{ProbeHandler: tcpHandler(), PeriodSeconds: -5}, nil, "", "periodSeconds"},
		{"tgps zero on liveness", &corev1.Probe{ProbeHandler: tcpHandler(), TerminationGracePeriodSeconds: &tgpsZero}, nil, "", "terminationGracePeriodSeconds"},
		{"tgps set on readiness", nil, &corev1.Probe{ProbeHandler: tcpHandler(), TerminationGracePeriodSeconds: &tgpsOK}, "", "terminationGracePeriodSeconds"},
		{"tcpSocket host", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8090), Host: "169.254.169.254"},
		}}, nil, "", "host"},
		{"httpGet host", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8090), Host: "example.com"},
		}}, nil, "", "host"},
		{"grpc on default image", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			GRPC: &corev1.GRPCAction{Port: 8090},
		}}, nil, "", "grpc"},
		{"https on default image", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8090), Scheme: corev1.URISchemeHTTPS},
		}}, nil, "", "scheme"},
		// The default image written out explicitly must NOT escape the gate.
		{"grpc, default image set explicitly", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			GRPC: &corev1.GRPCAction{Port: 8090},
		}}, nil, "busybox:1.37", "grpc"},
		// successThreshold on READINESS: the !=0 && !=1 branch is liveness-only,
		// so without it in the non-negative loop this was the parity hole.
		{"negative successThreshold on readiness", nil,
			&corev1.Probe{ProbeHandler: tcpHandler(), SuccessThreshold: -3}, "", "successThreshold"},
		{"negative successThreshold on liveness",
			&corev1.Probe{ProbeHandler: tcpHandler(), SuccessThreshold: -3}, nil, "", "successThreshold"},
		// Lowercase scheme is the most plausible hand-written-YAML typo.
		{"lowercase https scheme", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8090), Scheme: corev1.URIScheme("https")},
		}}, nil, "", "scheme"},
		// Port bounds. Custom image, so the grpc/HTTPS image gate is not what fires.
		{"tcpSocket port out of range", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(70000)},
		}}, nil, "ghcr.io/example/custom:1.0", "port"},
		{"httpGet port zero", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(0)},
		}}, nil, "ghcr.io/example/custom:1.0", "port"},
		{"grpc port out of range", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			GRPC: &corev1.GRPCAction{Port: 70000},
		}}, nil, "ghcr.io/example/custom:1.0", "port"},
		{"invalid string port name", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("Not_A_Port")},
		}}, nil, "ghcr.io/example/custom:1.0", "port"},
		// readinessProbe is validated on the same terms.
		{"readiness no handler", nil, &corev1.Probe{}, "", "readinessProbe"},
	}
	v := &GatewayValidator{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), gwWithProbes(tc.liveness, tc.readiness, tc.image))
			if err == nil {
				t.Fatalf("expected rejection for %q", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("expected the error to name %q, got %v", tc.wantField, err)
			}
		})
	}
}

func TestGatewayValidator_OpenAPIProbeAccepted(t *testing.T) {
	tgpsOK := int64(30)
	cases := []struct {
		name      string
		liveness  *corev1.Probe
		readiness *corev1.Probe
		image     string
	}{
		{"both unset", nil, nil, ""},
		{"successThreshold unset", &corev1.Probe{ProbeHandler: tcpHandler()}, nil, ""},
		{"successThreshold 1", &corev1.Probe{ProbeHandler: tcpHandler(), SuccessThreshold: 1}, nil, ""},
		{"readiness successThreshold 3 is legal", nil, &corev1.Probe{ProbeHandler: tcpHandler(), SuccessThreshold: 3}, ""},
		{"tgps positive on liveness", &corev1.Probe{ProbeHandler: tcpHandler(), TerminationGracePeriodSeconds: &tgpsOK}, nil, ""},
		{"httpGet without scheme or host", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/openapi.json", Port: intstr.FromInt32(8090)},
		}}, nil, ""},
		{"explicit uppercase HTTP scheme", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8090), Scheme: corev1.URISchemeHTTP},
		}}, nil, ""},
		{"valid string port name", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("openapi")},
		}}, nil, ""},
		{"boundary ports 1 and 65535", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(65535)},
		}}, nil, ""},
		// A custom image may genuinely speak gRPC -- the gate is image-scoped.
		{"grpc on a custom image", &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			GRPC: &corev1.GRPCAction{Port: 8090},
		}}, nil, "ghcr.io/example/grpc-server:1.0"},
	}
	v := &GatewayValidator{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.ValidateCreate(context.Background(), gwWithProbes(tc.liveness, tc.readiness, tc.image)); err != nil {
				t.Errorf("expected no error for %q, got %v", tc.name, err)
			}
		})
	}
}

func TestGatewayValidator_Update(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	newGW := old.DeepCopy()
	negative := resource.MustParse("-1Gi")
	newGW.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "true", TmpSizeLimit: &negative}
	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err == nil {
		t.Error("expected error on update to a negative postRestartJob.tmpSizeLimit")
	}
}

func TestGatewayValidator_Delete(t *testing.T) {
	v := &GatewayValidator{}
	_, err := v.ValidateDelete(context.Background(), &v1alpha1.KrakenDGateway{})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

// TestGatewayValidator_WorkingDirOutsideTmpWithROFSWarns covers overriding
// workingDir outside the /tmp emptyDir mount
// while readOnlyRootFilesystem is effectively true (the hardened default)
// must produce an admission warning, not silently pass — the container
// starts fine and the failure (EROFS) only surfaces when the script runs.
func TestGatewayValidator_WorkingDirOutsideTmpWithROFSWarns(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:    true,
				Script:     "echo ok",
				WorkingDir: "/work",
			},
		},
	}
	v := &GatewayValidator{}
	warnings, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no error (this is a warning, not a rejection), got: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "EROFS") {
		t.Errorf("expected warning to mention EROFS, got: %q", warnings[0])
	}
}

// TestGatewayValidator_WorkingDirOutsideTmpWithROFSDisabledNoWarning
// verifies the escape hatch: explicitly opting out of
// readOnlyRootFilesystem suppresses the warning, since the workingDir
// override is then a deliberate, working configuration.
func TestGatewayValidator_WorkingDirOutsideTmpWithROFSDisabledNoWarning(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:    true,
				Script:     "echo ok",
				WorkingDir: "/work",
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	warnings, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warning when readOnlyRootFilesystem is explicitly false, got: %v", warnings)
	}
}

// TestGatewayValidator_WorkingDirUnderTmpNoWarning verifies workingDir
// values still under the /tmp mount (e.g. a subdirectory) don't trigger the
// warning — only paths genuinely outside the writable mount do.
func TestGatewayValidator_WorkingDirUnderTmpNoWarning(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:    true,
				Script:     "echo ok",
				WorkingDir: "/tmp/subdir",
			},
		},
	}
	v := &GatewayValidator{}
	warnings, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warning for a /tmp subdirectory, got: %v", warnings)
	}
}

// TestGatewayValidator_ContainerRunAsUserZeroRejected covers the admission
// reject (a deliberate choice): a container-level
// securityContext.runAsUser: 0 with no explicit runAsNonRoot escape hatch
// (at either container or pod scope) must be rejected outright, since the
// resulting {runAsUser:0, runAsNonRoot:true} pair hangs the Job pod
// Pending until activeDeadlineSeconds expires instead of failing fast.
func TestGatewayValidator_ContainerRunAsUserZeroRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection for container runAsUser:0 with no runAsNonRoot escape hatch")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
	// field.Path assertion on a container-path rejection — kills the
	// mutant of transposed containerPath/podPath arguments at the
	// validateRunAsRootConflict call site.
	if !strings.Contains(err.Error(), "spec.postRestartJob.securityContext.runAsUser") {
		t.Errorf("expected the container-path rejection to point at "+
			"spec.postRestartJob.securityContext.runAsUser, got: %v", err)
	}
	// The scope-distinguishing clause must be present so a container-path
	// rejection doesn't misleadingly suggest a container-scope opt-out is a
	// universal acknowledgment.
	if !strings.Contains(err.Error(), "only acknowledges a container-scope runAsUser: 0") {
		t.Errorf("expected the scope-distinguishing clause, got: %v", err)
	}
}

// TestGatewayValidator_ContainerRunAsUserZeroWithPodRunAsNonRootFalseAllowed
// verifies the escape hatch: setting podSecurityContext.runAsNonRoot: false
// alongside the container's runAsUser: 0 is accepted.
func TestGatewayValidator_ContainerRunAsUserZeroWithPodRunAsNonRootFalseAllowed(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no error when podSecurityContext.runAsNonRoot:false is set, got: %v", err)
	}
}

// TestGatewayValidator_ContainerRunAsUserZeroWithContainerRunAsNonRootFalseAllowed
// verifies the other escape hatch: an explicit container-level
// runAsNonRoot: false alongside runAsUser: 0 is accepted.
func TestGatewayValidator_ContainerRunAsUserZeroWithContainerRunAsNonRootFalseAllowed(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no error when container securityContext.runAsNonRoot:false is set, got: %v", err)
	}
}

// TestGatewayValidator_PodScopeRunAsUserZeroWithExplicitRunAsNonRootTrueRejected
// covers the pod-scope hole. Unlike a fully
// unset podSecurityContext.runAsNonRoot (self-healed at build time by
// job.go's mergePodSecurityContext fixup), an EXPLICIT
// podSecurityContext.runAsNonRoot: true alongside podSecurityContext.
// runAsUser: 0 is never self-healed (the fixup only triggers when
// RunAsNonRoot is unset) and must be rejected at admission, same as the
// container-scope case.
func TestGatewayValidator_PodScopeRunAsUserZeroWithExplicitRunAsNonRootTrueRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(true),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection for pod-scope runAsUser:0 with explicit podSecurityContext.runAsNonRoot:true")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
	// field.Path assertion on a pod-path rejection — kills the mutant
	// of transposed containerPath/podPath arguments at the
	// validateRunAsRootConflict call site.
	if !strings.Contains(err.Error(), "spec.postRestartJob.podSecurityContext.runAsUser") {
		t.Errorf("expected the pod-path rejection to point at "+
			"spec.postRestartJob.podSecurityContext.runAsUser, got: %v", err)
	}
	// The scope-distinguishing clause must be present so a pod-path
	// rejection doesn't misleadingly suggest a container-scope opt-out would
	// have sufficed.
	if !strings.Contains(err.Error(), "only acknowledges a container-scope runAsUser: 0") {
		t.Errorf("expected the scope-distinguishing clause, got: %v", err)
	}
}

// TestGatewayValidator_PodScopeRunAsUserZeroUnsetRunAsNonRootAllowed verifies
// the second outcome of the pod-scope rule is preserved: pod-scope
// runAsUser:0 with runAsNonRoot left unset everywhere is NOT rejected at
// admission — job.go's mergePodSecurityContext fixup self-heals this
// combination at build time (kept as defense-in-depth for webhook-bypass
// paths).
func TestGatewayValidator_PodScopeRunAsUserZeroUnsetRunAsNonRootAllowed(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no rejection for pod-scope runAsUser:0 with runAsNonRoot unset "+
			"(self-healed by the builder fixup), got: %v", err)
	}
}

// TestGatewayValidator_PodScopeRunAsUserZeroContainerOptOutStillAllowedViaSelfHeal
// pins that the container-scope opt-out no longer unconditionally
// short-circuits validateRunAsRootConflict, for the
// postRestartJob (job) lane specifically: a pod-scope runAsUser:0 combined
// with a CONTAINER-scope runAsNonRoot:false (container runAsUser left
// unset) must remain ADMITTED — but now via the SAME pod-scope-unset
// self-heal carve-out (allowPodScopeUnsetSelfHeal: true) that
// TestGatewayValidator_PodScopeRunAsUserZeroUnsetRunAsNonRootAllowed already
// covers for a fully-unset runAsNonRoot, not via the old (and, for
// Dragonfly, unsafe) "any opt-out short-circuits" branch. The
// container-scope runAsNonRoot:false does not itself trigger
// containerAssertsTrue/podAssertsTrue (neither is true), so the self-heal
// condition still evaluates true and the outcome is unchanged from before
// the opt-out rule was tightened — see webhook.go's validateRunAsRootConflict doc for the full trace.
func TestGatewayValidator_PodScopeRunAsUserZeroContainerOptOutStillAllowedViaSelfHeal(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo ok",
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected postRestartJob pod-scope runAsUser:0 with a container-scope "+
			"runAsNonRoot:false opt-out to remain admitted via the pod-scope-unset self-heal "+
			"carve-out (the opt-out rule must not change job-lane outcomes), got: %v", err)
	}
}

// TestGatewayValidator_PodScopeRunAsUserZeroExplicitTrueContainerOptOutRejected_JobLane
// pins the 707166b behavior change for the postRestartJob (job) lane. podSecurityContext{runAsUser:0,
// runAsNonRoot:true} combined with a container-scope securityContext{
// runAsNonRoot:false} (no container-scope runAsUser) must be REJECTED at
// spec.postRestartJob.podSecurityContext.runAsUser: the pod scope's own
// EXPLICIT runAsNonRoot:true means the pod-scope-unset self-heal carve-out
// (allowPodScopeUnsetSelfHeal) does not apply, and — per the opt-out rule shared
// with Dragonfly via validateRunAsRootConflict — a container-scope opt-out
// only acknowledges a root request that itself came from the container
// scope (fromContainer), which is false here (the container never sets its
// own runAsUser). Before 707166b this combination was ADMITTED (the
// earlier `containerOptsOut || podOptsOut` logic accepted a container-scope
// opt-out unconditionally, regardless of which scope produced the root
// request).
func TestGatewayValidator_PodScopeRunAsUserZeroExplicitTrueContainerOptOutRejected_JobLane(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo ok",
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(true),
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection: a container-scope runAsNonRoot:false opt-out must not " +
			"mask the pod scope's own explicit, self-contradictory runAsNonRoot:true " +
			"(pinning the 707166b job-lane behavior change)")
	}
	if !strings.Contains(err.Error(), "spec.postRestartJob.podSecurityContext.runAsUser") {
		t.Errorf("expected the rejection to point at spec.postRestartJob.podSecurityContext.runAsUser, got: %v", err)
	}
	// This shape's rejection message is load-bearing — it is the ONLY
	// message text the user ever sees when the pod scope's own explicit
	// runAsNonRoot:true blocks a container-scope opt-out that would otherwise
	// short-circuit the check. Without the "Unless the Job container carries
	// its own runAsNonRoot: false" qualifier, the message would read as an
	// unconditional promise that a container-scope opt-out always works —
	// which the behavior pinned by this very test made false for exactly
	// this shape. Asserting the substring here means a
	// future edit that drops or waters down that qualifier fails this test,
	// not just a manual doc review.
	if !strings.Contains(err.Error(), "Unless the Job container carries its own runAsNonRoot: false") {
		t.Errorf("expected the rejection message to explain the container-scope-opt-out "+
			"qualifier (\"Unless the Job container carries its own runAsNonRoot: false\"), got: %v", err)
	}
}

// TestGatewayValidator_EffectiveRunAsRootCrossScopePrecedence covers the
// cross-scope precedence: every existing runAsUser:0 test above sets runAsUser
// at exactly one scope (container-only or pod-only), so none of them
// discriminates the cross-scope PRECEDENCE effectiveRunAsRoot implements
// (container wins over pod when container.RunAsUser is set) — the suite
// would stay green even if that fallback order were silently inverted. Each
// row below sets runAsUser at BOTH scopes to different values, so the
// admit/reject outcome flips depending on which scope effectively wins.
func TestGatewayValidator_EffectiveRunAsRootCrossScopePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		container *corev1.SecurityContext
		pod       *corev1.PodSecurityContext
		wantErr   bool
		reason    string
	}{
		{
			name: "container non-root wins over pod root, runAsNonRoot:true at pod scope",
			container: &corev1.SecurityContext{
				RunAsUser: new(int64(1000)),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser:    new(int64(0)),
				RunAsNonRoot: new(true),
			},
			wantErr: false,
			reason: "container.runAsUser:1000 must win over pod.runAsUser:0 (effective uid 1000, " +
				"non-root) — inverted precedence would use the pod's uid0 and reject",
		},
		{
			name: "container non-root wins over pod root, runAsNonRoot:true at container scope",
			container: &corev1.SecurityContext{
				RunAsUser:    new(int64(1000)),
				RunAsNonRoot: new(true),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser: new(int64(0)),
			},
			wantErr: false,
			reason: "container.runAsUser:1000 must win over pod.runAsUser:0 (effective uid 1000, " +
				"non-root) — inverted precedence would use the pod's uid0 and reject",
		},
		{
			name: "container root wins over pod non-root — rejected",
			container: &corev1.SecurityContext{
				RunAsUser: new(int64(0)),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser: new(int64(1000)),
			},
			wantErr: true,
			reason: "container.runAsUser:0 must win over pod.runAsUser:1000 (effective uid 0, no " +
				"escape hatch) — inverted precedence would use the pod's uid 1000 and allow",
		},
		{
			name: "container root wins over pod non-root, container opt-out — allowed",
			container: &corev1.SecurityContext{
				RunAsUser:    new(int64(0)),
				RunAsNonRoot: new(false),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser: new(int64(1000)),
			},
			wantErr: false,
			reason: "container.runAsUser:0 wins (effective uid 0), but the container's own " +
				"runAsNonRoot:false opts out — inverted precedence would use the pod's uid 1000 " +
				"and allow for an unrelated reason",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := &v1alpha1.KrakenDGateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: v1alpha1.KrakenDGatewaySpec{
					Version: "2.13", Edition: v1alpha1.EditionCE,
					Config: v1alpha1.GatewayConfig{},
					PostRestartJob: &v1alpha1.PostRestartJobSpec{
						Enabled:            true,
						Script:             "echo ok",
						SecurityContext:    tc.container,
						PodSecurityContext: tc.pod,
					},
				},
			}
			v := &GatewayValidator{}
			_, err := v.ValidateCreate(context.Background(), gw)
			if tc.wantErr && err == nil {
				t.Fatalf("expected rejection (%s), got no error", tc.reason)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error (%s), got: %v", tc.reason, err)
			}
		})
	}
}

// TestGatewayValidator_RunAsUserZeroRatchetUnchangedUpdateAllowed covers
// a CR already carrying container
// securityContext.runAsUser:0 with no runAsNonRoot escape hatch — as
// accepted by an OLDER operator version before this reject existed — must
// not start failing on an UNRELATED update as long as the relevant
// securityContext fields are unchanged from the stored spec.
func TestGatewayValidator_RunAsUserZeroRatchetUnchangedUpdateAllowed(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	newGW := old.DeepCopy()
	// Unrelated field changes; the securityContext runAsUser/runAsNonRoot
	// fields stay exactly as they were.
	newGW.Spec.PostRestartJob.Script = "npm install -g rdme && echo done"

	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err != nil {
		t.Fatalf("expected the pre-existing runAsUser:0 to be ratcheted (allowed) on an unrelated update, got: %v", err)
	}
}

// TestGatewayValidator_RunAsUserZeroRatchetNewlyIntroducedUpdateRejected
// covers the other half of the ratchet: it must NOT
// apply when the update is what actually INTRODUCES the offending
// combination — that must still be rejected exactly like a Create.
func TestGatewayValidator_RunAsUserZeroRatchetNewlyIntroducedUpdateRejected(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo ok",
			},
		},
	}
	newGW := old.DeepCopy()
	newGW.Spec.PostRestartJob.SecurityContext = &corev1.SecurityContext{
		RunAsUser: new(int64(0)),
	}

	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err == nil {
		t.Fatal("expected rejection: this update newly introduces runAsUser:0 with no escape hatch")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
}

// TestGatewayValidator_RunAsUserZeroRatchetDisabledThenEnabledRejected covers
// a spec stored with postRestartJob DISABLED
// (never validated by any operator version, old or new) must not grandfather
// its runAsUser:0 when the caller flips Enabled to true on the same update —
// the ratchet only applies to a previously-ENABLED spec.
func TestGatewayValidator_RunAsUserZeroRatchetDisabledThenEnabledRejected(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: false,
				Script:  "npm install -g rdme",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	newGW := old.DeepCopy()
	newGW.Spec.PostRestartJob.Enabled = true

	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err == nil {
		t.Fatal("expected rejection: enabling a previously-disabled spec must not ratchet off its stored runAsUser:0")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyContainerRunAsUserZeroRejected mirrors
// TestGatewayValidator_ContainerRunAsUserZeroRejected for the Dragonfly
// scope (spec.dragonfly.containerSecurityContext), now that
// mergeDragonflyContainerSecurityContext merges instead of replacing.
func TestGatewayValidator_DragonflyContainerRunAsUserZeroRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection for dragonfly container runAsUser:0 with no runAsNonRoot escape hatch")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
	// field.Path assertion on a container-path rejection — kills the
	// mutant of transposed containerPath/podPath arguments at the
	// validateRunAsRootConflict call site.
	if !strings.Contains(err.Error(), "spec.dragonfly.containerSecurityContext.runAsUser") {
		t.Errorf("expected the container-path rejection to point at "+
			"spec.dragonfly.containerSecurityContext.runAsUser, got: %v", err)
	}
	// The scope-distinguishing clause must be present so a container-path
	// rejection doesn't misleadingly suggest a pod-scope-only fix is required
	// when a container-scope opt-out (together with the container's own
	// runAsUser:0) would suffice.
	if !strings.Contains(err.Error(), "acknowledges only a container-scope runAsUser: 0") {
		t.Errorf("expected the scope-distinguishing clause, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyContainerRunAsUserZeroWithPodRunAsNonRootFalseAllowed
// verifies the escape hatch: setting podSecurityContext.runAsNonRoot: false
// alongside the container's runAsUser: 0 is accepted AT ADMISSION. This is
// admission-only, though: before the container-scope uid0 fixup in
// mergeDragonflyContainerSecurityContext existed, admission would allow this spec while the BUILDER still rendered the
// kubelet-rejected {runAsUser:0, runAsNonRoot:true} pair — the escape hatch
// was a lie. Paired with the build-level assertion that the escape hatch
// actually renders a startable container: see external_crd_test.go's
// TestBuildDragonfly_ContainerRootUserWithPodRunAsNonRootFalseStartable.
func TestGatewayValidator_DragonflyContainerRunAsUserZeroWithPodRunAsNonRootFalseAllowed(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no error when podSecurityContext.runAsNonRoot:false is set, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyContainerRunAsUserZeroWithContainerRunAsNonRootFalseAllowed
// verifies the other escape hatch: an explicit container-level
// runAsNonRoot: false alongside runAsUser: 0 is accepted.
func TestGatewayValidator_DragonflyContainerRunAsUserZeroWithContainerRunAsNonRootFalseAllowed(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected no error when container securityContext.runAsNonRoot:false is set, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroWithExplicitRunAsNonRootTrueRejected
// covers the EXPLICIT-runAsNonRoot:true variant of the pod-scope hole for
// Dragonfly: podSecurityContext.runAsUser:0 with an explicit
// podSecurityContext.runAsNonRoot:true must be rejected at admission, same
// as the container-scope case. Note there is no self-heal fixup to
// distinguish this from anymore (mergeDragonflyPodSecurityContext has no
// admission-time pod-scope fixup): a fully UNSET
// podSecurityContext.runAsNonRoot alongside runAsUser:0 is now ALSO
// rejected at admission — see the adjacent
// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroUnsetRunAsNonRootRejected.
// This test covers the same reject outcome for the explicit-true variant.
func TestGatewayValidator_DragonflyPodScopeRunAsUserZeroWithExplicitRunAsNonRootTrueRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(true),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection for dragonfly pod-scope runAsUser:0 with explicit podSecurityContext.runAsNonRoot:true")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroUnsetRunAsNonRootRejected
// covers that, unlike postRestartJob, the
// pod-scope-unset case for Dragonfly is now REJECTED at admission (renamed
// from ...Allowed). Dragonfly's container-scope default PINS RunAsNonRoot
// (and RunAsUser/RunAsGroup) regardless of pod scope — container-scope
// always overrides pod-scope per-field at the kubelet — so a pod-scope
// runAsUser:0 never changes the Dragonfly container's effective uid; it
// only silently roots injected sidecars with no legitimate capability
// gained. There is nothing to self-heal for admission purposes, so this
// combination is rejected outright for any NEW or CHANGED spec
// (allowPodScopeUnsetSelfHeal: false). Note: mergeDragonflyPodSecurityContext
// keeps a build-time-only, cross-scope-aware pod-scope fixup — but that
// serves GRANDFATHERED/webhook-bypassed CRs only (main-branch
// render parity), not this admission path; a brand-new spec with this exact
// shape is still rejected here regardless of what the builder would later
// do with it.
func TestGatewayValidator_DragonflyPodScopeRunAsUserZeroUnsetRunAsNonRootRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection for dragonfly pod-scope runAsUser:0 with runAsNonRoot unset " +
			"(no longer self-healed — the pod-scope fixup was removed)")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
	if !strings.Contains(err.Error(), "spec.dragonfly.podSecurityContext.runAsUser") {
		t.Errorf("expected the sanctioned error-path fix to point at "+
			"spec.dragonfly.podSecurityContext.runAsUser (the field the user actually set), got: %v", err)
	}
	// The scope-distinguishing clause must be present so a pod-path
	// rejection doesn't misleadingly suggest a container-scope opt-out alone
	// would acknowledge this pod-scope-originated request.
	if !strings.Contains(err.Error(), "acknowledges only a container-scope runAsUser: 0") {
		t.Errorf("expected the scope-distinguishing clause, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroContainerOptOutRejected
// covers a security-important rule: a CONTAINER-scope runAsNonRoot:false
// (with no container-scope runAsUser at all) must not mask a POD-scope root
// request — validateRunAsRootConflict's old
// `containerOptsOut || podOptsOut` accepted a container-scope opt-out
// unconditionally, even though the effective uid0 came from the POD scope
// (fromContainer: false) and the container never asked to run as root
// itself. An opt-out is only a valid acknowledgment from the scope that
// actually produced the root request: a bare container-scope
// runAsNonRoot:false does not (and cannot) acknowledge what OTHER
// sidecars/extra containers inheriting the pod-scope pair would still
// silently receive. This must now be REJECTED.
func TestGatewayValidator_DragonflyPodScopeRunAsUserZeroContainerOptOutRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection: a container-scope runAsNonRoot:false opt-out must not " +
			"mask a pod-scope runAsUser:0 root request")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroExplicitTrueContainerOptOutRejected
// covers the self-contradictory variant of that rule: pod scope explicitly asserts
// runAsNonRoot:true (a hard, explicit acknowledgment that root is NOT
// wanted) while root is requested via pod-scope runAsUser:0, and a
// container-scope runAsNonRoot:false tries to opt out on the pod's behalf.
// Before this fix the container-scope opt-out masked this self-contradiction
// entirely; it must now be rejected — the container's opt-out cannot
// override the pod's own explicit, contradictory assertion.
func TestGatewayValidator_DragonflyPodScopeRunAsUserZeroExplicitTrueContainerOptOutRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(true),
				},
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: new(false),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection: a container-scope runAsNonRoot:false opt-out must not " +
			"mask the pod scope's own explicit, self-contradictory runAsNonRoot:true")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyRunAsUserZeroRatchetUnchangedUpdateAllowed
// covers the Dragonfly ratchet: a CR already carrying container
// securityContext.runAsUser:0 with no runAsNonRoot escape hatch — as
// accepted before this reject existed — must not start failing on an
// UNRELATED update as long as the relevant securityContext fields are
// unchanged from the stored spec.
func TestGatewayValidator_DragonflyRunAsUserZeroRatchetUnchangedUpdateAllowed(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	newGW := old.DeepCopy()
	// Unrelated field changes; the securityContext runAsUser/runAsNonRoot
	// fields stay exactly as they were.
	newGW.Spec.Dragonfly.Image = "docker.dragonflydb.io/dragonflydb/dragonfly:v1.25.2"

	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err != nil {
		t.Fatalf("expected the pre-existing dragonfly runAsUser:0 to be ratcheted (allowed) on an unrelated update, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroRatchetUnchangedUpdateAllowed
// covers the ratchet for a grandfathered Dragonfly spec: now that pod-scope
// runAsUser:0 with runAsNonRoot unset is rejected at admission for NEW/changed specs (see
// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroUnsetRunAsNonRootRejected),
// a CR that already carries that shape — grandfathered from before the
// admission tightening — must still be ratcheted (allowed) on
// an update that leaves the relevant fields unchanged.
func TestGatewayValidator_DragonflyPodScopeRunAsUserZeroRatchetUnchangedUpdateAllowed(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	newGW := old.DeepCopy()
	// Unrelated field change; the pod-scope runAsUser/runAsNonRoot fields
	// stay exactly as they were.
	newGW.Spec.Dragonfly.Image = "docker.dragonflydb.io/dragonflydb/dragonfly:v1.25.2"

	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err != nil {
		t.Fatalf("expected the pre-existing grandfathered pod-scope dragonfly runAsUser:0 to be "+
			"ratcheted (allowed) on an unrelated update, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyRunAsUserZeroRatchetNewlyIntroducedUpdateRejected
// covers the other half of the ratchet: the update must NOT be ratcheted
// when it actually INTRODUCES the offending combination — that must still
// be rejected exactly like a Create.
func TestGatewayValidator_DragonflyRunAsUserZeroRatchetNewlyIntroducedUpdateRejected(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
			},
		},
	}
	newGW := old.DeepCopy()
	newGW.Spec.Dragonfly.ContainerSecurityContext = &corev1.SecurityContext{
		RunAsUser: new(int64(0)),
	}

	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err == nil {
		t.Fatal("expected rejection: this update newly introduces dragonfly runAsUser:0 with no escape hatch")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyRunAsUserZeroRatchetDisabledThenEnabledRejected
// covers a spec stored with dragonfly DISABLED (never validated by any
// operator version, old or new) must not grandfather its runAsUser:0 when
// the caller flips Enabled to true on the same update — the ratchet only
// applies to a previously-ENABLED spec.
func TestGatewayValidator_DragonflyRunAsUserZeroRatchetDisabledThenEnabledRejected(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: false,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	newGW := old.DeepCopy()
	newGW.Spec.Dragonfly.Enabled = true

	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err == nil {
		t.Fatal("expected rejection: enabling a previously-disabled dragonfly spec must not ratchet off its stored runAsUser:0")
	}
	if !strings.Contains(err.Error(), "runAsNonRoot") {
		t.Errorf("expected error to mention runAsNonRoot, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyEffectiveRunAsRootCrossScopePrecedence
// mirrors TestGatewayValidator_EffectiveRunAsRootCrossScopePrecedence for
// the Dragonfly scope: every runAsUser:0 test above sets runAsUser at
// exactly one scope (container-only or pod-only), so none of them
// discriminates the cross-scope PRECEDENCE effectiveRunAsRoot implements
// (container wins over pod when container.RunAsUser is set) — the suite
// would stay green even if that fallback order were silently inverted. Each
// row below sets runAsUser at BOTH scopes to different values, so the
// admit/reject outcome flips depending on which scope effectively wins.
//
// The first two rows below are rejection tests: "container.runAsUser:999 must
// win over pod.runAsUser:0" holds for effectiveRunAsRoot's OWN precedence (the
// primary container's effective uid really is 999, non-root), but that is not
// the whole admission picture: the independent pod-scope acknowledgment gate
// in validateRunAsRootConflict separately rejects the unacknowledged
// pod-scope runAsUser:0, because that value is still rendered on the shared
// pod-level securityContext every OTHER container/sidecar in the pod
// inherits when it sets nothing of its own — a hole the primary container's
// own non-root effective uid does nothing to close. Cross-scope-precedence
// coverage (the CONTAINER side of the precedence — a container-scope root
// request winning over a SAFE pod-scope value) is exercised by the two
// container-root rows further down (see
// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroWithNonZeroContainerRejected
// and TestGatewayValidator_DragonflyContainerRootRecipeWithPodScopeRootStillAdmitted
// below for the two pod-scope gate tests).
func TestGatewayValidator_DragonflyEffectiveRunAsRootCrossScopePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		container *corev1.SecurityContext
		pod       *corev1.PodSecurityContext
		wantErr   bool
		reason    string
		// wantDetail, when non-empty, asserts the
		// rejection message contains this substring — an empty wantDetail
		// skips the check for rows where the message's exact conditional
		// wording isn't load-bearing to this table's purpose.
		wantDetail string
	}{
		{
			name: "pod-scope gate rejects unacknowledged pod root even though container's own effective uid is non-root, runAsNonRoot:true at pod scope",
			container: &corev1.SecurityContext{
				RunAsUser: new(int64(999)),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser:    new(int64(0)),
				RunAsNonRoot: new(true),
			},
			wantErr: true,
			reason: "the independent pod-scope acknowledgment gate rejects the unacknowledged " +
				"pod.runAsUser:0 regardless of container.runAsUser:999's own non-root effective uid — " +
				"other sidecars in the pod would still silently inherit the pod-scope root request",
		},
		{
			name: "pod-scope gate rejects unacknowledged pod root even though container's own effective uid is non-root, runAsNonRoot:true at container scope",
			container: &corev1.SecurityContext{
				RunAsUser:    new(int64(999)),
				RunAsNonRoot: new(true),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser: new(int64(0)),
			},
			wantErr: true,
			reason: "the independent pod-scope acknowledgment gate rejects the unacknowledged " +
				"pod.runAsUser:0 regardless of container.runAsUser:999's own non-root effective uid — " +
				"a container-scope runAsNonRoot:true is not a pod-scope acknowledgment",
		},
		{
			name: "container root wins over pod non-root — rejected",
			container: &corev1.SecurityContext{
				RunAsUser: new(int64(0)),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser: new(int64(999)),
			},
			wantErr: true,
			reason: "container.runAsUser:0 must win over pod.runAsUser:999 (effective uid 0, no " +
				"escape hatch) — inverted precedence would use the pod's uid 999 and allow",
			// This shape (container runAsUser:0, runAsNonRoot left unset, no
			// pod-scope acknowledgment) renders either the kubelet-invalid
			// {0, true} pair OR a container silently running as root,
			// depending on the other securityContext fields — the
			// conditional wording in validateDragonflyRunAsRoot's rejection
			// message is load-bearing here specifically because this row's
			// shape is the "silently running as root" branch (the merge
			// fixup drops the inherited runAsNonRoot default since the
			// user's own container.RunAsNonRoot is nil), not the
			// kubelet-invalid-pair branch.
			wantDetail: "or a container silently running as root",
		},
		{
			name: "container root wins over pod non-root, container opt-out — allowed",
			container: &corev1.SecurityContext{
				RunAsUser:    new(int64(0)),
				RunAsNonRoot: new(false),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser: new(int64(999)),
			},
			wantErr: false,
			reason: "container.runAsUser:0 wins (effective uid 0), but the container's own " +
				"runAsNonRoot:false opts out — inverted precedence would use the pod's uid 999 " +
				"and allow for an unrelated reason",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := &v1alpha1.KrakenDGateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: v1alpha1.KrakenDGatewaySpec{
					Version: "2.13", Edition: v1alpha1.EditionCE,
					Config: v1alpha1.GatewayConfig{},
					Dragonfly: &v1alpha1.DragonflySpec{
						Enabled:                  true,
						ContainerSecurityContext: tc.container,
						PodSecurityContext:       tc.pod,
					},
				},
			}
			v := &GatewayValidator{}
			_, err := v.ValidateCreate(context.Background(), gw)
			if tc.wantErr && err == nil {
				t.Fatalf("expected rejection (%s), got no error", tc.reason)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error (%s), got: %v", tc.reason, err)
			}
			if tc.wantDetail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantDetail) {
					t.Errorf("expected rejection message to contain %q (%s), got: %v", tc.wantDetail, tc.reason, err)
				}
			}
		})
	}
}

// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroWithNonZeroContainerRejected
// covers the pod-scope gate's headline case directly (c{999}+p{0} unset): a
// pod-scope runAsUser:0 with runAsNonRoot left unset must be REJECTED even
// when the container
// scope sets its own non-zero, non-root runAsUser (999) — the container's
// own safety does nothing to acknowledge the pod-scope root request that is
// still rendered on the shared pod-level securityContext for any OTHER
// container/sidecar in the pod.
func TestGatewayValidator_DragonflyPodScopeRunAsUserZeroWithNonZeroContainerRejected(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(999)),
				},
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection: pod-scope runAsUser:0 with runAsNonRoot unset must be " +
			"rejected even when the container scope's own runAsUser is a non-zero, non-root value")
	}
	if !strings.Contains(err.Error(), "spec.dragonfly.podSecurityContext.runAsUser") {
		t.Errorf("expected the rejection to point at spec.dragonfly.podSecurityContext.runAsUser, got: %v", err)
	}
}

// TestGatewayValidator_DragonflyContainerRootRecipeWithPodScopeRootStillAdmitted
// covers the pod-scope gate's documented carve-out: the container-root
// recipe c{runAsUser:0,runAsNonRoot:false}+p{runAsUser:0} must remain
// ADMITTED — the pod-scope gate's "unless the container scope carries its
// own runAsUser: 0" clause routes this shape through the existing
// container-path rules instead, which the container's own runAsNonRoot:
// false already satisfies. There is no security delta vs.
// p{runAsUser:0,runAsNonRoot:false} alone (both scopes end up root either
// way; the container's own opt-out covers it either way).
func TestGatewayValidator_DragonflyContainerRootRecipeWithPodScopeRootStillAdmitted(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled: true,
				ContainerSecurityContext: &corev1.SecurityContext{
					RunAsUser:    new(int64(0)),
					RunAsNonRoot: new(false),
				},
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsUser: new(int64(0)),
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected the documented container-root recipe "+
			"(container{runAsUser:0,runAsNonRoot:false}+pod{runAsUser:0}) to remain admitted "+
			"(no security delta vs. pod{runAsUser:0,runAsNonRoot:false} alone), got: %v", err)
	}
}

// TestGatewayValidator_NegativeTmpSizeLimitRejected covers that a negative
// tmpSizeLimit must be rejected.
func TestGatewayValidator_NegativeTmpSizeLimitRejected(t *testing.T) {
	qty := resource.MustParse("-1Gi")
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:      true,
				Script:       "echo ok",
				TmpSizeLimit: &qty,
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected rejection for negative tmpSizeLimit")
	}
	if !strings.Contains(err.Error(), "tmpSizeLimit") {
		t.Errorf("expected error to mention tmpSizeLimit, got: %v", err)
	}
}

func TestEndpointValidator_Valid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestEndpointValidator_GatewayNotFound(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "nonexistent"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	v := &EndpointValidator{Client: fakeClient()}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err == nil {
		t.Error("expected error for missing gateway")
	}
}

func TestEndpointValidator_PolicyNotFound(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{
						Host: []string{"http://svc"}, URLPattern: "/",
						PolicyRef: &v1alpha1.PolicyRef{Name: "missing"},
					}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err == nil {
		t.Error("expected error for missing policy")
	}
}

func TestEndpointValidator_Update(t *testing.T) {
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "nonexistent"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	old := ep.DeepCopy()
	old.Spec.GatewayRef.Name = "previous"
	v := &EndpointValidator{Client: fakeClient()}
	_, err := v.ValidateUpdate(context.Background(), old, ep)
	if err == nil {
		t.Error("expected error on update to a missing gateway")
	}
}

func TestEndpointValidator_Delete(t *testing.T) {
	v := &EndpointValidator{}
	_, err := v.ValidateDelete(context.Background(), &v1alpha1.KrakenDEndpoint{})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestPolicyValidator_Valid(t *testing.T) {
	p := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{MaxErrors: 5, Interval: 60, Timeout: 30},
			RateLimit:      &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	v := &PolicyValidator{}
	_, err := v.ValidateCreate(context.Background(), p)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestPolicyValidator_DeleteBlocked(t *testing.T) {
	p := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{
						Host: []string{"http://svc"}, URLPattern: "/",
						PolicyRef: &v1alpha1.PolicyRef{Name: "my-policy"},
					}}},
			},
		},
	}
	v := &PolicyValidator{Client: fakeClient(p, ep)}
	_, err := v.ValidateDelete(context.Background(), p)
	if err == nil {
		t.Error("expected error: policy referenced")
	}
	if !strings.Contains(err.Error(), "ep1") {
		t.Errorf("expected ep1 in error, got: %v", err)
	}
}

func TestPolicyValidator_DeleteAllowed(t *testing.T) {
	p := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
	}
	v := &PolicyValidator{Client: fakeClient(p)}
	_, err := v.ValidateDelete(context.Background(), p)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestAutoConfigValidator_Valid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			OpenAPI:    v1alpha1.OpenAPISource{URL: "https://example.com/api"},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestAutoConfigValidator_GatewayNotFound(t *testing.T) {
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "missing"},
			OpenAPI:    v1alpha1.OpenAPISource{URL: "https://example.com/api"},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient()}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Error("expected error for missing gateway")
	}
}

func TestAutoConfigValidator_Update(t *testing.T) {
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "missing"},
			OpenAPI:    v1alpha1.OpenAPISource{URL: "https://example.com/api"},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	}
	old := ac.DeepCopy()
	old.Spec.GatewayRef.Name = "previous"
	v := &AutoConfigValidator{Client: fakeClient()}
	_, err := v.ValidateUpdate(context.Background(), old, ac)
	if err == nil {
		t.Error("expected error on update to a missing gateway")
	}
}

func TestAutoConfigValidator_Delete(t *testing.T) {
	v := &AutoConfigValidator{}
	_, err := v.ValidateDelete(context.Background(), &v1alpha1.KrakenDAutoConfig{})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestEndpointValidator_DeleteNoOp(t *testing.T) {
	v := &EndpointValidator{}
	_, err := v.ValidateDelete(context.Background(), &v1alpha1.KrakenDEndpoint{})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

// --- Cross-namespace tests ---

func TestEndpointValidator_CrossNamespaceGatewayValid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "infra"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "app"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw", Namespace: "infra"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err != nil {
		t.Errorf("expected no error for cross-ns gateway, got %v", err)
	}
}

func TestEndpointValidator_CrossNamespaceGatewayNotFound(t *testing.T) {
	// Gateway in "infra", endpoint references "other" namespace → not found.
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "infra"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "app"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw", Namespace: "other"},
			Endpoints:  []v1alpha1.EndpointEntry{},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err == nil {
		t.Error("expected error for gateway in wrong namespace")
	}
}

func TestEndpointValidator_CrossNamespacePolicyValid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	pol := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-policy", Namespace: "policies"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 100},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{
						Host: []string{"http://svc"}, URLPattern: "/",
						PolicyRef: &v1alpha1.PolicyRef{Name: "shared-policy", Namespace: "policies"},
					}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw, pol)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err != nil {
		t.Errorf("expected no error for cross-ns policy, got %v", err)
	}
}

func TestEndpointValidator_CrossNamespacePolicyNotFound(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{
						Host: []string{"http://svc"}, URLPattern: "/",
						PolicyRef: &v1alpha1.PolicyRef{Name: "missing", Namespace: "other-ns"},
					}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err == nil {
		t.Error("expected error for cross-ns policy not found")
	}
}

func TestPolicyValidator_DeleteBlockedCrossNamespace(t *testing.T) {
	p := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-policy", Namespace: "policies"},
	}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "app"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{
						Host: []string{"http://svc"}, URLPattern: "/",
						PolicyRef: &v1alpha1.PolicyRef{Name: "shared-policy", Namespace: "policies"},
					}}},
			},
		},
	}
	v := &PolicyValidator{Client: fakeClient(p, ep)}
	_, err := v.ValidateDelete(context.Background(), p)
	if err == nil {
		t.Error("expected error: cross-ns policy still referenced")
	}
	if !strings.Contains(err.Error(), "ep1") {
		t.Errorf("expected ep1 in error, got: %v", err)
	}
}

func TestAutoConfigValidator_CrossNamespaceGatewayValid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "infra"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "app"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw", Namespace: "infra"},
			OpenAPI:    v1alpha1.OpenAPISource{URL: "https://example.com/api"},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err != nil {
		t.Errorf("expected no error for cross-ns autoconfig gateway, got %v", err)
	}
}

func TestAutoConfigValidator_CrossNamespaceGatewayNotFound(t *testing.T) {
	ac := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "app"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw", Namespace: "infra"},
			OpenAPI:    v1alpha1.OpenAPISource{URL: "https://example.com/api"},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient()}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Error("expected error for cross-ns gateway not found")
	}
}

func newAutoConfigForAdditional(eps []v1alpha1.AdditionalEndpoint) *v1alpha1.KrakenDAutoConfig {
	return &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef:          v1alpha1.GatewayRef{Name: "test-gw"},
			OpenAPI:             v1alpha1.OpenAPISource{URL: "http://svc/openapi.json"},
			Trigger:             v1alpha1.TriggerOnChange,
			AdditionalEndpoints: eps,
		},
	}
}

func TestAutoConfigValidator_AdditionalEndpointValid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{
		{Endpoint: "/liveness", Encoding: "no-op"},
		{Endpoint: "/audit", Method: "POST", Backends: []v1alpha1.BackendSpec{
			{Host: []string{"http://audit"}, URLPattern: "/v2/audit"}}},
	})

	if _, err := v.ValidateCreate(context.Background(), ac); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAutoConfigValidator_BasePathValid(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}})
	ac.Spec.AdditionalEndpointsBasePath = "/api/v1/quote"

	if _, err := v.ValidateCreate(context.Background(), ac); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAutoConfigValidator_OverrideAudienceMustBeList(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional(nil)
	ac.Spec.Overrides = []v1alpha1.OperationOverride{{
		OperationID: "listUsers",
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"documentation/openapi":{"audience":{"internal":null}}}`),
		},
	}}

	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Fatal("expected error for non-list override audience")
	}
	wantPath := `spec.overrides[0].extraConfig["documentation/openapi"].audience`
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("expected error path %q, got %q", wantPath, err.Error())
	}
}

func TestAutoConfigValidator_DefaultsEndpointAudienceMustBeList(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional(nil)
	ac.Spec.Defaults = &v1alpha1.Defaults{
		Endpoint: &v1alpha1.EndpointDefaults{
			ExtraConfig: &runtime.RawExtension{
				Raw: []byte(`{"documentation/openapi":{"audience":"internal"}}`),
			},
		},
	}

	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Fatal("expected error for non-list defaults.endpoint audience")
	}
	wantPath := `spec.defaults.endpoint.extraConfig["documentation/openapi"].audience`
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("expected error path %q, got %q", wantPath, err.Error())
	}
}

func TestAutoConfigValidator_AdditionalEndpointAudienceMustBeList(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{{
		Endpoint: "/liveness",
		ExtraConfig: &runtime.RawExtension{
			Raw: []byte(`{"documentation/openapi":{"audience":[1]}}`),
		},
	}})

	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Fatal("expected error for non-list additionalEndpoints audience")
	}
	wantPath := `spec.additionalEndpoints[0].extraConfig["documentation/openapi"].audience`
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("expected error path %q, got %q", wantPath, err.Error())
	}
}

func TestEndpointValidator_EndpointAudienceMustBeList(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"}}
	ep := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep1", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{
					Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}},
					ExtraConfig: &runtime.RawExtension{
						Raw: []byte(`{"documentation/openapi":{"audience":"internal"}}`),
					},
				},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err == nil {
		t.Fatal("expected error for non-list endpoint audience")
	}
	wantPath := `spec.endpoints[0].extraConfig["documentation/openapi"].audience`
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("expected error path %q, got %q", wantPath, err.Error())
	}
}

func TestValidateExtraConfigAudience(t *testing.T) {
	p := field.NewPath("spec", "overrides").Index(0).Child("extraConfig")

	tests := map[string]struct {
		raw       string
		wantError bool
	}{
		"list of strings": {
			raw: `{"documentation/openapi":{"audience":["internal"]}}`,
		},
		"map instead of list": {
			raw:       `{"documentation/openapi":{"audience":{"internal":null}}}`,
			wantError: true,
		},
		"string instead of list": {
			raw:       `{"documentation/openapi":{"audience":"internal"}}`,
			wantError: true,
		},
		"list of non-strings": {
			raw:       `{"documentation/openapi":{"audience":[1]}}`,
			wantError: true,
		},
		"empty list": {
			raw: `{"documentation/openapi":{"audience":[]}}`,
		},
		"null instead of list": {
			raw:       `{"documentation/openapi":{"audience":null}}`,
			wantError: true,
		},
		"list of null": {
			raw:       `{"documentation/openapi":{"audience":[null]}}`,
			wantError: true,
		},
		"list with a null item": {
			raw:       `{"documentation/openapi":{"audience":["internal",null]}}`,
			wantError: true,
		},
		"documentation/openapi block absent": {
			raw: `{"qos/ratelimit/router":{"every":"2s"}}`,
		},
		"audience key absent": {
			raw: `{"documentation/openapi":{"operation_id":"foo"}}`,
		},
		"empty object": {
			raw: `{}`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ec := &runtime.RawExtension{Raw: []byte(tc.raw)}
			errs := validateExtraConfigAudience(p, ec)
			if tc.wantError && len(errs) == 0 {
				t.Fatalf("expected an error, got none")
			}
			if !tc.wantError && len(errs) != 0 {
				t.Fatalf("expected no error, got %v", errs)
			}
			if tc.wantError {
				wantPath := `spec.overrides[0].extraConfig["documentation/openapi"].audience`
				if !strings.Contains(errs[0].Error(), wantPath) {
					t.Errorf("expected error path %q, got %q", wantPath, errs[0].Error())
				}
				if !strings.Contains(errs[0].Error(), `must be a list of strings, e.g. ["internal"]`) {
					t.Errorf("expected message about list of strings, got %q", errs[0].Error())
				}
			}
		})
	}

	t.Run("nil RawExtension", func(t *testing.T) {
		if errs := validateExtraConfigAudience(p, nil); len(errs) != 0 {
			t.Fatalf("expected no error for nil RawExtension, got %v", errs)
		}
	})

	t.Run("empty raw", func(t *testing.T) {
		if errs := validateExtraConfigAudience(p, &runtime.RawExtension{}); len(errs) != 0 {
			t.Fatalf("expected no error for empty Raw, got %v", errs)
		}
	})
}

func TestGatewayValidator_WarnsWhenReplicasSetWithAutoscaling(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE, Config: v1alpha1.GatewayConfig{},
			Replicas:    ptr.To(int32(3)),
			Autoscaling: &v1alpha1.AutoscalingSpec{MaxReplicas: 5},
		},
	}
	warnings, err := (&GatewayValidator{}).ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatalf("expected the combination to be admitted, got %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "spec.replicas is ignored") {
		t.Errorf("warnings = %q, want one saying spec.replicas is ignored", warnings)
	}
}

func TestGatewayValidator_NoReplicasWarningWithoutAutoscaling(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE, Config: v1alpha1.GatewayConfig{},
			Replicas: ptr.To(int32(3)),
		},
	}
	warnings, err := (&GatewayValidator{}).ValidateCreate(context.Background(), gw)
	if err != nil || len(warnings) != 0 {
		t.Errorf("warnings = %q, err = %v; want neither", warnings, err)
	}
}

func TestGatewayValidator_WarnsWhenOpenAPIIsSetOnACEGateway(t *testing.T) {
	for _, tc := range []struct {
		name    string
		openapi *v1alpha1.OpenAPIExportSpec
		want    int
	}{
		{"export enabled", &v1alpha1.OpenAPIExportSpec{Enabled: true}, 1},
		{"export disabled", &v1alpha1.OpenAPIExportSpec{Enabled: false}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &v1alpha1.KrakenDGateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: v1alpha1.KrakenDGatewaySpec{
					Version: "2.13", Edition: v1alpha1.EditionCE, Config: v1alpha1.GatewayConfig{}, OpenAPI: tc.openapi,
				},
			}
			warnings, err := (&GatewayValidator{}).ValidateCreate(context.Background(), gw)
			if err != nil {
				t.Fatalf("expected the gateway to be admitted, got %v", err)
			}
			if len(warnings) != tc.want ||
				(tc.want == 1 && !strings.Contains(warnings[0], "spec.openapi is ignored on CE gateways")) {
				t.Errorf("warnings = %q, want %d saying spec.openapi is ignored on CE gateways", warnings, tc.want)
			}
		})
	}
}

// terminating marks obj as being deleted, the state in which the API server
// sends finalizer-removal UPDATEs.
func terminating[T metav1.Object](obj T) T {
	obj.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	obj.SetFinalizers([]string{"foregroundDeletion"})
	return obj
}

// unfinalized returns a copy of obj with its finalizers removed, the new
// object of a finalizer-removal UPDATE.
func unfinalized[T client.Object](obj T) T {
	c := obj.DeepCopyObject().(T)
	c.SetFinalizers(nil)
	return c
}

func TestValidators_AdmitUpdatesToTerminatingObjects(t *testing.T) {
	ctx := context.Background()
	gw := terminating(&v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionEE, Config: v1alpha1.GatewayConfig{}, // EE without a license
		},
	})
	ep := terminating(&v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "default"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "deleted-gw"}},
	})
	policy := terminating(&v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{RateLimit: &v1alpha1.RateLimitSpec{MaxRate: -1}},
	})
	ac := terminating(&v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "deleted-gw"},
			OpenAPI:    v1alpha1.OpenAPISource{URL: "https://example.com/api"},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	})
	cases := []struct {
		name     string
		validate func() (admission.Warnings, error)
	}{
		{"gateway", func() (admission.Warnings, error) {
			return (&GatewayValidator{}).ValidateUpdate(ctx, gw, unfinalized(gw))
		}},
		{"endpoint", func() (admission.Warnings, error) {
			return (&EndpointValidator{Client: fakeClient()}).ValidateUpdate(ctx, ep, unfinalized(ep))
		}},
		{"policy", func() (admission.Warnings, error) {
			return (&PolicyValidator{}).ValidateUpdate(ctx, policy, unfinalized(policy))
		}},
		{"autoconfig", func() (admission.Warnings, error) {
			return (&AutoConfigValidator{Client: fakeClient()}).ValidateUpdate(ctx, ac, unfinalized(ac))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := tc.validate()
			if err != nil || len(warnings) != 0 {
				t.Errorf("update of a terminating %s: warnings = %q, err = %v; want it admitted silently",
					tc.name, warnings, err)
			}
		})
	}
}

func TestGatewayValidator_RejectsSpecChangeOnTerminatingObject(t *testing.T) {
	oldGW := terminating(&v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       v1alpha1.KrakenDGatewaySpec{Version: "2.13", Edition: v1alpha1.EditionCE},
	})
	newGW := oldGW.DeepCopy()
	negative := resource.MustParse("-1Gi")
	newGW.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "true", TmpSizeLimit: &negative}

	_, err := (&GatewayValidator{}).ValidateUpdate(context.Background(), oldGW, newGW)
	if err == nil {
		t.Fatal("a spec change to an invalid value on a terminating gateway was admitted; want it rejected")
	}
}

func TestEndpointValidator_RejectsSpecChangeOnTerminatingObject(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	oldEP := terminating(&v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep", Namespace: "default"},
		Spec:       v1alpha1.KrakenDEndpointSpec{GatewayRef: v1alpha1.GatewayRef{Name: "gw"}},
	})
	newEP := oldEP.DeepCopy()
	newEP.Spec.GatewayRef.Name = "missing-gw"

	_, err := (&EndpointValidator{Client: fakeClient(gw)}).ValidateUpdate(context.Background(), oldEP, newEP)
	if err == nil {
		t.Fatal("a spec change to an invalid value on a terminating endpoint was admitted; want it rejected")
	}
}

func TestAutoConfigValidator_RejectsSpecChangeOnTerminatingObject(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	oldAC := terminating(&v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gw"},
			OpenAPI:    v1alpha1.OpenAPISource{URL: "https://example.com/api"},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	})
	newAC := oldAC.DeepCopy()
	newAC.Spec.GatewayRef.Name = "missing-gw"

	_, err := (&AutoConfigValidator{Client: fakeClient(gw)}).ValidateUpdate(context.Background(), oldAC, newAC)
	if err == nil {
		t.Fatal("a spec change to an invalid value on a terminating autoconfig was admitted; want it rejected")
	}
}

func TestTerminatingWithUnchangedSpec(t *testing.T) {
	base := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       v1alpha1.KrakenDBackendPolicySpec{RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 10}},
	}
	changed := base.DeepCopy()
	changed.Spec.RateLimit.MaxRate = 20
	unfinalizedTerminating := func(o *v1alpha1.KrakenDBackendPolicy) *v1alpha1.KrakenDBackendPolicy {
		return unfinalized(terminating(o.DeepCopy()))
	}

	cases := []struct {
		name           string
		oldObj, newObj runtime.Object
		want           bool
	}{
		{"terminating with the same spec", terminating(base.DeepCopy()), terminating(base.DeepCopy()), true},
		{"terminating with a finalizer removed", terminating(base.DeepCopy()), unfinalizedTerminating(base), true},
		{"not terminating", base.DeepCopy(), base.DeepCopy(), false},
		{"terminating with a changed spec", terminating(base.DeepCopy()), terminating(changed), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminatingWithUnchangedSpec(tc.oldObj, tc.newObj); got != tc.want {
				t.Errorf("terminatingWithUnchangedSpec = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGatewayValidator_WarnsAboutRedisSettingsWithNoEffect(t *testing.T) {
	// KrakenD CE never uses the Redis pool, so the gateway is an EE one.
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionEE,
			License: &v1alpha1.LicenseConfig{SecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "lic"}, Key: "LICENSE",
			}},
			Redis: &v1alpha1.RedisSpec{ConnectionPool: v1alpha1.RedisConnectionPool{
				Addresses: []string{"redis:6379"}, ReadTimeout: "3s", WriteTimeout: "3s",
				Password: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "p",
				},
				TLS: &v1alpha1.RedisTLSConfig{Enabled: true},
			}},
		},
	}
	warnings, err := (&GatewayValidator{}).ValidateCreate(context.Background(), gw)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"spec.redis.connectionPool.readTimeout", "spec.redis.connectionPool.writeTimeout",
		"spec.redis.connectionPool.password", "spec.redis.connectionPool.tls",
	} {
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, field) }) {
			t.Errorf("warnings %q do not mention %s", warnings, field)
		}
	}

	// Dragonfly requires its password, which is not rendered into KrakenD's
	// pool on EE; KrakenD CE never uses the pool.
	ee := &v1alpha1.KrakenDGateway{Spec: v1alpha1.KrakenDGatewaySpec{Edition: v1alpha1.EditionEE,
		Dragonfly: &v1alpha1.DragonflySpec{Enabled: true, Authentication: &v1alpha1.DragonflyAuthSpec{
			PasswordFromSecret: &corev1.SecretKeySelector{Key: "p"},
		}},
	}}
	const dfPassword = "spec.dragonfly.authentication.passwordFromSecret"
	if w := redisPoolWarnings(ee); len(w) != 1 || !strings.Contains(w[0], dfPassword) ||
		!strings.Contains(w[0], "Dragonfly requires this password") || !strings.Contains(w[0], "refused") {
		t.Errorf("EE Dragonfly password warnings = %q, want one naming %s and the refused connections", w, dfPassword)
	}
	ce := ee.DeepCopy()
	ce.Spec.Edition = v1alpha1.EditionCE
	if w := redisPoolWarnings(ce); len(w) != 0 {
		t.Errorf("CE Dragonfly password warnings = %q, want none", w)
	}
}

// A probe stored before the probe rules existed must not block other edits.
func TestGatewayAdmission_RatchetsStoredFieldErrors(t *testing.T) {
	old := gwWithProbes(nil, &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Host: "10.0.0.1", Path: "/", Port: intstr.FromInt32(8090)}}}, "")
	edited := old.DeepCopy()
	edited.Spec.Replicas = ptr.To[int32](3)
	v := &GatewayValidator{}

	if resp := review(t, v, "alice", edited, old); !resp.Allowed {
		t.Fatalf("unrelated edit denied: %+v", resp.Result)
	}
	worse := edited.DeepCopy()
	worse.Spec.PostRestartJob = &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "true",
		TmpSizeLimit: ptr.To(resource.MustParse("-1"))}
	if resp := review(t, v, "alice", worse, old); resp.Allowed {
		t.Error("newly introduced negative tmpSizeLimit admitted")
	}
}

// A metadata-only update is not validated at all: no stored error, and no
// warning, is reported again.
func TestGatewayAdmission_UnchangedSpecIsNotValidated(t *testing.T) {
	old := gwWithProbes(nil, &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Host: "10.0.0.1", Path: "/", Port: intstr.FromInt32(8090)}}}, "")
	old.Spec.Replicas = ptr.To[int32](2)
	old.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MaxReplicas: 5}
	labeled := old.DeepCopy()
	labeled.Labels = map[string]string{"team": "edge"}

	resp := review(t, &GatewayValidator{}, "alice", labeled, old)
	if !resp.Allowed {
		t.Errorf("label-only update denied: %+v", resp.Result)
	}
	if len(resp.Warnings) != 0 {
		t.Errorf("label-only update warned: %q", resp.Warnings)
	}
}

// An error without a value (Forbidden) reads the same before and after a
// change, so a probe that changed is judged again whatever the stored one said.
func TestGatewayAdmission_ChangedProbeIsRechecked(t *testing.T) {
	probeAt := func(host string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Host: host, Path: "/", Port: intstr.FromInt32(8090)}}}
	}
	old := gwWithProbes(nil, probeAt("10.0.0.1"), "")
	swapped := gwWithProbes(nil, probeAt("10.0.0.2"), "")

	resp := review(t, &GatewayValidator{}, "alice", swapped, old)
	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		resp.Result.Details.Causes[0].Field != "spec.openapi.readinessProbe.httpGet.host" {
		t.Errorf("probe host 10.0.0.1 -> 10.0.0.2: %+v, want 422 on spec.openapi.readinessProbe.httpGet.host", resp.Result)
	}
}

func TestGatewayAdmission_ChangedTerminationGracePeriodIsRechecked(t *testing.T) {
	readinessWith := func(grace int64) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: tcpHandler(), TerminationGracePeriodSeconds: ptr.To(grace)}
	}
	old := gwWithProbes(nil, readinessWith(30), "")
	changed := gwWithProbes(nil, readinessWith(60), "")

	resp := review(t, &GatewayValidator{}, "alice", changed, old)
	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity ||
		resp.Result.Details.Causes[0].Field != "spec.openapi.readinessProbe.terminationGracePeriodSeconds" {
		t.Errorf("readiness grace 30 -> 60: %+v, want 422 on its terminationGracePeriodSeconds", resp.Result)
	}
	scaled := old.DeepCopy()
	scaled.Spec.Replicas = ptr.To[int32](3)
	if resp := review(t, &GatewayValidator{}, "alice", scaled, old); !resp.Allowed {
		t.Errorf("unrelated edit with an unchanged probe denied: %+v", resp.Result)
	}
}

func TestAutoConfigAdmission_RatchetsGatewayRefAndFields(t *testing.T) {
	old := &v1alpha1.KrakenDAutoConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "ac", Namespace: "default"},
		Spec: v1alpha1.KrakenDAutoConfigSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "gone"}, Trigger: v1alpha1.TriggerOnChange,
			OpenAPI: v1alpha1.OpenAPISource{URL: "http://svc/openapi.json"},
			Overrides: []v1alpha1.OperationOverride{{OperationID: "getA",
				ExtraConfig: &runtime.RawExtension{Raw: []byte(`{"documentation/openapi":{"audience":"x"}}`)}}},
		},
	}
	v := &AutoConfigValidator{Client: fakeClient()}

	edited := old.DeepCopy()
	edited.Spec.Filter = &v1alpha1.FilterSpec{IncludeTags: []string{"public"}}
	if resp := review(t, v, "alice", edited, old); !resp.Allowed {
		t.Errorf("unrelated edit denied: %+v", resp.Result)
	}

	moved := edited.DeepCopy()
	moved.Spec.GatewayRef.Name = "other"
	if resp := review(t, v, "alice", moved, old); resp.Allowed {
		t.Error("gatewayRef changed to a missing gateway admitted")
	}

	worse := edited.DeepCopy()
	worse.Spec.Overrides[0].ExtraConfig = &runtime.RawExtension{
		Raw: []byte(`{"documentation/openapi":{"audience":"y"}}`)}
	if resp := review(t, v, "alice", worse, old); resp.Allowed {
		t.Error("a different malformed audience admitted")
	}
}

// The runAs errors carry the same text for every violating shape, so a stored
// one must not hide a changed securityContext that still violates the rule.
func TestGatewayAdmission_ChangedDragonflyRunAsStillUnacknowledgedIsRejected(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Dragonfly: &v1alpha1.DragonflySpec{
				Enabled:            true,
				PodSecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))},
			},
		},
	}
	changed := old.DeepCopy()
	changed.Spec.Dragonfly.PodSecurityContext.RunAsNonRoot = ptr.To(true)

	resp := review(t, &GatewayValidator{}, "alice", changed, old)
	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Errorf("changed, still unacknowledged dragonfly runAsUser 0: %+v, want 422", resp.Result)
	}
}

func TestGatewayAdmission_ChangedPostRestartRunAsStillUnacknowledgedIsRejected(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true, Script: "true",
				SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(0))},
			},
		},
	}
	changed := old.DeepCopy()
	changed.Spec.PostRestartJob.SecurityContext.RunAsNonRoot = ptr.To(true)

	resp := review(t, &GatewayValidator{}, "alice", changed, old)
	if resp.Allowed || resp.Result.Code != http.StatusUnprocessableEntity {
		t.Errorf("changed, still unacknowledged postRestartJob runAsUser 0: %+v, want 422", resp.Result)
	}
}
