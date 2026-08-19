package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func fakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, controller.EndpointGatewayIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				ns := ep.Spec.GatewayRef.ResolvedNamespace(ep.Namespace)
				return []string{ns + "/" + ep.Spec.GatewayRef.Name}
			},
		).
		Build()
}

// fakeClientWithPolicyIndex builds a fake client with the endpoint-policy
// field index registered, required for PolicyValidator.ValidateDelete.
func fakeClientWithPolicyIndex(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithIndex(&v1alpha1.KrakenDEndpoint{}, controller.EndpointPolicyIndex,
			func(obj client.Object) []string {
				ep, ok := obj.(*v1alpha1.KrakenDEndpoint)
				if !ok {
					return nil
				}
				var refs []string
				seen := make(map[string]struct{})
				for _, entry := range ep.Spec.Endpoints {
					for _, be := range entry.Backends {
						if be.PolicyRef == nil {
							continue
						}
						key := be.PolicyRef.PolicyKey(ep.Namespace)
						if _, ok := seen[key]; ok {
							continue
						}
						seen[key] = struct{}{}
						refs = append(refs, key)
					}
				}
				return refs
			},
		).
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

func TestGatewayValidator_EERequiresLicense(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionEE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Error("expected error for EE without license")
	}
}

func TestGatewayValidator_CEWithLicenseForbidden(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
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
	if err == nil {
		t.Error("expected error for CE with license")
	}
}

func TestGatewayValidator_MutuallyExclusiveLicense(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionEE,
			Config: v1alpha1.GatewayConfig{},
			License: &v1alpha1.LicenseConfig{
				ExternalSecret: v1alpha1.ExternalSecretLicenseConfig{Enabled: true},
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "lic"},
					Key:                  "LICENSE",
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Error("expected error for mutually exclusive license sources")
	}
}

func TestGatewayValidator_MultiplePVCForbidden(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			Plugins: &v1alpha1.PluginsSpec{
				Sources: []v1alpha1.PluginSource{
					{PersistentVolumeClaimRef: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "a"}},
					{PersistentVolumeClaimRef: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "b"}},
				},
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Error("expected error for multiple PVC sources")
	}
}

func TestGatewayValidator_OpenAPIPortConflict(t *testing.T) {
	// Explicit port collision: openapi port == gateway port
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{Port: 9090},
			OpenAPI: &v1alpha1.OpenAPIExportSpec{Enabled: true, Port: 9090},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Error("expected error when openapi port equals gateway port")
	}
}

func TestGatewayValidator_OpenAPIPortDefaultConflict(t *testing.T) {
	// Default gateway port (8080) set explicitly as openapi port
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config:  v1alpha1.GatewayConfig{},
			OpenAPI: &v1alpha1.OpenAPIExportSpec{Enabled: true, Port: 8080},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Error("expected error when openapi port collides with default gateway port 8080")
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

func TestGatewayValidator_Update(t *testing.T) {
	old := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	newGW := old.DeepCopy()
	newGW.Spec.Edition = v1alpha1.EditionEE
	v := &GatewayValidator{}
	_, err := v.ValidateUpdate(context.Background(), old, newGW)
	if err == nil {
		t.Error("expected error on update to EE without license")
	}
}

func TestGatewayValidator_Delete(t *testing.T) {
	v := &GatewayValidator{}
	_, err := v.ValidateDelete(context.Background(), &v1alpha1.KrakenDGateway{})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestGatewayValidator_PostRestartJobEmptyScript(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "",
			},
		},
	}
	v := &GatewayValidator{}
	_, err := v.ValidateCreate(context.Background(), gw)
	if err == nil {
		t.Fatal("expected error for enabled postRestartJob with empty script")
	}
	if !strings.Contains(err.Error(), "script") {
		t.Errorf("expected error about script, got: %v", err)
	}
}

// TestGatewayValidator_WorkingDirOutsideTmpWithROFSWarns covers review id
// 3804144425 (#4): overriding workingDir outside the /tmp emptyDir mount
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

// TestGatewayValidator_ContainerRunAsUserZeroRejected covers review id
// 3805157408 (#1, ADMISSION REJECT — USER-CHOSEN fork): a container-level
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
// covers review id 3807285645 (#6): the pod-scope hole. Unlike a fully
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
}

