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

package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func makeGWWithJob(script string) *v1alpha1.KrakenDGateway {
	return &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Edition: v1alpha1.EditionCE,
			Version: "2.13",
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  script,
			},
		},
	}
}

func makeConvergedDeployment(gw *v1alpha1.KrakenDGateway, checksum string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gw.Name, Namespace: gw.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						resources.PostRestartJobChecksumAnnotation: checksum,
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:          1,
			UpdatedReplicas:   1,
			AvailableReplicas: 1,
		},
	}
}

func TestReconcilePostRestartJob_CreatesWhenConverged(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected one job, got %d", len(jobs.Items))
	}
	wantChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing expected checksum: %v", err)
	}
	if gw.Status.LastPostRestartJobChecksum != wantChecksum {
		t.Fatalf("status checksum not recorded: got %q want %q", gw.Status.LastPostRestartJobChecksum, wantChecksum)
	}
}

func TestReconcilePostRestartJob_SkipsWhenNotConverged(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")
	dep.Status.UpdatedReplicas = 0
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected no job created, got %d", len(jobs.Items))
	}
}

func TestReconcilePostRestartJob_SkipsWhenChecksumMismatch(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "old-checksum")
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "new-checksum"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected no job before rollout of new checksum")
	}
}

func TestReconcilePostRestartJob_Idempotent(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      resources.PostRestartJobName(gw, jobChecksum),
		Namespace: gw.Namespace,
	}}
	c := fakeClientBuilder().WithObjects(gw, dep, existing).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly one job (idempotent), got %d", len(jobs.Items))
	}
}

// TestReconcilePostRestartJob_SkipsRecreateAfterTTLGC covers nhig root
// cause 1: TTLSecondsAfterFinished GC's the finished Job object, but the
// gateway's status already recorded that this exact (config, spec)
// revision ran. The reconciler must NOT recreate (and thus not re-execute)
// the Job purely because the object disappeared.
func TestReconcilePostRestartJob_SkipsRecreateAfterTTLGC(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	// Simulate: the Job already ran and was TTL-GC'd (no Job object exists),
	// but status still records the checksum from the prior run.
	gw.Status.LastPostRestartJobChecksum = jobChecksum
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected no Job recreated after TTL GC for an already-run revision, got %d", len(jobs.Items))
	}
}

func TestReconcilePostRestartJob_DisabledSpec(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{},
	}
	dep := makeConvergedDeployment(gw, "abc")
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected no jobs when disabled")
	}
}

func TestReconcilePostRestartJob_DeploymentNotFound(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	c := fakeClientBuilder().WithObjects(gw).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc"); err != nil {
		t.Fatalf("unexpected error when deployment missing: %v", err)
	}
	got := &batchv1.Job{}
	err := c.Get(context.Background(), types.NamespacedName{
		Name: resources.PostRestartJobName(gw, "abc"), Namespace: "ns",
	}, got)
	if err == nil {
		t.Fatalf("expected no job without a Deployment")
	}
}
