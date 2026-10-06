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

package main

import (
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	gatewayv1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	webhooksetup "github.com/mycarrier-devops/krakend-operator/internal/webhook"
	// +kubebuilder:scaffold:imports
)

// configCheckSlots is how many config validations the operator runs at once,
// across the gateway controller and the admission webhooks. Each krakend exec
// peaks at ~110 MB.
const configCheckSlots = 3

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() { //nolint:gochecknoinits // required by controller-runtime scheme registration
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(gatewayv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var enableWebhooks bool
	var operatorUsername string
	var autoConfigMaxConcurrentReconciles int
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Serve the validating admission webhooks. With false the webhook server does not start "+
			"and invalid objects are caught only at render time.")
	flag.StringVar(&operatorUsername, "operator-username", defaultOperatorUsername(),
		"Username of the operator's own API requests. Its writes to KrakenDEndpoints a KrakenDAutoConfig "+
			"controls skip the admission render check. Defaults to the pod's ServiceAccount "+
			"(system:serviceaccount:$POD_NAMESPACE:$POD_SERVICE_ACCOUNT); empty disables the exemption.")
	flag.IntVar(&autoConfigMaxConcurrentReconciles, "autoconfig-max-concurrent-reconciles", 4,
		"How many KrakenDAutoConfigs reconcile at once. Each reconcile fetches its OpenAPI spec over the network; "+
			"values below 1 mean 1.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// The webhook and metrics servers start their own certificate watchers
	// when given a certificate directory, and those run on every replica, so
	// a renewed certificate reaches standbys too. With --metrics-cert-path
	// unset, controller-runtime generates a self-signed metrics certificate,
	// which suits development; for production use cert-manager (see
	// config/default/kustomization.yaml and config/prometheus/kustomization.yaml).
	if webhookCertWatchNeeded(enableWebhooks, webhookCertPath) {
		setupLog.Info("Serving webhooks with the provided certificates",
			"webhook-cert-path", webhookCertPath,
			"webhook-cert-name", webhookCertName,
			"webhook-cert-key", webhookCertKey,
		)
		if err := checkServingFiles(webhookCertPath, webhookCertName, webhookCertKey); err != nil {
			setupLog.Error(err, "Webhook certificate files are not readable")
			os.Exit(1)
		}
	}
	webhookServer := webhook.NewServer(
		webhookServerOptions(enableWebhooks, webhookCertPath, webhookCertName, webhookCertKey, tlsOpts))

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	if metricsCertPath != "" {
		setupLog.Info("Serving metrics with the provided certificates",
			"metrics-cert-path", metricsCertPath,
			"metrics-cert-name", metricsCertName,
			"metrics-cert-key", metricsCertKey,
		)
		if err := checkServingFiles(metricsCertPath, metricsCertName, metricsCertKey); err != nil {
			setupLog.Error(err, "Metrics certificate files are not readable")
			os.Exit(1)
		}
	}
	metricsOptions := metricsServerOptions(
		metricsAddr, secureMetrics, metricsCertPath, metricsCertName, metricsCertKey, tlsOpts)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: controller.UncachedObjects()},
		},
		Cache:                  cache.Options{ByObject: controller.CacheByObject()},
		Metrics:                metricsOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "krakend-operator-leader",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	krakendRenderer := renderer.New(renderer.Options{})
	krakendValidator := renderer.NewValidator(renderer.ValidatorOptions{
		Executor:   renderer.NewKrakenDExecutor("/usr/local/bin/krakend"),
		BinaryPath: "/usr/local/bin/krakend",
	})

	// One checker for the whole pod: its slots bound concurrent krakend
	// executions across the gateway controller, the AutoConfig controller and
	// the admission webhooks.
	wired := wireValidation(mgr, krakendRenderer, krakendValidator, operatorUsername)
	wired.AutoConfig.MaxConcurrentReconciles = autoConfigMaxConcurrentReconciles
	if enableWebhooks && operatorUsername == "" {
		setupLog.Info("no operator username: every KrakenDEndpoint write gets the admission render check")
	} else if enableWebhooks {
		setupLog.Info("admission skips the render check for AutoConfig endpoint writes from",
			"username", operatorUsername)
	}

	if err := wired.Gateway.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDGateway")
		os.Exit(1)
	}
	if err := (&controller.KrakenDEndpointReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("krakendendpoint-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDEndpoint")
		os.Exit(1)
	}
	if err := (&controller.KrakenDBackendPolicyReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("krakendbackendpolicy-controller"),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDBackendPolicy")
		os.Exit(1)
	}
	if err := wired.AutoConfig.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDAutoConfig")
		os.Exit(1)
	}
	if err := registerWebhooks(mgr, enableWebhooks, func(m ctrl.Manager) error {
		return webhooksetup.SetupWebhooks(m, wired.Validators)
	}); err != nil {
		setupLog.Error(err, "unable to set up webhooks")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