// TestGatewayValidator_PodScopeRunAsUserZeroUnsetRunAsNonRootAllowed verifies
// outcome 2 from review id 3807285645 (#6) is preserved: pod-scope
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

// TestGatewayValidator_EffectiveRunAsRootCrossScopePrecedence covers review
// id 3811443593 (#6): every existing runAsUser:0 test above sets runAsUser
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
// review id 3807285627 (#2): a CR already carrying container
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
// covers the other half of review id 3807285627 (#2): the ratchet must NOT
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
// review id 3811443520 (#1): a spec stored with postRestartJob DISABLED
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
}

// TestGatewayValidator_DragonflyContainerRunAsUserZeroWithPodRunAsNonRootFalseAllowed
// verifies the escape hatch: setting podSecurityContext.runAsNonRoot: false
// alongside the container's runAsUser: 0 is accepted AT ADMISSION. This is
// admission-only, though: before fix-round review 1's change #1 (the
// container-scope uid0 fixup in mergeDragonflyContainerSecurityContext),
// admission would allow this spec while the BUILDER still rendered the
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
// distinguish this from anymore (fix-round review 1, change #2 removed
// mergeDragonflyPodSecurityContext's pod-scope fixup): a fully UNSET
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
// covers fix-round review 1, change #2: unlike postRestartJob, the
// pod-scope-unset case for Dragonfly is now REJECTED at admission (renamed
// from ...Allowed). Dragonfly's container-scope default PINS RunAsNonRoot
// (and RunAsUser/RunAsGroup) regardless of pod scope — container-scope
// always overrides pod-scope per-field at the kubelet — so a pod-scope
// runAsUser:0 never changes the Dragonfly container's effective uid; it
// only silently roots injected sidecars with no legitimate capability
// gained. There is nothing to self-heal, so
// mergeDragonflyPodSecurityContext's pod-scope fixup was removed and this
// combination is now rejected outright instead.
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
// covers item 5c of fix-round review 1: now that pod-scope runAsUser:0 with
// runAsNonRoot unset is rejected at admission for NEW/changed specs (see
// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroUnsetRunAsNonRootRejected),
// a CR that already carries that shape — grandfathered from before this
// fix-round's admission tightening — must still be ratcheted (allowed) on
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
func TestGatewayValidator_DragonflyEffectiveRunAsRootCrossScopePrecedence(t *testing.T) {
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
				RunAsUser: new(int64(999)),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser:    new(int64(0)),
				RunAsNonRoot: new(true),
			},
			wantErr: false,
			reason: "container.runAsUser:999 must win over pod.runAsUser:0 (effective uid 999, " +
				"non-root) — inverted precedence would use the pod's uid0 and reject",
		},
		{
			name: "container non-root wins over pod root, runAsNonRoot:true at container scope",
			container: &corev1.SecurityContext{
				RunAsUser:    new(int64(999)),
				RunAsNonRoot: new(true),
			},
			pod: &corev1.PodSecurityContext{
				RunAsUser: new(int64(0)),
			},
			wantErr: false,
			reason: "container.runAsUser:999 must win over pod.runAsUser:0 (effective uid 999, " +
				"non-root) — inverted precedence would use the pod's uid0 and reject",
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
		})
	}
}

// TestGatewayValidator_NegativeTmpSizeLimitRejected covers review id
// 3805157457 (#5): a negative tmpSizeLimit must be rejected.
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

func TestEndpointValidator_ConflictWarning(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	existing := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "old-ep", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}},
			},
		},
	}
	newEP := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "new-ep", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc2"}, URLPattern: "/"}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw, existing)}
	warnings, err := v.ValidateCreate(context.Background(), newEP)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected conflict warning")
	}
	w := warnings[0]
	if !strings.Contains(w, "default/old-ep") {
		t.Errorf("warning should reference existing endpoint, got: %s", w)
	}
	if !strings.Contains(w, "default/my-gw") {
		t.Errorf("warning should reference gateway, got: %s", w)
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
	v := &EndpointValidator{Client: fakeClient()}
	_, err := v.ValidateUpdate(context.Background(), ep, ep)
	if err == nil {
		t.Error("expected error on update")
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

func TestPolicyValidator_InvalidCB(t *testing.T) {
	p := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			CircuitBreaker: &v1alpha1.CircuitBreakerSpec{MaxErrors: 0, Interval: 0, Timeout: 0},
		},
	}
	v := &PolicyValidator{}
	_, err := v.ValidateCreate(context.Background(), p)
	if err == nil {
		t.Error("expected error for invalid CB")
	}
	if !strings.Contains(err.Error(), "maxErrors") {
		t.Error("expected maxErrors in error")
	}
}

