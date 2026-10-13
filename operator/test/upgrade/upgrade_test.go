//go:build upgrade

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

package upgrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

const (
	release   = "krakend-operator"
	namespace = "krakend-operator-system"

	// objectNamespace is where the specs create one object of each kind.
	objectNamespace = "default"

	gatewayCRD = "krakendgateways.gateway.krakend.io"
	policyCRD  = "krakendbackendpolicies.gateway.krakend.io"

	schemaRoot = "{.spec.versions[0].schema.openAPIV3Schema.properties."
)

// crdNames are the CRDs the chart installs.
var crdNames = []string{
	gatewayCRD,
	"krakendendpoints.gateway.krakend.io",
	policyCRD,
	"krakendautoconfigs.gateway.krakend.io",
}

// object is a custom resource that both the 0.14.0 and the current schemas admit.
type object struct {
	resource, name, manifest string
}

var objects = []object{
	{"krakendgateways", "upgrade-gateway", `
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDGateway
metadata: {name: upgrade-gateway, namespace: default}
spec: {version: "2.8", edition: CE, replicas: 1, config: {cacheTTL: "0s"}}
`},
	{"krakendendpoints", "upgrade-endpoint", `
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDEndpoint
metadata: {name: upgrade-endpoint, namespace: default}
spec:
  gatewayRef: {name: upgrade-gateway}
  endpoints:
  - endpoint: /api/users
    method: GET
    backends:
    - host: ["http://users-service.default.svc.cluster.local:8080"]
      urlPattern: /v1/users
`},
	{"krakendbackendpolicies", "upgrade-policy", `
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDBackendPolicy
metadata: {name: upgrade-policy, namespace: default}
spec: {}
`},
	{"krakendautoconfigs", "upgrade-autoconfig", `
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDAutoConfig
metadata: {name: upgrade-autoconfig, namespace: default}
spec:
  gatewayRef: {name: upgrade-gateway}
  openapi: {url: "http://users-service.default.svc.cluster.local:8080/openapi.json"}
  trigger: OnChange
`},
}

// origin is how the 0.14.0 release came to be, which decides how Helm 4 applies
// the next upgrade.
type origin struct {
	name         string
	installFlags []string
}

// origins lists the ways a release from chart 0.14.0 can have been installed
// with the helm on PATH. Helm 3 only applies client-side.
func origins() []origin {
	if helmVersion.major < 4 {
		return []origin{{name: "client-side"}}
	}
	return []origin{
		{name: "client-side", installFlags: []string{"--server-side=false"}},
		{name: "server-side"},
	}
}

// migrationFlags are the flags the upgrade guide gives for adopting the CRDs
// into an existing release.
func migrationFlags() []string {
	if helmVersion.major >= 4 {
		return []string{"--take-ownership", "--server-side=true", "--force-conflicts"}
	}
	return []string{"--take-ownership"}
}

// installOverCRDsFlags are the flags the guide gives for the first Helm install
// over CRDs that something else created.
func installOverCRDsFlags() []string {
	if helmVersion.major >= 4 {
		return []string{"--take-ownership", "--force-conflicts"}
	}
	return []string{"--take-ownership"}
}

// fieldManagerOfHelm is the manager and operation Helm leaves on a CRD it
// applied last: Helm 4 applies server-side after the migration, Helm 3 patches.
func fieldManagerOfHelm() string {
	if helmVersion.major >= 4 {
		return "helm/Apply"
	}
	return "helm/Update"
}

