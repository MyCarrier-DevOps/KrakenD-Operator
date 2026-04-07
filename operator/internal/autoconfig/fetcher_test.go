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

package autoconfig

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func TestFetcher_ConfigMap(t *testing.T) {
	spec := `{"openapi": "3.0.0"}`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-spec", Namespace: "default"},
		Data:       map[string]string{"openapi.json": spec},
	}

	f := NewFetcher(fakeClient(cm))
	result, err := f.Fetch(context.Background(), FetchSource{
		ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "my-spec"},
		Namespace:    "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Data) != spec {
		t.Errorf("expected %s, got %s", spec, string(result.Data))
	}
	expectedChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(spec)))
	if result.Checksum != expectedChecksum {
		t.Errorf("expected checksum %s, got %s", expectedChecksum, result.Checksum)
	}
}

func TestFetcher_ConfigMapCustomKey(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-spec", Namespace: "default"},
		Data:       map[string]string{"spec.yaml": "openapi: '3.0.0'"},
	}

	f := NewFetcher(fakeClient(cm))
	result, err := f.Fetch(context.Background(), FetchSource{
		ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "my-spec", Key: "spec.yaml"},
		Namespace:    "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Data) != "openapi: '3.0.0'" {
		t.Errorf("unexpected data: %s", string(result.Data))
	}
}

func TestFetcher_ConfigMapNotFound(t *testing.T) {
	f := NewFetcher(fakeClient())
	_, err := f.Fetch(context.Background(), FetchSource{
		ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "missing"},
		Namespace:    "default",
	})
	if err == nil {
		t.Error("expected error for missing configmap")
	}
}

func TestFetcher_ConfigMapKeyNotFound(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-spec", Namespace: "default"},
		Data:       map[string]string{"other.json": "{}"},
	}

	f := NewFetcher(fakeClient(cm))
	_, err := f.Fetch(context.Background(), FetchSource{
		ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "my-spec", Key: "spec.json"},
		Namespace:    "default",
	})
	if err == nil {
		t.Error("expected error for missing key")
	}
}

