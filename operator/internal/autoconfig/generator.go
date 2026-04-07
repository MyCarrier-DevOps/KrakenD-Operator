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
	"fmt"
	"strings"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GenerateInput provides the data needed to generate KrakenDEndpoint CRs.
type GenerateInput struct {
	AutoConfig     *v1alpha1.KrakenDAutoConfig
	Entries        []v1alpha1.EndpointEntry
	OperationIDs   map[string]string
	GatewayRefName string
}

// GenerateOutput contains the generated endpoint CRs and metadata.
type GenerateOutput struct {
	Endpoints         []*v1alpha1.KrakenDEndpoint
	SkippedOperations int
	DuplicateIDs      []string
}

// Generator wraps endpoint entries in KrakenDEndpoint CRs with metadata and labels.
type Generator interface {
	Generate(ctx context.Context, input GenerateInput) (*GenerateOutput, error)
}

// NewGenerator returns a Generator implementation.
func NewGenerator() Generator {
	return &endpointGenerator{}
}

type endpointGenerator struct{}

func (g *endpointGenerator) Generate(
	_ context.Context,
	input GenerateInput,
) (*GenerateOutput, error) {
	ac := input.AutoConfig
	seen := map[string]struct{}{}
	output := &GenerateOutput{}

	for _, entry := range input.Entries {
		key := entry.Endpoint + ":" + entry.Method
		opID := input.OperationIDs[key]

		if opID != "" {
			if _, exists := seen[opID]; exists {
				output.SkippedOperations++
				output.DuplicateIDs = append(output.DuplicateIDs, opID)
				continue
			}
			seen[opID] = struct{}{}
		}

		name := endpointName(ac.Name, opID, entry.Method, entry.Endpoint)
		ep := &v1alpha1.KrakenDEndpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ac.Namespace,
				Labels: map[string]string{
					"gateway.krakend.io/auto-generated": "true",
					"gateway.krakend.io/autoconfig":     ac.Name,
				},
			},
			Spec: v1alpha1.KrakenDEndpointSpec{
				GatewayRef: v1alpha1.GatewayRef{Name: input.GatewayRefName},
				Endpoints:  []v1alpha1.EndpointEntry{entry},
			},
		}
		output.Endpoints = append(output.Endpoints, ep)
	}

	return output, nil
}

// maxNameLength is the Kubernetes DNS-1123 subdomain name limit.
const maxNameLength = 253

func endpointName(autoconfigName, operationID, method, path string) string {
	var name string
	if operationID != "" {
		name = fmt.Sprintf("%s-%s", autoconfigName, sanitizeName(operationID))
	} else {
		name = fmt.Sprintf("%s-%s-%s", autoconfigName, strings.ToLower(method), sanitizePath(path))
	}
	if len(name) > maxNameLength {
		name = name[:maxNameLength]
	}
	return strings.TrimRight(name, "-")
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, s)
	return strings.Trim(s, "-")
}

func sanitizePath(path string) string {
	path = strings.TrimPrefix(path, "/")
	path = strings.ToLower(path)
	path = strings.ReplaceAll(path, "/", "-")
	path = strings.ReplaceAll(path, "{", "")
	path = strings.ReplaceAll(path, "}", "")
	return strings.Trim(path, "-")
}