var _ = Describe("The operator CRDs under Helm "+helmVersion.raw, func() {
	AfterEach(resetCluster)

	It("installs the CRDs with the release, keeps them on uninstall and adopts them again", func() {
		helmInstall("installing the repository chart", newChart)
		expectCRDsAdopted("after a fresh install")
		expectCRDsInManifest("after a fresh install", len(crdNames))

		uids := createObjects()
		helmUninstall("uninstalling the release")
		expectCRDsAdopted("after helm uninstall")
		expectObjects("after helm uninstall", uids)

		helmInstall("installing again under the same name and namespace, without --take-ownership", newChart)
		expectCRDsAdopted("after installing over the kept CRDs")
		expectCRDsInManifest("after installing over the kept CRDs", len(crdNames))
		expectObjects("after installing over the kept CRDs", uids)
	})

	for _, o := range origins() {
		It("migrates a release installed "+o.name+" from chart "+publishedChartVersion, func() {
			By("installing chart " + publishedChartVersion + ", which ships the CRDs in crds/")
			helmInstall("installing chart "+publishedChartVersion, oldChart, o.installFlags...)
			Expect(hasConfigEdition()).To(BeFalse(),
				"chart %s already has status.configEdition in its gateway CRD: the migration would prove nothing",
				publishedChartVersion)

			By("applying the CRDs by hand, as the upgrade guide used to say")
			mustRun("applying chart "+publishedChartVersion+"'s CRDs with kubectl apply --server-side --force-conflicts, "+
				"so field manager kubectl owns .spec.versions", "kubectl", "apply", "--server-side", "--force-conflicts",
				"-f", filepath.Join(oldChart, "crds"))
			uids := createObjects()

			if o.name == "server-side" {
				By("failing to adopt the CRDs without --force-conflicts, and rolling back")
				annotateKeep()
				out, err := helmUpgradeErr(newChart, "--take-ownership")
				Expect(err).To(HaveOccurred(),
					"helm upgrade --take-ownership without --force-conflicts succeeded; expected a server-side apply "+
						"conflict with field manager kubectl on .spec.versions")
				Expect(out).To(And(ContainSubstring("conflict"), ContainSubstring(`"kubectl"`),
					ContainSubstring(".spec.versions")), "the failed upgrade did not name a conflict with kubectl on .spec.versions")
				helmRollback("rolling back to revision 1 after the failed upgrade", 1)
				expectCRDsAdopted("after rolling back the failed upgrade")
				expectObjects("after rolling back a failed upgrade", uids)
			}

			By("migrating with the documented command")
			annotateKeep()
			helmUpgrade("upgrading with "+strings.Join(migrationFlags(), " "), newChart, migrationFlags()...)
			migrated := currentRevision()
			expectCRDsAdopted("after the migration upgrade")
			Expect(hasConfigEdition()).To(BeTrue(),
				"after the migration upgrade the live gateway CRD has no status.configEdition: helm did not move the schema")
			expectCRDsInManifest("after the migration upgrade", len(crdNames))
			expectObjects("after the migration upgrade", uids)
			expectHelmOwnsSpecVersions("after the migration upgrade")

			By("upgrading to a chart whose policy CRD has one more property")
			newer := newerChart()
			helmUpgrade("upgrading to a chart with a changed policy CRD", newer)
			Expect(hasProbeProperty()).To(BeTrue(),
				"after helm upgrade the live CRD %s has no upgradeProbe property: helm did not move the schema", policyCRD)
			expectCRDsAdopted("after upgrading to a changed CRD")

			By("rolling back to the migration revision")
			helmRollback("rolling back to revision "+strconv.Itoa(migrated), migrated)
			Expect(hasProbeProperty()).To(BeFalse(),
				"after helm rollback the live CRD %s still has the upgradeProbe property: helm did not move the schema back",
				policyCRD)
			expectCRDsAdopted("after rolling back to the migration revision")

			By("rolling back to revision 1, from before the CRDs were templates")
			helmRollback("rolling back to revision 1", 1)
			expectCRDsAdopted("after rolling back to revision 1")
			Expect(hasConfigEdition()).To(BeTrue(),
				"after rolling back to revision 1 the live gateway CRD lost status.configEdition: helm moved the schema back")
			expectCRDsInManifest("after rolling back to revision 1", 0)
			expectObjects("after rolling back to revision 1", uids)

			By("upgrading again without --take-ownership")
			helmUpgrade("upgrading to the chart again without --take-ownership", newChart)
			expectCRDsAdopted("after upgrading again")
			expectCRDsInManifest("after upgrading again", len(crdNames))

			By("uninstalling")
			helmUninstall("uninstalling the migrated release")
			expectCRDsAdopted("after helm uninstall")
			expectObjects("after helm uninstall", uids)
		})
	}

	It("adopts CRDs that were applied by hand before the first Helm install", func() {
		By("applying chart " + publishedChartVersion + "'s CRDs client-side, as a kustomize install does")
		mustRun("applying the 0.14.0 CRDs with kubectl apply", "kubectl", "apply", "-f", filepath.Join(oldChart, "crds"))
		uids := createObjects()
		annotateKeep()

		helmInstall("installing with "+strings.Join(installOverCRDsFlags(), " "), newChart, installOverCRDsFlags()...)
		expectCRDsAdopted("after installing over CRDs that were applied by hand")
		Expect(hasConfigEdition()).To(BeTrue(),
			"after the install the live gateway CRD has no status.configEdition: helm did not move the schema")
		expectCRDsInManifest("after installing over CRDs that were applied by hand", len(crdNames))
		expectObjects("after installing over CRDs that were applied by hand", uids)
		expectHelmOwnsSpecVersions("after installing over CRDs that were applied by hand")
	})
})

