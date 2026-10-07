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
	"os"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/mycarrier-devops/krakend-operator/internal/autoconfig"
	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
	"github.com/mycarrier-devops/krakend-operator/internal/controller"
	"github.com/mycarrier-devops/krakend-operator/internal/renderer"
	"github.com/mycarrier-devops/krakend-operator/internal/telemetry"
	licenseutil "github.com/mycarrier-devops/krakend-operator/internal/util/license"
	webhooksetup "github.com/mycarrier-devops/krakend-operator/internal/webhook"
)

// instrumentationScope names the operator's tracer and meter.
const instrumentationScope = "github.com/mycarrier-devops/krakend-operator"

// gatewayCheckWorkers is how many gateway checks the gateway controller runs at
// once: it reconciles with this many workers (wireValidation sets its
// MaxConcurrentReconciles) and each reconcile holds one checker slot at a time.
const gatewayCheckWorkers = 1

// autoConfigCheckSlots is how many checker slots the AutoConfig prechecks and
// the policy checks may hold at once, between them: the checker's slots, less
// one the controllers never take, left to admission (which waits against a
// short deadline), less the gateway controller's. Together the controllers
// never hold more than all but one slot; concurrent admission requests can
// still take the rest.
const autoConfigCheckSlots = configCheckSlots - 1 - gatewayCheckWorkers

// policyMemoSize is how many policy verdicts the policy controller remembers,
// the most recent first. A verdict depends only on the policy's content, so
// reconciles of content already judged run no krakend.
const policyMemoSize = 256

// instrumentation is what every component records its spans and metrics
// with: the pod's one tracer and one metrics recorder.
type instrumentation struct {
	Tracer  trace.Tracer
	Metrics *telemetry.OperatorMetrics
}

// gatewayMetrics is the gateway controller's port, nil without a recorder: a
// nil *OperatorMetrics in the interface would not be nil.
func (i instrumentation) gatewayMetrics() controller.GatewayMetrics {
	if i.Metrics == nil {
		return nil
	}
	return i.Metrics
}

// autoConfigMetrics is the AutoConfig controller's port, nil without a
// recorder.
func (i instrumentation) autoConfigMetrics() controller.AutoConfigMetrics {
	if i.Metrics == nil {
		return nil
	}
	return i.Metrics
}

// validation is everything that holds the pod's one config checker.
type validation struct {
	Checker    *configcheck.Checker
	Gateway    *controller.KrakenDGatewayReconciler
	AutoConfig *controller.KrakenDAutoConfigReconciler
	Policy     *controller.KrakenDBackendPolicyReconciler
	Validators webhooksetup.Validators
}

// wireValidation builds the pod's one config checker and the parts that use
// it. The checker's slots bound concurrent krakend executions across the
// gateway controller, the AutoConfig controller and the admission webhooks, so
// they must share it.
func wireValidation(
	mgr ctrl.Manager, r renderer.Renderer, v renderer.Validator, operatorUsername string, inst instrumentation,
) validation {
	checker := configcheck.New(mgr.GetClient(), r, v, configCheckSlots, inst.Tracer)
	// The AutoConfig prechecks and the policy checks hold at most
	// autoConfigCheckSlots of the checker's slots between them, however many
	// workers there are.
	controllerSlots := make(chan struct{}, autoConfigCheckSlots)
	return validation{
		Checker: checker,
		Gateway: &controller.KrakenDGatewayReconciler{
			Client:        mgr.GetClient(),
			Scheme:        mgr.GetScheme(),
			Recorder:      mgr.GetEventRecorderFor("krakendgateway-controller"),
			Renderer:      r,
			Checker:       checker,
			Clock:         clock.RealClock{},
			APIReader:     mgr.GetAPIReader(),
			LicenseParser: licenseutil.NewX509LicenseParser(),
			// Each gateway reconcile holds one checker slot, so the workers
			// are the slots the AutoConfig bound leaves the gateway.
			MaxConcurrentReconciles: gatewayCheckWorkers,
			Metrics:                 inst.gatewayMetrics(),
			Tracer:                  inst.Tracer,
		},
		AutoConfig: &controller.KrakenDAutoConfigReconciler{
			Client:       mgr.GetClient(),
			Scheme:       mgr.GetScheme(),
			Recorder:     mgr.GetEventRecorderFor("krakendautoconfig-controller"),
			Fetcher:      autoconfig.NewFetcher(mgr.GetClient(), inst.Tracer),
			CUEEvaluator: autoconfig.NewCUEEvaluator(),
			Filter:       autoconfig.NewFilter(),
			Generator:    autoconfig.NewGenerator(),
			Checker:      checker,
			CheckSlots:   controllerSlots,
			Clock:        clock.RealClock{},
			Metrics:      inst.autoConfigMetrics(),
			Tracer:       inst.Tracer,
		},
		Policy: &controller.KrakenDBackendPolicyReconciler{
			Client:     mgr.GetClient(),
			Scheme:     mgr.GetScheme(),
			Recorder:   mgr.GetEventRecorderFor("krakendbackendpolicy-controller"),
			APIReader:  mgr.GetAPIReader(),
			Tracer:     inst.Tracer,
			Checker:    checker,
			Memo:       configcheck.NewLRUMemo(policyMemoSize),
			CheckSlots: controllerSlots,
		},
		Validators: webhooksetup.NewValidators(
			mgr.GetClient(), mgr.GetAPIReader(), checker, operatorUsername, inst.Tracer),
	}
}

// wireEndpointController builds the endpoint reconciler, which resolves
// references and holds no config checker.
func wireEndpointController(mgr ctrl.Manager, inst instrumentation) *controller.KrakenDEndpointReconciler {
	return &controller.KrakenDEndpointReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("krakendendpoint-controller"),
		Tracer:   inst.Tracer,
	}
}

// newKrakenDValidator returns the validator that runs the krakend binary at
// path, each run a span of tracer.
func newKrakenDValidator(path string, tracer trace.Tracer) *renderer.KrakenDValidator {
	return renderer.NewValidator(renderer.ValidatorOptions{
		Executor:   telemetry.TraceExecutor(renderer.NewKrakenDExecutor(path), tracer),
		BinaryPath: path,
	})
}

// newManagerClient builds the manager's client as controller-runtime does,
// with each read an event on the active span: reads the cache serves send no
// request, so the transport cannot see them.
func newManagerClient(config *rest.Config, options client.Options) (client.Client, error) {
	c, err := client.New(config, options)
	if err != nil {
		return nil, err
	}
	return telemetry.ReadEvents(c), nil
}

// telemetryConfig is the telemetry setup of the operator process: its build
// version, the pod the downward API names, the log level and format of its
// flags, and controller-runtime's registry behind /metrics.
func telemetryConfig(level otellog.Severity, format telemetry.LogFormat) telemetry.Config {
	return telemetry.Config{
		ServiceVersion: version,
		PodName:        os.Getenv("POD_NAME"),
		PodNamespace:   os.Getenv("POD_NAMESPACE"),
		LogLevel:       level,
		LogFormat:      format,
		Registerer:     ctrlmetrics.Registry,
	}
}

// newInstrumentation returns the operator's tracer and metrics recorder over
// tel's providers.
func newInstrumentation(tel *telemetry.Telemetry) (instrumentation, error) {
	metrics, err := telemetry.NewOperatorMetrics(tel.MeterProvider.Meter(instrumentationScope))
	if err != nil {
		return instrumentation{}, err
	}
	return instrumentation{Tracer: tel.TracerProvider.Tracer(instrumentationScope), Metrics: metrics}, nil
}
