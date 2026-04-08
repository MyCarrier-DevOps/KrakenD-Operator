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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck // dot-import required by Ginkgo DSL
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck // dot-import required by Gomega DSL

	"github.com/mycarrier-devops/krakend-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "krakend-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "krakend-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "krakend-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "krakend-operator-metrics-binding"

// testNamespace is a dedicated namespace for CRD lifecycle tests.
const testNamespace = "krakend-e2e-test"

var _ = Describe("KrakenD Operator", Ordered, func() {
	var controllerPodName string

	// ── Setup ──────────────────────────────────────────────────────────────

	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("creating test namespace for CRD lifecycle tests")
		cmd = exec.Command("kubectl", "create", "ns", testNamespace)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace")
	})

	// ── Teardown ───────────────────────────────────────────────────────────

	AfterAll(func() {
		By("deleting test namespace")
		cmd := exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up the curl pod for metrics")
		cmd = exec.Command("kubectl", "delete", "pod", "curl-metrics",
			"-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("cleaning up metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	// After each test, collect diagnostic info on failure.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	// ── Controller Manager Tests ───────────────────────────────────────────

	Context("Controller Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=krakend-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
				"--dry-run=client", "-o", "yaml",
			)
			crb, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to generate ClusterRoleBinding")

			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(crb)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		It("should reconcile sample CRs successfully", func() {
			By("applying the gateway sample CR first")
			cmd := exec.Command("kubectl", "apply",
				"-f", "config/samples/gateway_v1alpha1_krakendgateway.yaml",
				"-n", "default")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply gateway sample CR")

			By("applying the remaining sample CRs")
			cmd = exec.Command("kubectl", "apply",
				"-f", "config/samples/gateway_v1alpha1_krakendbackendpolicy.yaml",
				"-f", "config/samples/gateway_v1alpha1_krakendendpoint.yaml",
				"-f", "config/samples/gateway_v1alpha1_krakendautoconfig.yaml",
				"-n", "default")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply dependent sample CRs")

			By("verifying that the gateway controller reconciled successfully")
			verifyGatewayReconciled := func(g Gomega) {
				metricsOutput := getMetricsOutput()
				g.Expect(metricsOutput).To(ContainSubstring(
					fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"}`,
						strings.ToLower("KrakenDGateway")),
				))
			}
			Eventually(verifyGatewayReconciled).Should(Succeed())

			By("cleaning up the sample CRs")
			cmd = exec.Command("kubectl", "delete",
				"-f", "config/samples/gateway_v1alpha1_krakendautoconfig.yaml",
				"-f", "config/samples/gateway_v1alpha1_krakendendpoint.yaml",
				"-f", "config/samples/gateway_v1alpha1_krakendbackendpolicy.yaml",
				"-f", "config/samples/gateway_v1alpha1_krakendgateway.yaml",
				"-n", "default", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	// ── CRD Lifecycle: Basic CE Gateway ────────────────────────────────────

	Context("CRD Lifecycle — Basic CE Gateway", func() {
		It("should create and reconcile a basic CE gateway", func() {
			By("creating a KrakenDGateway CR")
			cmd := exec.Command("kubectl", "apply", "-n", testNamespace, "-f", "-")
			cmd.Stdin = strings.NewReader(`
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDGateway
metadata:
  name: e2e-basic
spec:
  version: "2.9"
  edition: CE
  config: {}
`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Deployment is created")
			verifyDeployment := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "e2e-basic",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-basic"))
			}
			Eventually(verifyDeployment).Should(Succeed())

			By("verifying the Service is created")
			verifyService := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", "e2e-basic",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-basic"))
			}
			Eventually(verifyService).Should(Succeed())

			By("verifying no VirtualService is created (Istio disabled)")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-basic",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "VirtualService should not exist when Istio is disabled")
			}, 5*time.Second, time.Second).Should(Succeed())

			By("verifying no Dragonfly is created (Dragonfly disabled)")
			Consistently(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-basic-dragonfly",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Dragonfly should not exist when disabled")
			}, 5*time.Second, time.Second).Should(Succeed())
		})

		It("should manage endpoint lifecycle with the basic gateway", func() {
			By("creating a KrakenDEndpoint CR")
			cmd := exec.Command("kubectl", "apply", "-n", testNamespace, "-f", "-")
			cmd.Stdin = strings.NewReader(`
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDEndpoint
metadata:
  name: e2e-users
spec:
  gatewayRef:
    name: e2e-basic
  endpoints:
  - endpoint: /api/v1/users
    method: GET
    backends:
    - host:
      - http://users-svc:8080
      urlPattern: /users
`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the endpoint is Active")
			verifyEndpointActive := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "krakendendpoint", "e2e-users",
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Active"))
			}
			Eventually(verifyEndpointActive).Should(Succeed())

			By("verifying the gateway has endpoint count")
			verifyGatewayEndpoints := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "krakendgateway", "e2e-basic",
					"-n", testNamespace, "-o", "jsonpath={.status.endpointCount}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}
			Eventually(verifyGatewayEndpoints).Should(Succeed())

			By("deleting the basic gateway")
			cmd = exec.Command("kubectl", "delete", "krakendgateway", "e2e-basic",
				"-n", testNamespace, "--timeout=60s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the endpoint is Detached after gateway deletion")
			verifyEndpointDetached := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "krakendendpoint", "e2e-users",
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Detached"))
			}
			Eventually(verifyEndpointDetached).Should(Succeed())

			By("cleaning up the endpoint")
			cmd = exec.Command("kubectl", "delete", "krakendendpoint", "e2e-users",
				"-n", testNamespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
	})

	// ── CRD Lifecycle: Gateway with Dragonfly ──────────────────────────────

	Context("CRD Lifecycle — Gateway with Dragonfly", func() {
		It("should create a gateway with Dragonfly enabled and reconcile the Dragonfly CR", func() {
			By("creating a KrakenDGateway with Dragonfly enabled")
			cmd := exec.Command("kubectl", "apply", "-n", testNamespace, "-f", "-")
			cmd.Stdin = strings.NewReader(`
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDGateway
metadata:
  name: e2e-dragonfly
spec:
  version: "2.9"
  edition: CE
  config: {}
  dragonfly:
    enabled: true
    replicas: 1
`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Deployment is created")
			verifyDeployment := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "e2e-dragonfly",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-dragonfly"))
			}
			Eventually(verifyDeployment).Should(Succeed())

			By("verifying the Dragonfly CR is created")
			verifyDragonfly := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-dragonfly-dragonfly",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-dragonfly-dragonfly"))
			}
			Eventually(verifyDragonfly).Should(Succeed())

			By("verifying the Dragonfly CR has correct replicas")
			verifyDragonflyReplicas := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-dragonfly-dragonfly",
					"-n", testNamespace, "-o", "jsonpath={.spec.replicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}
			Eventually(verifyDragonflyReplicas).Should(Succeed())

			By("verifying the Dragonfly CR has owner reference to the gateway")
			verifyOwnerRef := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-dragonfly-dragonfly",
					"-n", testNamespace,
					"-o", "jsonpath={.metadata.ownerReferences[0].name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-dragonfly"))
			}
			Eventually(verifyOwnerRef).Should(Succeed())

			By("cleaning up the Dragonfly gateway")
			cmd = exec.Command("kubectl", "delete", "krakendgateway", "e2e-dragonfly",
				"-n", testNamespace, "--timeout=60s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Dragonfly CR is garbage collected")
			verifyDragonflyDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-dragonfly-dragonfly",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Dragonfly CR should be garbage collected")
			}
			Eventually(verifyDragonflyDeleted).Should(Succeed())
		})
	})

	// ── CRD Lifecycle: Gateway with Istio ──────────────────────────────────

	Context("CRD Lifecycle — Gateway with Istio", func() {
		It("should create a gateway with Istio enabled and reconcile the VirtualService", func() {
			By("creating a KrakenDGateway with Istio enabled")
			cmd := exec.Command("kubectl", "apply", "-n", testNamespace, "-f", "-")
			cmd.Stdin = strings.NewReader(`
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDGateway
metadata:
  name: e2e-istio
spec:
  version: "2.9"
  edition: CE
  config: {}
  istio:
    enabled: true
    hosts:
    - "api.example.com"
    gateways:
    - "istio-system/default-gateway"
`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Deployment is created")
			verifyDeployment := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "e2e-istio",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-istio"))
			}
			Eventually(verifyDeployment).Should(Succeed())

			By("verifying the VirtualService is created")
			verifyVS := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-istio",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-istio"))
			}
			Eventually(verifyVS).Should(Succeed())

			By("verifying the VirtualService has correct hosts")
			verifyVSHosts := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-istio",
					"-n", testNamespace, "-o", "jsonpath={.spec.hosts[0]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("api.example.com"))
			}
			Eventually(verifyVSHosts).Should(Succeed())

			By("verifying the VirtualService has correct gateways")
			verifyVSGateways := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-istio",
					"-n", testNamespace, "-o", "jsonpath={.spec.gateways[0]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("istio-system/default-gateway"))
			}
			Eventually(verifyVSGateways).Should(Succeed())

			By("verifying the VirtualService has owner reference to the gateway")
			verifyOwnerRef := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-istio",
					"-n", testNamespace,
					"-o", "jsonpath={.metadata.ownerReferences[0].name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-istio"))
			}
			Eventually(verifyOwnerRef).Should(Succeed())

			By("cleaning up the Istio gateway")
			cmd = exec.Command("kubectl", "delete", "krakendgateway", "e2e-istio",
				"-n", testNamespace, "--timeout=60s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the VirtualService is garbage collected")
			verifyVSDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-istio",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "VirtualService should be garbage collected")
			}
			Eventually(verifyVSDeleted).Should(Succeed())
		})
	})

	// ── CRD Lifecycle: Gateway with Dragonfly + Istio ──────────────────────

	Context("CRD Lifecycle — Gateway with Dragonfly and Istio", func() {
		It("should create a gateway with both Dragonfly and Istio enabled", func() {
			By("creating a KrakenDGateway with Dragonfly and Istio enabled")
			cmd := exec.Command("kubectl", "apply", "-n", testNamespace, "-f", "-")
			cmd.Stdin = strings.NewReader(`
apiVersion: gateway.krakend.io/v1alpha1
kind: KrakenDGateway
metadata:
  name: e2e-full
spec:
  version: "2.9"
  edition: CE
  config: {}
  dragonfly:
    enabled: true
    replicas: 1
  istio:
    enabled: true
    hosts:
    - "api-full.example.com"
    gateways:
    - "istio-system/default-gateway"
`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Deployment is created")
			verifyDeployment := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "deployment", "e2e-full",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-full"))
			}
			Eventually(verifyDeployment).Should(Succeed())

			By("verifying the Dragonfly CR is created")
			verifyDragonfly := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-full-dragonfly",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-full-dragonfly"))
			}
			Eventually(verifyDragonfly).Should(Succeed())

			By("verifying the VirtualService is created")
			verifyVS := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-full",
					"-n", testNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-full"))
			}
			Eventually(verifyVS).Should(Succeed())

			By("verifying both have owner references")
			verifyDFOwner := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-full-dragonfly",
					"-n", testNamespace,
					"-o", "jsonpath={.metadata.ownerReferences[0].name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-full"))
			}
			Eventually(verifyDFOwner).Should(Succeed())

			verifyVSOwner := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-full",
					"-n", testNamespace,
					"-o", "jsonpath={.metadata.ownerReferences[0].name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("e2e-full"))
			}
			Eventually(verifyVSOwner).Should(Succeed())

			By("cleaning up the full gateway")
			cmd = exec.Command("kubectl", "delete", "krakendgateway", "e2e-full",
				"-n", testNamespace, "--timeout=60s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying Dragonfly and VirtualService are garbage collected")
			verifyDFDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "dragonfly", "e2e-full-dragonfly",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyDFDeleted).Should(Succeed())

			verifyVSDeleted := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "virtualservice", "e2e-full",
					"-n", testNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}
			Eventually(verifyVSDeleted).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())

		var token tokenRequest
		err = json.Unmarshal([]byte(output), &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