// releaseArgs are the arguments every install and upgrade shares. The operator's
// webhook and pod are left out: the suite has no operator image and no
// cert-manager, and the specs are about the CRDs.
func releaseArgs(chart string) []string {
	return []string{release, chart, "--namespace", namespace, "--set", "webhooks.enabled=false",
		"--set", "replicaCount=0"}
}

func helmInstall(step, chart string, extra ...string) string {
	GinkgoHelper()
	args := append([]string{"install"}, releaseArgs(chart)...)
	return mustRun(step, "helm", append(append(args, "--create-namespace"), extra...)...)
}

func helmUpgrade(step, chart string, extra ...string) string {
	GinkgoHelper()
	return mustRun(step, "helm", append(append([]string{"upgrade"}, releaseArgs(chart)...), extra...)...)
}

// helmUpgradeErr runs an upgrade that the spec expects may fail.
func helmUpgradeErr(chart string, extra ...string) (string, error) {
	return run("helm", append(append([]string{"upgrade"}, releaseArgs(chart)...), extra...)...)
}

func helmRollback(step string, revision int) {
	GinkgoHelper()
	mustRun(step, "helm", "rollback", release, strconv.Itoa(revision), "--namespace", namespace)
}

func helmUninstall(step string) {
	GinkgoHelper()
	mustRun(step, "helm", "uninstall", release, "--namespace", namespace)
}

// currentRevision is the newest revision of the release.
func currentRevision() int {
	GinkgoHelper()
	out := mustRun("reading the history of the release", "helm", "history", release, "--namespace", namespace,
		"--output", "json")
	var history []struct {
		Revision int `json:"revision"`
	}
	Expect(json.Unmarshal([]byte(out), &history)).To(Succeed(), "helm history printed %q", out)
	Expect(history).NotTo(BeEmpty(), "release %s has no revisions", release)
	revision := 0
	for _, h := range history {
		revision = max(revision, h.Revision)
	}
	return revision
}

// annotateKeep marks the live CRDs, as the upgrade guide does before a migration:
// Helm reads the annotation from the live object when an upgrade or a rollback
// drops a resource.
func annotateKeep() {
	GinkgoHelper()
	mustRun("annotating the live CRDs helm.sh/resource-policy=keep", "kubectl",
		append(append([]string{"annotate", "crd"}, crdNames...), "helm.sh/resource-policy=keep", "--overwrite")...)
}

// crdField is one jsonpath value of a live CRD.
func crdField(crd, jsonpath string) string {
	GinkgoHelper()
	return mustRun("reading "+jsonpath+" of CRD "+crd, "kubectl", "get", "crd", crd, "-o", "jsonpath="+jsonpath)
}

func hasConfigEdition() bool {
	GinkgoHelper()
	return crdField(gatewayCRD, schemaRoot+"status.properties.configEdition.type}") != ""
}

func hasProbeProperty() bool {
	GinkgoHelper()
	return crdField(policyCRD, schemaRoot+"spec.properties.upgradeProbe.type}") != ""
}

// expectCRDsAdopted expects every live CRD to name the release and carry keep.
func expectCRDsAdopted(step string) {
	GinkgoHelper()
	for _, crd := range crdNames {
		for _, want := range []struct{ what, jsonpath, value string }{
			{"annotation helm.sh/resource-policy", `{.metadata.annotations.helm\.sh/resource-policy}`, "keep"},
			{"annotation meta.helm.sh/release-name", `{.metadata.annotations.meta\.helm\.sh/release-name}`, release},
			{"annotation meta.helm.sh/release-namespace", `{.metadata.annotations.meta\.helm\.sh/release-namespace}`, namespace},
			{"label app.kubernetes.io/managed-by", `{.metadata.labels.app\.kubernetes\.io/managed-by}`, "Helm"},
		} {
			Expect(crdField(crd, want.jsonpath)).To(Equal(want.value), "%s: CRD %s: %s", step, crd, want.what)
		}
	}
}

// expectCRDsInManifest expects the stored manifest of the release to hold want CRDs.
func expectCRDsInManifest(step string, want int) {
	GinkgoHelper()
	out := mustRun("reading the manifest of the release", "helm", "get", "manifest", release, "--namespace", namespace)
	got := len(regexp.MustCompile(`(?m)^kind: CustomResourceDefinition$`).FindAllString(out, -1))
	Expect(got).To(Equal(want), "%s: CRDs in the manifest of release %s", step, release)
}