func TestFetcher_HTTPSuccess(t *testing.T) {
	body := `{"openapi": "3.0.0"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	fetcher := &httpFetcher{
		client:           fakeClient(),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	result, err := fetcher.Fetch(context.Background(), FetchSource{URL: ts.URL})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Data) != body {
		t.Errorf("expected %s, got %s", body, string(result.Data))
	}
}

func TestFetcher_HTTPBadStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	fetcher := &httpFetcher{
		client:           fakeClient(),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	_, err := fetcher.Fetch(context.Background(), FetchSource{URL: ts.URL})
	if err == nil {
		t.Error("expected error for 500 status")
	}
}

func TestFetcher_UnsupportedScheme(t *testing.T) {
	f := NewFetcher(fakeClient())
	_, err := f.Fetch(context.Background(), FetchSource{URL: "ftp://example.com/spec.json"})
	if err == nil {
		t.Error("expected error for ftp scheme")
	}
}

func TestFetcher_NoSource(t *testing.T) {
	f := NewFetcher(fakeClient())
	_, err := f.Fetch(context.Background(), FetchSource{})
	if err == nil {
		t.Error("expected error for no source")
	}
}

func TestValidateIP_Loopback(t *testing.T) {
	if err := validateIP(net.ParseIP("127.0.0.1")); err == nil {
		t.Error("expected error for loopback")
	}
}

func TestValidateIP_LinkLocal(t *testing.T) {
	if err := validateIP(net.ParseIP("169.254.1.1")); err == nil {
		t.Error("expected error for link-local")
	}
}

func TestValidateIP_Public(t *testing.T) {
	if err := validateIP(net.ParseIP("8.8.8.8")); err != nil {
		t.Errorf("expected no error for public IP, got %v", err)
	}
}

func TestValidateIPStrict_Private(t *testing.T) {
	privateIPs := []string{"10.0.0.1", "172.16.0.1", "192.168.1.1"}
	for _, ip := range privateIPs {
		if err := ValidateIPStrict(net.ParseIP(ip)); err == nil {
			t.Errorf("expected strict to block %s", ip)
		}
	}
}

func TestValidateIPAllowPrivate(t *testing.T) {
	privateIPs := []string{"10.0.0.1", "172.16.0.1", "192.168.1.1"}
	for _, ip := range privateIPs {
		if err := ValidateIPAllowPrivate(net.ParseIP(ip)); err != nil {
			t.Errorf("expected allow-private to permit %s, got %v", ip, err)
		}
	}
}

func TestNormalizeIP_IPv4Mapped(t *testing.T) {
	ip := net.ParseIP("::ffff:127.0.0.1")
	normalized := normalizeIP(ip)
	if normalized.To4() == nil {
		t.Error("expected IPv4 after normalization")
	}
}

func TestIsRFC1918(t *testing.T) {
	tests := []struct {
		ip       string
		expected bool
	}{
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false},
		{"192.168.1.1", true},
		{"8.8.8.8", false},
	}
	for _, tt := range tests {
		if got := isRFC1918(net.ParseIP(tt.ip)); got != tt.expected {
			t.Errorf("isRFC1918(%s) = %v, want %v", tt.ip, got, tt.expected)
		}
	}
}

func TestFetcher_BearerTokenAuth(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"openapi": "3.0.0"}`)
	}))
	defer ts.Close()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("super-secret-token")},
	}

	fetcher := &httpFetcher{
		client:           fakeClient(secret),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	_, err := fetcher.Fetch(context.Background(), FetchSource{
		URL:       ts.URL,
		Namespace: "default",
		Auth: &v1alpha1.AuthConfig{
			BearerTokenSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-token"},
				Key:                  "token",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer super-secret-token" {
		t.Errorf("expected Bearer auth header, got %q", gotAuth)
	}
}

func TestFetcher_BasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		fmt.Fprint(w, `{"openapi": "3.0.0"}`)
	}))
	defer ts.Close()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-creds", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("p@ss")},
	}

	fetcher := &httpFetcher{
		client:           fakeClient(secret),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	_, err := fetcher.Fetch(context.Background(), FetchSource{
		URL:       ts.URL,
		Namespace: "default",
		Auth: &v1alpha1.AuthConfig{
			BasicAuthSecret: &v1alpha1.BasicAuthSecretRef{Name: "my-creds"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotOK || gotUser != "admin" || gotPass != "p@ss" {
		t.Errorf("expected basic auth admin/p@ss, got %s/%s (ok=%v)", gotUser, gotPass, gotOK)
	}
}

func TestFetcher_BasicAuthCustomKeys(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		fmt.Fprint(w, `{"openapi": "3.0.0"}`)
	}))
	defer ts.Close()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-creds", Namespace: "default"},
		Data:       map[string][]byte{"usr": []byte("bob"), "pwd": []byte("s3cret")},
	}

	fetcher := &httpFetcher{
		client:           fakeClient(secret),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	_, err := fetcher.Fetch(context.Background(), FetchSource{
		URL:       ts.URL,
		Namespace: "default",
		Auth: &v1alpha1.AuthConfig{
			BasicAuthSecret: &v1alpha1.BasicAuthSecretRef{
				Name:        "my-creds",
				UsernameKey: "usr",
				PasswordKey: "pwd",
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotOK || gotUser != "bob" || gotPass != "s3cret" {
		t.Errorf("expected basic auth bob/s3cret, got %s/%s (ok=%v)", gotUser, gotPass, gotOK)
	}
}

func TestFetcher_BearerTokenSecretNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openapi": "3.0.0"}`)
	}))
	defer ts.Close()

	fetcher := &httpFetcher{
		client:           fakeClient(),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	_, err := fetcher.Fetch(context.Background(), FetchSource{
		URL:       ts.URL,
		Namespace: "default",
		Auth: &v1alpha1.AuthConfig{
			BearerTokenSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing"},
				Key:                  "token",
			},
		},
	})
	if err == nil {
		t.Error("expected error for missing bearer secret")
	}
}

func TestFetcher_BearerTokenKeyNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openapi": "3.0.0"}`)
	}))
	defer ts.Close()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-token", Namespace: "default"},
		Data:       map[string][]byte{"other-key": []byte("val")},
	}

	fetcher := &httpFetcher{
		client:           fakeClient(secret),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	_, err := fetcher.Fetch(context.Background(), FetchSource{
		URL:       ts.URL,
		Namespace: "default",
		Auth: &v1alpha1.AuthConfig{
			BearerTokenSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "my-token"},
				Key:                  "token",
			},
		},
	})
	if err == nil {
		t.Error("expected error for missing key in bearer secret")
	}
}