func TestPolicyValidator_InvalidRL(t *testing.T) {
	p := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: 0},
		},
	}
	v := &PolicyValidator{}
	_, err := v.ValidateCreate(context.Background(), p)
	if err == nil {
		t.Error("expected error for invalid RL")
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
	v := &PolicyValidator{Client: fakeClientWithPolicyIndex(p, ep)}
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
	v := &PolicyValidator{Client: fakeClientWithPolicyIndex(p)}
	_, err := v.ValidateDelete(context.Background(), p)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestPolicyValidator_Update(t *testing.T) {
	p := &v1alpha1.KrakenDBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec: v1alpha1.KrakenDBackendPolicySpec{
			RateLimit: &v1alpha1.RateLimitSpec{MaxRate: -1},
		},
	}
	v := &PolicyValidator{}
	_, err := v.ValidateUpdate(context.Background(), p, p)
	if err == nil {
		t.Error("expected error on update")
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

func TestAutoConfigValidator_BothSources(t *testing.T) {
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
			OpenAPI: v1alpha1.OpenAPISource{
				URL:          "https://example.com/api",
				ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "cm"},
			},
			Trigger: v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Error("expected error for both sources")
	}
}

func TestAutoConfigValidator_NoSource(t *testing.T) {
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
			OpenAPI:    v1alpha1.OpenAPISource{},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Error("expected error for no source")
	}
}

func TestAutoConfigValidator_CMRequiresHostMapping(t *testing.T) {
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
			OpenAPI:    v1alpha1.OpenAPISource{ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "cm"}},
			Trigger:    v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Error("expected error for CM without hostMapping")
	}
}

func TestAutoConfigValidator_PeriodicNoInterval(t *testing.T) {
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
			Trigger:    v1alpha1.TriggerPeriodic,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Error("expected error for periodic without interval")
	}
}

func TestAutoConfigValidator_MutuallyExclusiveAuth(t *testing.T) {
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
			OpenAPI: v1alpha1.OpenAPISource{
				URL: "https://example.com/api",
				Auth: &v1alpha1.AuthConfig{
					BearerTokenSecret: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "t"},
						Key:                  "k",
					},
					BasicAuthSecret: &v1alpha1.BasicAuthSecretRef{Name: "b"},
				},
			},
			Trigger: v1alpha1.TriggerOnChange,
		},
	}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ac)
	if err == nil {
		t.Error("expected error for mutually exclusive auth")
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
	v := &AutoConfigValidator{Client: fakeClient()}
	_, err := v.ValidateUpdate(context.Background(), ac, ac)
	if err == nil {
		t.Error("expected error on update")
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

func TestEndpointValidator_ConflictSameNameDifferentNamespace(t *testing.T) {
	// Two gateways named "my-gw" in different namespaces.
	// Endpoints referencing each should NOT produce a conflict warning.
	gwA := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "ns-a"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	gwB := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "ns-b"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	// Existing endpoint points to gw in ns-a.
	existing := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ep-a", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw", Namespace: "ns-a"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}},
			},
		},
	}
	// New endpoint points to gw in ns-b — same path, different gateway.
	newEP := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-b", Namespace: "default"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "my-gw", Namespace: "ns-b"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc2"}, URLPattern: "/"}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gwA, gwB, existing)}
	warnings, err := v.ValidateCreate(context.Background(), newEP)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no conflict warning for different gateway namespaces, got %v", warnings)
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
	v := &PolicyValidator{Client: fakeClientWithPolicyIndex(p, ep)}
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

