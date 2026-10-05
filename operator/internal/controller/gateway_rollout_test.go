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
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
)

// servedGateway is a CE gateway serving config "A" whose Deployment has
// settled, in a cluster whose API server bumps the Deployment generation on a
// spec change.
type servedGateway struct {
	c  client.Client
	r  *KrakenDGatewayReconciler
	gw *v1alpha1.KrakenDGateway
	// cached, while set, is what every read of the Deployment returns, as an
	// informer cache does until it has seen the controller's own update.
	cached *appsv1.Deployment
}

func serveGateway(t *testing.T) *servedGateway {
	t.Helper()
	s := &servedGateway{gw: servingGateway("A", convergedImage)}
	s.c = fakeClientBuilder().WithObjects(s.gw, settledDeployment(s.gw, "A")).WithStatusSubresource(s.gw).
		WithInterceptorFuncs(withGenerationBumps(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption,
			) error {
				if dep, ok := obj.(*appsv1.Deployment); ok && s.cached != nil {
					s.cached.DeepCopyInto(dep)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})).Build()
	s.r = newTestGatewayReconciler(s.c, renderOutput("A"), &mockValidator{})
	return s
}

// reconcileWhileCacheLags reconciles with every Deployment read returning
// the Deployment as it stands now.
func (s *servedGateway) reconcileWhileCacheLags(t *testing.T) *v1alpha1.KrakenDGateway {
	t.Helper()
	s.cached = s.deployment(t)
	defer func() { s.cached = nil }()
	return s.reconcile(t)
}

// reconcile runs one reconcile pass and fails the test on an error.
func (s *servedGateway) reconcile(t *testing.T) *v1alpha1.KrakenDGateway {
	t.Helper()
	if err := reconcileGateway(t, s.r, s.gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return getGateway(t, s.c, s.gw)
}

// editSpec changes the stored gateway's spec.
func (s *servedGateway) editSpec(t *testing.T, edit func(spec *v1alpha1.KrakenDGatewaySpec)) {
	t.Helper()
	stored := getGateway(t, s.c, s.gw)
	edit(&stored.Spec)
	if err := s.c.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
}

// deployment returns the gateway Deployment as stored.
func (s *servedGateway) deployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	getObject(t, s.c, s.gw, s.gw.Name, &dep)
	return &dep
}

// deploymentControllerObserves is the Deployment controller acting on the
// Deployment's latest spec: status.observedGeneration catches up, and
// edit sets the replica counts it reports.
func (s *servedGateway) deploymentControllerObserves(t *testing.T, edit func(st *appsv1.DeploymentStatus)) {
	t.Helper()
	dep := s.deployment(t)
	dep.Status.ObservedGeneration = dep.Generation
	if edit != nil {
		edit(&dep.Status)
	}
	if err := s.c.Status().Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

// settled is every replica updated and available.
func settled(st *appsv1.DeploymentStatus) {
	st.Replicas, st.UpdatedReplicas, st.AvailableReplicas, st.ReadyReplicas = 1, 1, 1, 1
}

// requireProgressing fails unless gw reports Progressing with the given
// status, and a Ready that is True only when ready.
func requireProgressing(t *testing.T, gw *v1alpha1.KrakenDGateway, want metav1.ConditionStatus, ready bool) {
	t.Helper()
	progressing := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != want {
		t.Errorf("Progressing = %+v, want status %s", progressing, want)
	}
	if got := meta.IsStatusConditionTrue(gw.Status.Conditions, v1alpha1.ConditionReady); got != ready {
		t.Errorf("Ready True = %v, want %v (phase %s)", got, ready, gw.Status.Phase)
	}
}

// limitCPU is a spec change that rolls the pods and changes no annotation the
// operator tracks.
func limitCPU(spec *v1alpha1.KrakenDGatewaySpec) {
	spec.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("500m"),
	}}
}

func TestGatewayReconcile_TemplateOnlyChangeIsProgressingUntilTheDeploymentConverges(t *testing.T) {
	s := serveGateway(t)
	s.editSpec(t, limitCPU)

	got := s.reconcile(t)
	requireProgressing(t, got, metav1.ConditionTrue, false)
	if got.Status.Phase != v1alpha1.PhaseDeploying {
		t.Errorf("phase = %s, want %s while the pods roll", got.Status.Phase, v1alpha1.PhaseDeploying)
	}

	s.deploymentControllerObserves(t, settled)
	requireProgressing(t, s.reconcile(t), metav1.ConditionFalse, true)
}

