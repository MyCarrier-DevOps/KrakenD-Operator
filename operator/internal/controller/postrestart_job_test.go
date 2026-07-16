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
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
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
			Replicas: ptr.To(int32(1)),
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
	if gw.Status.LastPostRestartJobChecksum != "abc123" {
		t.Fatalf("status checksum not recorded")
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
	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      resources.PostRestartJobName(gw, "abc123"),
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

// makeFailedJob returns a post-restart Job for the given checksum whose
// status reflects a BackoffLimitExceeded failure. The job script itself
// (run by the Job's own backoffLimit) is responsible for any retries; the
// operator only surfaces the terminal Failed status.
func makeFailedJob(gw *v1alpha1.KrakenDGateway, checksum string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.PostRestartJobName(gw, checksum),
			Namespace: gw.Namespace,
			Annotations: map[string]string{
				resources.PostRestartJobChecksumAnnotation: checksum,
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "BackoffLimitExceeded",
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
	}
}

// makeSucceededJob returns a post-restart Job for the given checksum whose
// status reflects a successful completion.
func makeSucceededJob(gw *v1alpha1.KrakenDGateway, checksum string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.PostRestartJobName(gw, checksum),
			Namespace: gw.Namespace,
			Annotations: map[string]string{
				resources.PostRestartJobChecksumAnnotation: checksum,
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
}

// drainEvents reads all currently-buffered events off a FakeRecorder without
// blocking.
func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-rec.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// countMatching counts how many events contain all of the given substrings.
func countMatching(events []string, substrs ...string) int {
	count := 0
	for _, e := range events {
		matched := true
		for _, s := range substrs {
			if !strings.Contains(e, s) {
				matched = false
				break
			}
		}
		if matched {
			count++
		}
	}
	return count
}

func TestReconcilePostRestartJob_FailedJobSetsConditionFalseAndEmitsWarningOnce(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")
	failed := makeFailedJob(gw, "abc123")
	c := fakeClientBuilder().WithObjects(gw, dep, failed).Build()
	rec := fakeRecorder()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: rec}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error (1st reconcile): %v", err)
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected the failed job to remain untouched (no retry/delete/recreate), got %d jobs",
			len(jobs.Items))
	}

	cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionPostRestartJobSucceeded)
	if cond == nil {
		t.Fatalf("expected %s condition to be set", v1alpha1.ConditionPostRestartJobSucceeded)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected %s condition to be False, got %s", v1alpha1.ConditionPostRestartJobSucceeded, cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonPostRestartJobFailed {
		t.Fatalf("expected condition reason %s, got %s", v1alpha1.ReasonPostRestartJobFailed, cond.Reason)
	}

	firstWarnings := countMatching(drainEvents(rec), "Warning", v1alpha1.ReasonPostRestartJobFailed)
	if firstWarnings != 1 {
		t.Fatalf("expected exactly 1 Warning event on first reconcile, got %d", firstWarnings)
	}

	// Second reconcile of the same gateway/checksum/Job state: the failure
	// was already surfaced, so no further Warning event should be emitted.
	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error (2nd reconcile): %v", err)
	}
	secondWarnings := countMatching(drainEvents(rec), "Warning", v1alpha1.ReasonPostRestartJobFailed)
	if secondWarnings != 0 {
		t.Fatalf("expected no Warning event on repeated reconcile of an unresolved failure, got %d", secondWarnings)
	}
}

func TestReconcilePostRestartJob_SucceededJobUntouched(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")
	succeeded := makeSucceededJob(gw, "abc123")
	c := fakeClientBuilder().WithObjects(gw, dep, succeeded).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected the succeeded job to remain untouched, got %d jobs", len(jobs.Items))
	}
	if jobs.Items[0].Status.Succeeded != 1 {
		t.Fatalf("expected the existing succeeded job's status to be preserved (not recreated)")
	}
}

func TestReconcilePostRestartJob_SucceededSetsConditionTrue(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")
	succeeded := makeSucceededJob(gw, "abc123")
	c := fakeClientBuilder().WithObjects(gw, dep, succeeded).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gw.Status.LastPostRestartJobChecksum != "abc123" {
		t.Fatalf("expected LastPostRestartJobChecksum to be recorded")
	}
	cond := meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionPostRestartJobSucceeded)
	if cond == nil {
		t.Fatalf("expected %s condition to be set", v1alpha1.ConditionPostRestartJobSucceeded)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected %s condition to be True, got %s", v1alpha1.ConditionPostRestartJobSucceeded, cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonPostRestartJobSucceeded {
		t.Fatalf("expected condition reason %s, got %s", v1alpha1.ReasonPostRestartJobSucceeded, cond.Reason)
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
