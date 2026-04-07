package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
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
		t.Error("expected conflict warning")
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
