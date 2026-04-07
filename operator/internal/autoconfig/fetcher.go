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
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	maxRedirects = 5
	fetchTimeout = 30 * time.Second
	maxBodyBytes = 10 * 1024 * 1024 // 10 MiB
)

// FetchSource describes where to fetch an OpenAPI spec from.
type FetchSource struct {
	URL               string
	ConfigMapRef      *v1alpha1.ConfigMapKeyRef
	Auth              *v1alpha1.AuthConfig
	AllowClusterLocal bool
	Namespace         string
}

// FetchResult contains the fetched spec data and its checksum.
type FetchResult struct {
	Data     []byte
	Checksum string
}

// Fetcher fetches OpenAPI specs from URLs or ConfigMaps.
type Fetcher interface {
	Fetch(ctx context.Context, source FetchSource) (*FetchResult, error)
}

// NewFetcher creates a Fetcher that can read from HTTP and ConfigMaps.
func NewFetcher(k8sClient client.Client) Fetcher {
	return &httpFetcher{
		client:           k8sClient,
		strictTransport:  SSRFSafeTransportWithPolicy(false),
		lenientTransport: SSRFSafeTransportWithPolicy(true),
	}
}

type httpFetcher struct {
	client           client.Client
	strictTransport  http.RoundTripper
	lenientTransport http.RoundTripper
}

func (f *httpFetcher) Fetch(ctx context.Context, source FetchSource) (*FetchResult, error) {
	if source.ConfigMapRef != nil {
		return f.fetchFromConfigMap(ctx, source)
	}
	return f.fetchFromURL(ctx, source)
}

func (f *httpFetcher) fetchFromConfigMap(ctx context.Context, source FetchSource) (*FetchResult, error) {
	cm := &corev1.ConfigMap{}
	if err := f.client.Get(ctx, types.NamespacedName{
		Name:      source.ConfigMapRef.Name,
		Namespace: source.Namespace,
	}, cm); err != nil {
		return nil, fmt.Errorf("getting configmap %s/%s: %w", source.Namespace, source.ConfigMapRef.Name, err)
	}

	key := source.ConfigMapRef.Key
	if key == "" {
		key = "openapi.json"
	}
	data, ok := cm.Data[key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in configmap %s/%s", key, source.Namespace, source.ConfigMapRef.Name)
	}

	raw := []byte(data)
	checksum := fmt.Sprintf("%x", sha256.Sum256(raw))
	return &FetchResult{Data: raw, Checksum: checksum}, nil
}

func (f *httpFetcher) fetchFromURL(ctx context.Context, source FetchSource) (*FetchResult, error) {
	if source.URL == "" {
		return nil, fmt.Errorf("no URL or ConfigMapRef provided")
	}

	parsed, err := url.Parse(source.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q: only http and https are allowed", parsed.Scheme)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	if source.Auth != nil {
		if err := f.applyAuth(ctx, req, source.Auth, source.Namespace); err != nil {
			return nil, fmt.Errorf("applying auth: %w", err)
		}
	}

	transport := f.strictTransport
	if source.AllowClusterLocal {
		transport = f.lenientTransport
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects (max %d)", maxRedirects)
			}
			return nil
		},
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", source.URL, err)
	}
	defer func() { resp.Body.Close() }() //nolint:errcheck // best-effort close on HTTP response

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, source.URL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if int64(len(body)) > maxBodyBytes {
		return nil, fmt.Errorf("response body exceeds maximum size of %d bytes", maxBodyBytes)
	}

	checksum := fmt.Sprintf("%x", sha256.Sum256(body))
	return &FetchResult{Data: body, Checksum: checksum}, nil
}

func (f *httpFetcher) applyAuth(
	ctx context.Context,
	req *http.Request,
	auth *v1alpha1.AuthConfig,
	namespace string,
) error {
	if auth.BearerTokenSecret != nil {
		secret := &corev1.Secret{}
		if err := f.client.Get(ctx, types.NamespacedName{
			Name:      auth.BearerTokenSecret.Name,
			Namespace: namespace,
		}, secret); err != nil {
			return fmt.Errorf("getting bearer token secret %s/%s: %w", namespace, auth.BearerTokenSecret.Name, err)
		}
		token, ok := secret.Data[auth.BearerTokenSecret.Key]
		if !ok {
			return fmt.Errorf(
				"key %q not found in secret %s/%s",
				auth.BearerTokenSecret.Key, namespace, auth.BearerTokenSecret.Name,
			)
		}
		req.Header.Set("Authorization", "Bearer "+string(token))
	}

	if auth.BasicAuthSecret != nil {
		secret := &corev1.Secret{}
		if err := f.client.Get(ctx, types.NamespacedName{
			Name:      auth.BasicAuthSecret.Name,
			Namespace: namespace,
		}, secret); err != nil {
			return fmt.Errorf("getting basic auth secret %s/%s: %w", namespace, auth.BasicAuthSecret.Name, err)
		}
		usernameKey := auth.BasicAuthSecret.UsernameKey
		if usernameKey == "" {
			usernameKey = "username"
		}
		passwordKey := auth.BasicAuthSecret.PasswordKey
		if passwordKey == "" {
			passwordKey = "password"
		}
		usernameVal, ok := secret.Data[usernameKey]
		if !ok {
			return fmt.Errorf("key %q not found in secret %s/%s", usernameKey, namespace, auth.BasicAuthSecret.Name)
		}
		passwordVal, ok := secret.Data[passwordKey]
		if !ok {
			return fmt.Errorf("key %q not found in secret %s/%s", passwordKey, namespace, auth.BasicAuthSecret.Name)
		}
		req.SetBasicAuth(string(usernameVal), string(passwordVal))
	}

	return nil
}

func normalizeIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func validateIP(ip net.IP) error {
	if ip.IsLoopback() {
		return fmt.Errorf("loopback address")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("link-local address")
	}
	if isIPv6ULA(ip) {
		return fmt.Errorf("IPv6 ULA address")
	}
	return nil
}

func isIPv6ULA(ip net.IP) bool {
	return (len(ip) == net.IPv6len && ip[0] == 0xfc) || (len(ip) == net.IPv6len && ip[0] == 0xfd)
}

func isRFC1918(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return (ip4[0] == 10) ||
		(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
		(ip4[0] == 192 && ip4[1] == 168)
}

// ValidateIPAllowPrivate blocks loopback, link-local, and ULA but permits RFC 1918 private ranges.
func ValidateIPAllowPrivate(ip net.IP) error {
	return validateIP(ip)
}

// ValidateIPStrict blocks all private ranges including RFC 1918.
func ValidateIPStrict(ip net.IP) error {
	if err := validateIP(ip); err != nil {
		return err
	}
	if isRFC1918(ip) {
		return fmt.Errorf("private address")
	}
	return nil
}

// SSRFSafeTransportWithPolicy creates a transport with configurable private-range policy.
func SSRFSafeTransportWithPolicy(allowClusterLocal bool) *http.Transport {
	validate := ValidateIPStrict
	if allowClusterLocal {
		validate = ValidateIPAllowPrivate
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("splitting host:port: %w", err)
			}

			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolving %s: %w", host, err)
			}

			if len(ips) == 0 {
				return nil, fmt.Errorf("resolving %s: no IP addresses returned", host)
			}

			for _, ipAddr := range ips {
				ip := normalizeIP(ipAddr.IP)
				if err := validate(ip); err != nil {
					return nil, fmt.Errorf("blocked address %s: %w", ip, err)
				}
			}

			dialer := &net.Dialer{Timeout: 10 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
}