func TestFetcher_BasicAuthSecretNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openapi": "3.0.0"}`)
	}))
	defer ts.Close()

	fetcher := &httpFetcher{
		client:           fakeClient(),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	_, err := fetcher.Fetch(context.Background(), FetchSource{
		URL:       ts.URL,
		Namespace: "default",
		Auth: &v1alpha1.AuthConfig{
			BasicAuthSecret: &v1alpha1.BasicAuthSecretRef{Name: "missing"},
		},
	})
	if err == nil {
		t.Error("expected error for missing basic auth secret")
	}
}

func TestFetcher_AllowClusterLocal(t *testing.T) {
	body := `{"openapi": "3.0.0"}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer ts.Close()

	fetcher := &httpFetcher{
		client:           fakeClient(),
		strictTransport:  http.DefaultTransport,
		lenientTransport: http.DefaultTransport,
	}

	result, err := fetcher.Fetch(context.Background(), FetchSource{
		URL:               ts.URL,
		AllowClusterLocal: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Data) != body {
		t.Errorf("expected %s, got %s", body, string(result.Data))
	}
}

func TestSSRFSafeTransport_BlocksLoopback(t *testing.T) {
	transport := SSRFSafeTransportWithPolicy(false)
	client := &http.Client{Transport: transport}

	// Attempt to connect to loopback — the DialContext should reject it.
	_, err := client.Get("http://127.0.0.1:1/test")
	if err == nil {
		t.Error("expected SSRF transport to block loopback")
	}
}

func TestSSRFSafeTransport_BlocksPrivateStrict(t *testing.T) {
	transport := SSRFSafeTransportWithPolicy(false)
	client := &http.Client{Transport: transport}

	// 10.x.x.x should be blocked in strict mode
	_, err := client.Get("http://10.0.0.1:1/test")
	if err == nil {
		t.Error("expected strict SSRF transport to block RFC1918 address")
	}
}

func TestSSRFSafeTransport_AllowsPrivateLenient(t *testing.T) {
	// In lenient mode, RFC1918 should pass the IP validation stage.
	// We verify by starting a listener on the loopback interface — which is
	// blocked by both modes — and instead rely on the existing ValidateIPAllowPrivate
	// unit tests for IP-level behavior. Here we verify the transport is wired correctly.
	transport := SSRFSafeTransportWithPolicy(true)
	if transport == nil {
		t.Fatal("expected non-nil transport")
	}
	if transport.DialContext == nil {
		t.Fatal("expected custom DialContext")
	}
	// Strict mode should block 10.x, lenient should not.
	// Use a short-lived context to avoid long timeouts on unreachable IPs.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := transport.DialContext(ctx, "tcp", "10.0.0.1:80")
	if err == nil {
		return // unexpectedly connected — not blocked, which is correct
	}
	errStr := err.Error()
	for _, blocked := range []string{"blocked address", "private address"} {
		if contains(errStr, blocked) {
			t.Errorf("lenient mode should not block RFC1918, got: %v", err)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestValidateIP_IPv6ULA(t *testing.T) {
	// fc00::/7 addresses should be blocked
	ulaIPs := []string{"fc00::1", "fd00::1", "fdab:cdef:1234::1"}
	for _, ip := range ulaIPs {
		if err := validateIP(net.ParseIP(ip)); err == nil {
			t.Errorf("expected ULA address %s to be blocked", ip)
		}
	}
}

func TestValidateIPStrict_PublicAllowed(t *testing.T) {
	if err := ValidateIPStrict(net.ParseIP("8.8.8.8")); err != nil {
		t.Errorf("expected public IP to pass strict validation, got %v", err)
	}
}

func TestIsRFC1918_IPv6(t *testing.T) {
	// IPv6 address should return false
	if isRFC1918(net.ParseIP("::1")) {
		t.Error("IPv6 loopback should not be RFC1918")
	}
	if isRFC1918(net.ParseIP("2001:db8::1")) {
		t.Error("IPv6 documentation address should not be RFC1918")
	}
}

func TestNormalizeIP_PureIPv6(t *testing.T) {
	ip := net.ParseIP("2001:db8::1")
	normalized := normalizeIP(ip)
	if normalized.To4() != nil {
		t.Error("pure IPv6 should remain IPv6 after normalization")
	}
}

func TestFetcher_InvalidURL(t *testing.T) {
	f := NewFetcher(fakeClient())
	_, err := f.Fetch(context.Background(), FetchSource{URL: "://invalid"})
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}