func TestGatewayReconcile_TemplateChangeIsProgressingWhileTheCacheLags(t *testing.T) {
	s := serveGateway(t)
	s.editSpec(t, limitCPU)

	got := s.reconcileWhileCacheLags(t)

	requireProgressing(t, got, metav1.ConditionTrue, false)
}

func TestGatewayReconcile_RecreatedDeploymentIsDeployingNotReadyAndNotError(t *testing.T) {
	s := serveGateway(t)
	if err := s.c.Delete(context.Background(), s.deployment(t)); err != nil {
		t.Fatal(err)
	}

	got := s.reconcile(t)
	requireProgressing(t, got, metav1.ConditionTrue, false)
	if got.Status.Phase != v1alpha1.PhaseDeploying {
		t.Errorf("phase = %s on the pass that recreates the Deployment, want %s", got.Status.Phase, v1alpha1.PhaseDeploying)
	}

	// The Deployment controller has observed it and its pod is not available yet.
	s.deploymentControllerObserves(t, func(st *appsv1.DeploymentStatus) {
		st.Replicas, st.UpdatedReplicas = 1, 1
		st.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: "MinimumReplicasUnavailable",
		}}
	})
	got = s.reconcile(t)
	requireProgressing(t, got, metav1.ConditionTrue, false)
	if got.Status.Phase != v1alpha1.PhaseDeploying {
		t.Errorf("phase = %s while the new Deployment starts, want %s", got.Status.Phase, v1alpha1.PhaseDeploying)
	}
}

func TestGatewayReconcile_RolloutStaysReportedAfterAFailedStatusWrite(t *testing.T) {
	s := settleLicensedGateway(t)
	failGatewayStatus := true
	s.r.Client = interceptor.NewClient(s.c.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(
			ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption,
		) error {
			if _, ok := obj.(*v1alpha1.KrakenDGateway); ok && failGatewayStatus {
				return errors.New("the status write failed")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	s.setLicenseBytes(t, "renewed certificate")

	if err := reconcileGateway(t, s.r, s.gw); err == nil {
		t.Fatal("the failed status write must fail the pass")
	}
	// The write that rolls the pods to the renewed license went through; the
	// Deployment controller has observed it and is surging a new pod.
	dep := s.deployment(t)
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: dep.Generation, Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 2, ReadyReplicas: 2,
	}
	if err := s.c.Status().Update(context.Background(), dep); err != nil {
		t.Fatal(err)
	}

	failGatewayStatus = false
	if err := reconcileGateway(t, s.r, s.gw); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	requireProgressing(t, getGateway(t, s.c, s.gw), metav1.ConditionTrue, false)
}

// stuck puts the gateway in the state two passes of a Deployment that missed
// its progress deadline leave behind: the Deployment reports
// ProgressDeadlineExceeded for the generation it has observed, and the gateway
// reports RolloutFailed.
func (s *servedGateway) stuck(t *testing.T) {
	t.Helper()
	s.deploymentControllerObserves(t, func(st *appsv1.DeploymentStatus) {
		st.Replicas, st.UpdatedReplicas, st.AvailableReplicas, st.ReadyReplicas = 2, 1, 1, 1
		st.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
		}}
	})
	stored := getGateway(t, s.c, s.gw)
	for _, cond := range []metav1.Condition{
		{Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonRolloutFailed},
		{Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonRolloutFailed},
	} {
		meta.SetStatusCondition(&stored.Status.Conditions, cond)
	}
	if err := s.c.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayReconcile_FixPushedDuringAStuckRolloutReadsDeployingNotError(t *testing.T) {
	s := serveGateway(t)
	s.stuck(t)
	rec := s.r.Recorder.(*record.FakeRecorder)
	s.r.Renderer = renderOutput("B") // the fix

	for pass, name := range []string{"the pass that pushes the fix", "the pass before the Deployment controller observes it"} {
		got := s.reconcile(t)
		progressing := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionProgressing)
		if progressing == nil || progressing.Status != metav1.ConditionTrue ||
			progressing.Reason != v1alpha1.ReasonConfigDeployed {
			t.Errorf("%s: Progressing = %+v, want True/%s", name, progressing, v1alpha1.ReasonConfigDeployed)
		}
		if got.Status.Phase != v1alpha1.PhaseDeploying {
			t.Errorf("%s: phase = %s, want %s", name, got.Status.Phase, v1alpha1.PhaseDeploying)
		}
		if available := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionAvailable); available != nil &&
			available.Reason == v1alpha1.ReasonRolloutFailed {
			t.Errorf("%s: Available = %+v, want it no longer RolloutFailed", name, available)
		}
		if n := eventsWithReason(rec, v1alpha1.ReasonRolloutFailed); n != 0 {
			t.Errorf("%s (pass %d): %d RolloutFailed events, want none for a rollout that replaced the failed one", name, pass, n)
		}
	}
}