// expectHelmOwnsSpecVersions expects helm to be the only field manager of the
// schema of every CRD. Each CRD's schema changed since 0.14.0, so the migration
// takes the field over from kubectl.
func expectHelmOwnsSpecVersions(step string) {
	GinkgoHelper()
	for _, crd := range crdNames {
		Expect(specVersionsOwners(crd)).To(Equal([]string{fieldManagerOfHelm()}),
			"%s: field managers (manager/operation) of .spec.versions of CRD %s", step, crd)
	}
}

// specVersionsOwners lists the managers that own .spec.versions of a live CRD,
// as manager/operation.
func specVersionsOwners(crd string) []string {
	GinkgoHelper()
	out := mustRun("reading the managed fields of CRD "+crd, "kubectl", "get", "crd", crd, "--show-managed-fields",
		"-o", "json")
	var doc struct {
		Metadata struct {
			ManagedFields []struct {
				Manager   string `json:"manager"`
				Operation string `json:"operation"`
				FieldsV1  struct {
					Spec map[string]json.RawMessage `json:"f:spec"`
				} `json:"fieldsV1"`
			} `json:"managedFields"`
		} `json:"metadata"`
	}
	Expect(json.Unmarshal([]byte(out), &doc)).To(Succeed(), "kubectl printed %q", out)
	var owners []string
	for _, mf := range doc.Metadata.ManagedFields {
		if _, ok := mf.FieldsV1.Spec["f:versions"]; ok {
			owners = append(owners, mf.Manager+"/"+mf.Operation)
		}
	}
	slices.Sort(owners)
	return owners
}

// createObjects creates one object of each kind, after the CRDs are served, and
// returns their UIDs by name.
func createObjects() map[string]string {
	GinkgoHelper()
	for _, crd := range crdNames {
		mustRun("waiting for CRD "+crd+" to be Established", "kubectl", "wait", "--for=condition=Established",
			"crd/"+crd, "--timeout=60s")
	}
	for _, o := range objects {
		_, err := runWithStdin(o.manifest, "kubectl", "create", "-f", "-")
		Expect(err).NotTo(HaveOccurred(), "creating %s %s", o.resource, o.name)
	}
	return objectUIDs()
}

func objectUIDs() map[string]string {
	GinkgoHelper()
	uids := map[string]string{}
	for _, o := range objects {
		uids[o.resource+"/"+o.name] = strings.TrimSpace(mustRun("reading the UID of "+o.resource+" "+o.name, "kubectl",
			"get", o.resource, o.name, "--namespace", objectNamespace, "-o", "jsonpath={.metadata.uid}"))
	}
	return uids
}

// expectObjects expects every object to exist with the UID it had when it was
// created: deleting a CRD deletes its objects, so a new UID means helm deleted one.
func expectObjects(step string, want map[string]string) {
	GinkgoHelper()
	Expect(objectUIDs()).To(Equal(want), "%s: UIDs of the custom resources", step)
}

// newerChart copies the repository chart and adds one optional property to the
// schema of the policy CRD.
func newerChart() string {
	GinkgoHelper()
	chart := copyRepositoryChart("newer")
	file := filepath.Join(chart, "files", "crds", "gateway.krakend.io_krakendbackendpolicies.yaml")
	data, err := os.ReadFile(file)
	Expect(err).NotTo(HaveOccurred(),
		"the chart has no files/crds/gateway.krakend.io_krakendbackendpolicies.yaml: the CRDs are not chart templates")
	var crd map[string]any
	Expect(yaml.Unmarshal(data, &crd)).To(Succeed())
	versions := crd["spec"].(map[string]any)["versions"].([]any)
	schema := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	specProps := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	specProps["upgradeProbe"] = map[string]any{"type": "string"}
	out, err := yaml.Marshal(crd)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(file, append([]byte("---\n"), out...), 0o600)).To(Succeed())
	return chart
}

// resetCluster removes everything a spec left behind, so each spec starts from a
// cluster with no release, no CRDs and no objects.
func resetCluster() {
	_, _ = run("helm", "uninstall", release, "--namespace", namespace) // no release is fine
	for _, o := range objects {
		_, _ = run("kubectl", "delete", o.resource, o.name, "--namespace", objectNamespace, "--ignore-not-found")
	}
	mustRun("deleting the CRDs", "kubectl", append([]string{"delete", "crd", "--ignore-not-found", "--timeout=180s"},
		crdNames...)...)
	mustRun("deleting the release namespace", "kubectl", "delete", "namespace", namespace, "--ignore-not-found",
		"--timeout=180s")
}