func TestEndpointValidator_ConflictCrossNamespaceEndpoints(t *testing.T) {
	// Two endpoints in DIFFERENT namespaces both targeting the same gateway.
	// Same path/method → should produce a conflict warning.
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-gw", Namespace: "infra"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Version: "2.13", Edition: v1alpha1.EditionCE,
			Config: v1alpha1.GatewayConfig{},
		},
	}
	existing := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ep-tenant-a", Namespace: "tenant-a",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "shared-gw", Namespace: "infra"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc"}, URLPattern: "/"}}},
			},
		},
	}
	newEP := &v1alpha1.KrakenDEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "ep-tenant-b", Namespace: "tenant-b"},
		Spec: v1alpha1.KrakenDEndpointSpec{
			GatewayRef: v1alpha1.GatewayRef{Name: "shared-gw", Namespace: "infra"},
			Endpoints: []v1alpha1.EndpointEntry{
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc2"}, URLPattern: "/"}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw, existing)}
	warnings, err := v.ValidateCreate(context.Background(), newEP)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected conflict warning for cross-namespace endpoints targeting same gateway")
	}
	w := warnings[0]
	if !strings.Contains(w, "tenant-a/ep-tenant-a") {
		t.Errorf("warning should include namespace-qualified conflicting endpoint, got: %s", w)
	}
	if !strings.Contains(w, "infra/shared-gw") {
		t.Errorf("warning should include namespace-qualified gateway, got: %s", w)
	}
}

func TestEndpointValidator_IntraCRDuplicate(t *testing.T) {
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
				{Endpoint: "/api", Method: "GET",
					Backends: []v1alpha1.BackendSpec{{Host: []string{"http://svc2"}, URLPattern: "/"}}},
			},
		},
	}
	v := &EndpointValidator{Client: fakeClient(gw)}
	_, err := v.ValidateCreate(context.Background(), ep)
	if err == nil {
		t.Error("expected error for duplicate (endpoint, method) within same CR")
	}
	if !strings.Contains(err.Error(), "Duplicate") {
		t.Errorf("expected Duplicate in error, got: %v", err)
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

func TestAutoConfigValidator_AdditionalEndpointEmptyPath(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{{Endpoint: ""}})

	if _, err := v.ValidateCreate(context.Background(), ac); err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}

func TestAutoConfigValidator_AdditionalEndpointBackendsAndShorthand(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{{
		Endpoint: "/x",
		Host:     "http://svc",
		Backends: []v1alpha1.BackendSpec{{Host: []string{"http://y"}, URLPattern: "/x"}},
	}})

	if _, err := v.ValidateCreate(context.Background(), ac); err == nil {
		t.Fatal("expected error for backends + shorthand")
	}
}

func TestAutoConfigValidator_AdditionalEndpointDuplicate(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{
		{Endpoint: "/live"}, {Endpoint: "/live"}, // both default to GET → duplicate
	})

	if _, err := v.ValidateCreate(context.Background(), ac); err == nil {
		t.Fatal("expected error for duplicate endpoint/method")
	}
}

func TestAutoConfigValidator_AdditionalEndpointNoLeadingSlash(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{{Endpoint: "liveness"}})

	if _, err := v.ValidateCreate(context.Background(), ac); err == nil {
		t.Fatal("expected error for endpoint without leading slash")
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

func TestAutoConfigValidator_BasePathNoLeadingSlash(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}})
	ac.Spec.AdditionalEndpointsBasePath = "api/v1" // no leading slash

	if _, err := v.ValidateCreate(context.Background(), ac); err == nil {
		t.Fatal("expected error for base path without leading slash")
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

func TestAutoConfigValidator_BasePathAndAddPathPrefixMutuallyExclusive(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"}}
	v := &AutoConfigValidator{Client: fakeClient(gw)}
	ac := newAutoConfigForAdditional([]v1alpha1.AdditionalEndpoint{{Endpoint: "/liveness"}})
	ac.Spec.AdditionalEndpointsBasePath = "/custom"
	ac.Spec.URLTransform = &v1alpha1.URLTransformSpec{AddPathPrefix: "/api/v1/quote"}

	if _, err := v.ValidateCreate(context.Background(), ac); err == nil {
		t.Fatal("expected error when both additionalEndpointsBasePath and urlTransform.addPathPrefix are set")
	}
}
