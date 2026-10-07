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
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	gatewayv1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	webhooksetup "github.com/mycarrier-devops/krakend-operator/internal/webhook"
	// +kubebuilder:scaffold:imports
)

// configCheckSlots is how many config validations the operator runs at once,
// across the gateway controller and the admission webhooks. Each krakend exec
// peaks at ~110 MB.
const configCheckSlots = 3

// krakendBinary is the krakend binary the image ships for validation.
const krakendBinary = "/usr/local/bin/krakend"

// telemetryFlushTimeout bounds the final flush of traces, metrics and logs. The
// pod's grace period is 10 seconds and the manager stops first, within
// managerStopTimeout, so manager stop plus flush take at most 9. Stdout records
// are written as they are logged: only batched OTLP data waits on the flush.
const telemetryFlushTimeout = 5 * time.Second

// managerStopTimeout bounds how long the manager waits for its runnables to
// stop. controller-runtime's own default is 30 seconds, which the pod's 10
// second grace period cannot hold: the flush that follows needs its own time.
const managerStopTimeout = 4 * time.Second

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
	os.Exit(run())
}

// run runs the operator and returns its exit code. The telemetry is set up
// before anything logs, and flushed on every return.
func run() int {
	// grpc-go requires its logger before any gRPC call, and the OTLP gRPC
	// exporters Setup builds are gRPC clients. It logs through the pipeline
	// once InstallLogging has run.
	telemetry.InstallGRPCLogging()
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
	var logs logFlags
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
	logs.bind(flag.CommandLine)
	flag.Parse()

	level, format, ignored, err := logs.resolve()
	if err != nil {
		// No log pipeline exists yet.
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	tel, err := telemetry.Setup(context.Background(), telemetryConfig(level, format))
	if err != nil {
		// The log pipeline is what failed to start.
		fmt.Fprintln(os.Stderr, "setting up telemetry:", err)
		return 1
	}
	defer flushTelemetry(tel, telemetryFlushTimeout, os.Stderr)
	telemetry.InstallLogging(tel.Logger, tel.Diagnostics)
	setupLog.Info("starting the operator", "version", version)
	if tel.Warning != nil {
		setupLog.Error(tel.Warning, "ignoring part of the telemetry configuration")
	}
	if len(ignored) > 0 {
		setupLog.Info("ignoring deprecated logging flags", "flags", ignored)
	}
	inst, err := newInstrumentation(tel)
	if err != nil {
		setupLog.Error(err, "unable to create the operator's metrics")
		return 1
	}

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
			return 1
		}
	}
	webhookServer := telemetry.TraceWebhookServer(webhook.NewServer(
		webhookServerOptions(enableWebhooks, webhookCertPath, webhookCertName, webhookCertKey, tlsOpts)),
		tel.TracerProvider)

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
			return 1
		}
	}
	metricsOptions := metricsServerOptions(
		metricsAddr, secureMetrics, metricsCertPath, metricsCertName, metricsCertKey, tlsOpts)

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to load the Kubernetes client configuration")
		return 1
	}
	telemetry.TraceKubeAPI(restConfig, tel.TracerProvider)
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:    scheme,
		NewClient: newManagerClient,
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

		// The manager stops first on SIGTERM; the telemetry flush follows within
		// the pod's grace period.
		GracefulShutdownTimeout: new(managerStopTimeout),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		return 1
	}

	krakendRenderer := renderer.New(renderer.Options{})
	krakendValidator := newKrakenDValidator(krakendBinary, inst.Tracer)

	// One checker for the whole pod: its slots bound concurrent krakend
	// executions across the gateway controller, the AutoConfig controller and
	// the admission webhooks.
	wired := wireValidation(mgr, krakendRenderer, krakendValidator, operatorUsername, inst)
	wired.AutoConfig.MaxConcurrentReconciles = autoConfigMaxConcurrentReconciles
	if enableWebhooks && operatorUsername == "" {
		setupLog.Info("no operator username: every KrakenDEndpoint write gets the admission render check")
	} else if enableWebhooks {
		setupLog.Info("admission skips the render check for AutoConfig endpoint writes from",
			"username", operatorUsername)
	}

	if err := wired.Gateway.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDGateway")
		return 1
	}
	endpoints := wireEndpointController(mgr, inst)
	if err := endpoints.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDEndpoint")
		return 1
	}
	if err := wired.Policy.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDBackendPolicy")
		return 1
	}
	if err := wired.AutoConfig.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "KrakenDAutoConfig")
		return 1
	}
	if err := registerWebhooks(mgr, enableWebhooks, func(m ctrl.Manager) error {
		return webhooksetup.SetupWebhooks(m, wired.Validators)
	}); err != nil {
		setupLog.Error(err, "unable to set up webhooks")
		return 1
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		return 1
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		return 1
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		return 1
	}
	return 0
}

// flushTelemetry flushes the traces, metrics and logs still buffered, waiting
// at most timeout. A failed flush is written to stderr: the log pipeline is
// what is being shut down.
func flushTelemetry(tel *telemetry.Telemetry, timeout time.Duration, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		// A logger of its own: nothing is left to report a failed write to stderr.
		log.New(stderr, "", 0).Println("flushing telemetry:", err)
	}
}
