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

package controller

import (
	"errors"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// discoveryDownMapper fails every lookup the way an unreachable API server does.
type discoveryDownMapper struct{ meta.RESTMapper }

func (discoveryDownMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, errors.New("the server is currently unable to handle the request")
}

func TestInstalledOptionalKinds(t *testing.T) {
	installed, missing, err := installedOptionalKinds(optionalCRDMapper(virtualServiceGVK))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(installed, []schema.GroupVersionKind{virtualServiceGVK}) {
		t.Errorf("installed = %v, want only the VirtualService kind", installed)
	}
	if !slices.Equal(missing, []schema.GroupVersionKind{dragonflyGVK, externalSecretGVK}) {
		t.Errorf("missing = %v, want Dragonfly and ExternalSecret", missing)
	}

	if _, _, err := installedOptionalKinds(discoveryDownMapper{}); err == nil {
		t.Error("a discovery failure must fail startup, not silently skip a watch")
	}
}