// failDeploymentWrites makes every update of the gateway Deployment fail.
func (s *servedGateway) failDeploymentWrites() {
	s.r.Client = interceptor.NewClient(s.c.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				return errors.New("the Deployment update was rejected")
			}
			return c.Update(ctx, obj, opts...)
		},
	})
}

func TestGatewayReconcile_FailedDeploymentWriteLeavesProgressingAlone(t *testing.T) {
	s := serveGateway(t)
	s.failDeploymentWrites()
	s.editSpec(t, func(spec *v1alpha1.KrakenDGatewaySpec) { spec.Image = "img:v2" })

	if err := reconcileGateway(t, s.r, s.gw); err == nil {
		t.Fatal("a rejected Deployment update must fail the pass")
	}

	requireProgressing(t, getGateway(t, s.c, s.gw), metav1.ConditionFalse, true)
}

func TestGatewayReconcile_RollingRestartsCountsOnlyTheWriteThatChangedTheTemplate(t *testing.T) {
	s := serveGateway(t)
	s.editSpec(t, func(spec *v1alpha1.KrakenDGatewaySpec) { spec.Image = "img:v2" })
	base := testutil.ToFloat64(rollingRestarts)
	restarts := func() float64 { return testutil.ToFloat64(rollingRestarts) - base }

	s.failDeploymentWrites()
	if err := reconcileGateway(t, s.r, s.gw); err == nil {
		t.Fatal("a rejected Deployment update must fail the pass")
	}
	if got := restarts(); got != 0 {
		t.Errorf("rollingRestarts rose by %v on a pass whose Deployment write failed, want 0", got)
	}

	s.r.Client = s.c
	s.reconcile(t)
	if got := restarts(); got != 1 {
		t.Errorf("rollingRestarts rose by %v after the write that rolled the pods, want 1", got)
	}

	s.reconcile(t)
	if got := restarts(); got != 1 {
		t.Errorf("rollingRestarts rose by %v after a pass that wrote nothing, want it still 1", got)
	}
}

func TestGatewayReconcile_HPAScaleDoesNotReportARollout(t *testing.T) {
	s := serveGateway(t)
	s.editSpec(t, func(spec *v1alpha1.KrakenDGatewaySpec) {
		spec.Autoscaling = &v1alpha1.AutoscalingSpec{MaxReplicas: 5}
	})
	// The autoscaler takes over spec.replicas; the gateway settles on it.
	s.reconcile(t)
	s.deploymentControllerObserves(t, settled)
	requireProgressing(t, s.reconcile(t), metav1.ConditionFalse, true)

	// The autoscaler scales up. A change to spec.replicas bumps the Deployment
	// generation, and the Deployment controller has not observed it yet.
	scaled := s.deployment(t)
	scaled.Spec.Replicas = new(int32(3))
	if err := s.c.Update(context.Background(), scaled); err != nil {
		t.Fatal(err)
	}
	if dep := s.deployment(t); dep.Generation <= dep.Status.ObservedGeneration {
		t.Fatalf("generation %d, observed %d: the scale must outrun the Deployment controller",
			dep.Generation, dep.Status.ObservedGeneration)
	}

	requireProgressing(t, s.reconcile(t), metav1.ConditionFalse, true)
}

