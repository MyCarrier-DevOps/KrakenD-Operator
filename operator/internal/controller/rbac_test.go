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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// managerRoleVerbs is the least-privilege manager ClusterRole, keyed by
// "group/resource". architecture/README.md, section 14, names the call
// that needs each verb.
var managerRoleVerbs = map[string][]string{
	"/configmaps":      {"create", "delete", "get", "list", "watch"},
	"/events":          {"create", "patch"},
	"/secrets":         {"get", "list", "watch"},
	"/serviceaccounts": {"create", "delete", "get", "list", "update", "watch"},
	"/services":        {"create", "delete", "get", "list", "update", "watch"},
	"apps/deployments": {"create", "delete", "get", "list", "update", "watch"},
	"apps/replicasets": {"list"},
	"authorization.k8s.io/subjectaccessreviews":        {"create"},
	"autoscaling/horizontalpodautoscalers":             {"create", "delete", "get", "list", "update", "watch"},
	"batch/jobs":                                       {"create", "delete", "get", "list", "watch"},
	"dragonflydb.io/dragonflies":                       {"create", "delete", "get", "list", "update", "watch"},
	"external-secrets.io/externalsecrets":              {"create", "delete", "get", "list", "update", "watch"},
	"gateway.krakend.io/krakendautoconfigs":            {"get", "list", "watch"},
	"gateway.krakend.io/krakendautoconfigs/finalizers": {"update"},
	"gateway.krakend.io/krakendautoconfigs/status":     {"update"},
	"gateway.krakend.io/krakendbackendpolicies":        {"get", "list", "update", "watch"},
	"gateway.krakend.io/krakendbackendpolicies/status": {"update"},
	"gateway.krakend.io/krakendendpoints":              {"create", "delete", "get", "list", "update", "watch"},
	"gateway.krakend.io/krakendendpoints/status":       {"patch"},
	"gateway.krakend.io/krakendgateways":               {"get", "list", "watch"},
	"gateway.krakend.io/krakendgateways/finalizers":    {"update"},
	"gateway.krakend.io/krakendgateways/status":        {"update"},
	"networking.istio.io/virtualservices":              {"create", "delete", "get", "list", "update", "watch"},
	"policy/poddisruptionbudgets":                      {"create", "delete", "get", "list", "update", "watch"},
}

// TestManagerRoleGrantsOnlyUsedVerbs pins config/rbac/role.yaml, generated
// from the +kubebuilder:rbac markers by `make manifests`, to
// managerRoleVerbs. A new API call needs its verb added in both places; a
// removed call needs its verb removed from both.
func TestManagerRoleGrantsOnlyUsedVerbs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(data, &role); err != nil {
		t.Fatalf("decoding role.yaml: %v", err)
	}
	got := map[string][]string{}
	for _, rule := range role.Rules {
		if len(rule.NonResourceURLs) > 0 || len(rule.ResourceNames) > 0 {
			t.Errorf("rule %+v is scoped by URL or resource name; the table models neither", rule)
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				key := group + "/" + resource
				got[key] = append(got[key], rule.Verbs...)
			}
		}
	}
	keys := slices.Sorted(maps.Keys(got))
	for k := range maps.Keys(managerRoleVerbs) {
		if _, ok := got[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, key := range keys {
		verbs := slices.Clone(got[key])
		slices.Sort(verbs)
		verbs = slices.Compact(verbs)
		if want := managerRoleVerbs[key]; !slices.Equal(verbs, want) {
			t.Errorf("%s: role grants [%s], want [%s]", key, strings.Join(verbs, " "), strings.Join(want, " "))
		}
	}
}