func TestGatewayReconcile_CommunityUpgradeRollReportsDeployingWithAnUnchangedRender(t *testing.T) {
	s := serveGateway(t)
	// The Deployment an earlier operator version left: it mounts the ConfigMap
	// named after the gateway and carries no image annotation.
	legacy := s.deployment(t)
	delete(legacy.Spec.Template.Annotations, resources.ImageAnnotation)
	for i, v := range legacy.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil {
			legacy.Spec.Template.Spec.Volumes[i].ConfigMap.Name = s.gw.Name
		}
	}
	if err := s.c.Update(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	s.deploymentControllerObserves(t, settled)

	got := s.reconcile(t)

	requireProgressing(t, got, metav1.ConditionTrue, false)
	if got.Status.Phase != v1alpha1.PhaseDeploying {
		t.Errorf("phase = %s, want %s while the pods roll to the content-addressed ConfigMap",
			got.Status.Phase, v1alpha1.PhaseDeploying)
	}
}

func TestInspectDeploymentStatus_ATemplateThatIsNotTheWantedOneIsARollout(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(dep *appsv1.Deployment)
	}{
		{
			name: "annotations of another config",
			mutate: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Annotations[resources.PostRestartJobChecksumAnnotation] = "B"
			},
		},
		{
			name: "mounts another config ConfigMap",
			mutate: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
					Name: "config",
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "another"},
					}},
				}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := convergedGatewayAt("A")
			dep := makeConvergedDeployment(gw, "A")
			tc.mutate(dep)
			r := &KrakenDGatewayReconciler{Client: fakeClientBuilder().Build(), Scheme: testScheme(), Recorder: fakeRecorder()}

			r.inspectDeploymentStatus(context.Background(), gw, convergedInputs("A"), deploymentObservation{dep: dep}, nil)

			if progressing := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionProgressing); !condTrue(progressing) {
				t.Errorf("Progressing = %+v, want True: the Deployment's template is not the wanted one", progressing)
			}
		})
	}
}

func TestInspectDeploymentStatus_AMissedDeadlineOfAnotherTemplateIsNotTheCurrentRollout(t *testing.T) {
	gw := convergedGatewayAt("A")
	dep := makeConvergedDeployment(gw, "B") // the template of a config that is no longer wanted
	dep.Generation, dep.Status.ObservedGeneration = 2, 2
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	}}
	r := &KrakenDGatewayReconciler{Client: fakeClientBuilder().Build(), Scheme: testScheme(), Recorder: fakeRecorder()}

	r.inspectDeploymentStatus(context.Background(), gw, convergedInputs("A"), deploymentObservation{dep: dep}, nil)

	progressing := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionProgressing)
	if progressing == nil || progressing.Status != metav1.ConditionTrue {
		t.Errorf("Progressing = %+v, want True: the deadline judged a template that is not the wanted one", progressing)
	}
}

func TestInspectDeploymentStatus_ResetsRolloutFailedAvailabilityFromTheDeployment(t *testing.T) {
	cases := []struct {
		name                string
		deploymentAvailable corev1.ConditionStatus
		want                *metav1.ConditionStatus
	}{
		{name: "available Deployment", deploymentAvailable: corev1.ConditionTrue, want: new(metav1.ConditionTrue)},
		{name: "Deployment that reports nothing", deploymentAvailable: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := convergedGatewayAt("A")
			meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
				Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonRolloutFailed,
			})
			dep := makeConvergedDeployment(gw, "A")
			dep.Generation = 2 // a newer spec the Deployment controller has not observed
			dep.Status.Conditions = []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
			}}
			if tc.deploymentAvailable != "" {
				dep.Status.Conditions = append(dep.Status.Conditions,
					appsv1.DeploymentCondition{Type: appsv1.DeploymentAvailable, Status: tc.deploymentAvailable})
			}
			r := &KrakenDGatewayReconciler{Client: fakeClientBuilder().Build(), Scheme: testScheme(), Recorder: fakeRecorder()}

			r.inspectDeploymentStatus(context.Background(), gw, convergedInputs("A"),
				deploymentObservation{dep: dep, templateChanged: true}, nil)

			available := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionAvailable)
			switch {
			case tc.want == nil && available != nil:
				t.Errorf("Available = %+v, want it removed", available)
			case tc.want != nil && (available == nil || available.Status != *tc.want):
				t.Errorf("Available = %+v, want status %s", available, *tc.want)
			}
		})
	}
}
