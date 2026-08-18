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
	"fmt"
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	"github.com/mycarrier-devops/krakend-operator/internal/resources"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

	// review id 3805157450 (#4): the PostRestartJobSkipped condition must
	// reflect the last decision (False here — a Job WAS created), not only
	// ever be set to True.
	found := false
	for _, cond := range gw.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPostRestartJobSkipped {
			found = true
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("expected PostRestartJobSkipped=False after a fresh create, got %v", cond.Status)
			}
			if cond.Reason != v1alpha1.ReasonPostRestartJobCreated {
				t.Errorf("expected reason %q, got %q", v1alpha1.ReasonPostRestartJobCreated, cond.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("expected a PostRestartJobSkipped condition to be set on create")
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

	found := false
	for _, c := range gw.Status.Conditions {
		if c.Type == v1alpha1.ConditionPostRestartJobSkipped {
			found = true
			if c.Status != metav1.ConditionTrue {
				t.Errorf("expected PostRestartJobSkipped condition status True, got %v", c.Status)
			}
			if c.Reason != v1alpha1.ReasonPostRestartJobAlreadyRun {
				t.Errorf("expected reason %q, got %q", v1alpha1.ReasonPostRestartJobAlreadyRun, c.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("expected a PostRestartJobSkipped status condition explaining the no-op, got %+v",
			gw.Status.Conditions)
	}
}

// TestReconcilePostRestartJob_StaleOldFormatStatusTriggersRerun covers
// review id 3804144463 (#10): the actual day-one upgrade state is a
// non-empty, non-matching gw.Status.LastPostRestartJobChecksum — a bare
// 64-char config checksum written by <= v0.13.3, before this PR's combined
// (config + postRestartJob-spec projection) checksum format existed. On the
// first reconcile after upgrading, that stale value can never match the new
// combined checksum, so a Job MUST be created and the status rewritten to
// the new combined value — this is the mass-rerun-at-rollout behavior
// documented in docs/upgrade-guide.md, turned into an executable fact so a
// future "harden the guard" change that starts treating any non-empty
// status as already-satisfied fails loudly here.
func TestReconcilePostRestartJob_StaleOldFormatStatusTriggersRerun(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	dep := makeConvergedDeployment(gw, "abc123")

	// A bare 64-hex-char SHA-256 config checksum, exactly the pre-v0.13.4
	// format (no postRestartJob-spec projection folded in).
	staleStatus := strings.Repeat("a", 64)
	gw.Status.LastPostRestartJobChecksum = staleStatus

	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing expected checksum: %v", err)
	}
	if wantChecksum == staleStatus {
		t.Fatalf("test setup invalid: stale status must not equal the new combined checksum")
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected a Job to be created for the stale pre-upgrade status, got %d", len(jobs.Items))
	}
	if gw.Status.LastPostRestartJobChecksum != wantChecksum {
		t.Fatalf("expected status rewritten to the new combined checksum: got %q want %q",
			gw.Status.LastPostRestartJobChecksum, wantChecksum)
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

// TestReconcilePostRestartJob_DisabledSpecClearsStaleConditions covers
// review id 3807285652 (#7): disabling postRestartJob (or never having it
// configured) must clear any PostRestartJobSkipped / ROFS conditions a
// PRIOR reconcile left behind while it WAS enabled — otherwise `kubectl
// describe krakendgateway` would show a stale condition forever after the
// user turns the feature off.
func TestReconcilePostRestartJob_DisabledSpecClearsStaleConditions(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec:       v1alpha1.KrakenDGatewaySpec{},
	}
	// Simulate leftover conditions from a prior reconcile while
	// postRestartJob was enabled.
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPostRestartJobSkipped,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonPostRestartJobAlreadyRun,
		Message:            "stale from a prior enabled revision",
	})
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPostRestartJobReadOnlyRootFilesystem,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gw.Generation,
		Reason:             v1alpha1.ReasonPostRestartJobROFSEnabled,
		Message:            "stale from a prior enabled revision",
	})

	dep := makeConvergedDeployment(gw, "abc")
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionPostRestartJobSkipped) != nil {
		t.Error("expected PostRestartJobSkipped condition cleared when postRestartJob is disabled")
	}
	if meta.FindStatusCondition(gw.Status.Conditions, v1alpha1.ConditionPostRestartJobReadOnlyRootFilesystem) != nil {
		t.Error("expected ROFS condition cleared when postRestartJob is disabled")
	}
}

// setJobCondition appends a batchv1.JobCondition with Status: True to the
// given Job, used by the tests below to simulate an observed outcome
// (Complete/Failed) on an existing Job object.
func setJobCondition(job *batchv1.Job, condType batchv1.JobConditionType) {
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type:   condType,
		Status: corev1.ConditionTrue,
	})
}

// TestReconcileExistingPostRestartRevision_SucceededKnobChangeSkips covers
// review id 3805157426 (#2)'s no-over-trigger property: a SUCCEEDED Job for
// the current revision must NOT be re-created just because an operator
// subsequently edits a checksum-excluded knob (backoffLimit here) — that is
// exactly the over-trigger the projection-hash design was built to avoid.
func TestReconcileExistingPostRestartRevision_SucceededKnobChangeSkips(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	jobName := resources.PostRestartJobName(gw, jobChecksum)

	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
	resources.BuildPostRestartJob(existing, gw, "abc123", jobChecksum)
	setJobCondition(existing, batchv1.JobComplete)
	existing.Status.Succeeded = 1

	gw.Status.LastPostRestartJobChecksum = jobChecksum
	// Operator edits an excluded knob AFTER success — must not change the
	// checksum (BackoffLimit is excluded from postRestartJobProjection).
	gw.Spec.PostRestartJob.BackoffLimit = new(int32(9))
	recheck, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil || recheck != jobChecksum {
		t.Fatalf("test setup invalid: backoffLimit edit must not change the checksum, got %q want %q (err=%v)",
			recheck, jobChecksum, err)
	}

	c := fakeClientBuilder().WithObjects(gw, existing).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected the succeeded job to be left alone (no re-create), got %d jobs", len(jobs.Items))
	}
	if jobs.Items[0].Spec.BackoffLimit == nil || *jobs.Items[0].Spec.BackoffLimit == 9 {
		t.Fatalf("expected the ORIGINAL succeeded job (backoffLimit != 9) to survive untouched, got %+v",
			jobs.Items[0].Spec.BackoffLimit)
	}

	found := false
	for _, cond := range gw.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPostRestartJobSkipped {
			found = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected PostRestartJobSkipped=True, got %v", cond.Status)
			}
		}
	}
	if !found {
		t.Fatalf("expected a PostRestartJobSkipped condition")
	}
}

// TestReconcileExistingPostRestartRevision_FailedSpecUnchangedSkips covers
// review id 3805157426 (#2)'s loop-safety property: a persistently-failing
// Job whose spec is UNCHANGED must NOT be re-created on every reconcile —
// that would be an unbounded, silent retry loop.
func TestReconcileExistingPostRestartRevision_FailedSpecUnchangedSkips(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	jobName := resources.PostRestartJobName(gw, jobChecksum)

	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
	resources.BuildPostRestartJob(existing, gw, "abc123", jobChecksum)
	setJobCondition(existing, batchv1.JobFailed)

	gw.Status.LastPostRestartJobChecksum = jobChecksum

	c := fakeClientBuilder().WithObjects(gw, existing).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	// Simulate several consecutive reconciles: none must re-create the Job.
	for i := 0; i < 3; i++ {
		if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
			t.Fatalf("unexpected error on reconcile %d: %v", i, err)
		}
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly one job (no retry loop), got %d", len(jobs.Items))
	}
	if jobs.Items[0].ResourceVersion != existing.ResourceVersion {
		t.Fatalf("expected the ORIGINAL failed job object to survive untouched (no delete+recreate)")
	}

	for _, cond := range gw.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPostRestartJobSkipped && cond.Status != metav1.ConditionTrue {
			t.Errorf("expected PostRestartJobSkipped=True for an unchanged failed revision, got %v", cond.Status)
		}
	}
}

// TestReconcileExistingPostRestartRevision_FailedSpecChangedRecreates covers
// review id 3805157426 (#2)'s failure-signal re-create: a FAILED Job for the
// current revision IS re-created when the operator edits an execution-
// affecting but checksum-excluded knob (backoffLimit here) — the Job's
// identity checksum (and name) is unchanged, so this is a delete+re-create
// under the SAME name, not a new Job.
func TestReconcileExistingPostRestartRevision_FailedSpecChangedRecreates(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	jobName := resources.PostRestartJobName(gw, jobChecksum)

	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
	resources.BuildPostRestartJob(existing, gw, "abc123", jobChecksum)
	setJobCondition(existing, batchv1.JobFailed)

	gw.Status.LastPostRestartJobChecksum = jobChecksum
	// Operator bumps backoffLimit to retry after diagnosing a transient
	// failure — must not change the checksum.
	gw.Spec.PostRestartJob.BackoffLimit = new(int32(9))
	recheck, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil || recheck != jobChecksum {
		t.Fatalf("test setup invalid: backoffLimit edit must not change the checksum, got %q want %q (err=%v)",
			recheck, jobChecksum, err)
	}

	c := fakeClientBuilder().WithStatusSubresource(&v1alpha1.KrakenDGateway{}).WithObjects(gw, existing).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly one job (re-created under the same name), got %d", len(jobs.Items))
	}
	if jobs.Items[0].Name != jobName {
		t.Fatalf("expected the re-created job to keep the same name %q, got %q", jobName, jobs.Items[0].Name)
	}
	if jobs.Items[0].Spec.BackoffLimit == nil || *jobs.Items[0].Spec.BackoffLimit != 9 {
		t.Fatalf("expected the re-created job to carry the updated backoffLimit 9, got %+v",
			jobs.Items[0].Spec.BackoffLimit)
	}
	if len(jobs.Items[0].Status.Conditions) != 0 {
		t.Fatalf("expected the re-created job to be a fresh object with no stale Failed condition, got %+v",
			jobs.Items[0].Status.Conditions)
	}

	found := false
	for _, cond := range gw.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPostRestartJobSkipped {
			found = true
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("expected PostRestartJobSkipped=False after a re-create, got %v", cond.Status)
			}
			if cond.Reason != v1alpha1.ReasonPostRestartJobCreated {
				t.Errorf("expected reason %q, got %q", v1alpha1.ReasonPostRestartJobCreated, cond.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("expected a PostRestartJobSkipped condition")
	}
}

// TestReconcilePostRestartJob_ROFSConditionSetOnCreate covers review id
// 3805157497 (#9): the ROFS informational condition must be set
// unconditionally on Job creation, even when workingDir is left unset (the
// prod case the admission-time warning misses, since that warning only
// fires when workingDir is overridden outside /tmp).
func TestReconcilePostRestartJob_ROFSConditionSetOnCreate(t *testing.T) {
	gw := makeGWWithJob("npm install -g rdme") // workingDir unset (default /tmp)
	dep := makeConvergedDeployment(gw, "abc123")
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, cond := range gw.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPostRestartJobReadOnlyRootFilesystem {
			found = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected ROFS condition True (hardened default), got %v", cond.Status)
			}
			if !strings.Contains(cond.Message, "/tmp") {
				t.Errorf("expected ROFS message to mention /tmp, got: %q", cond.Message)
			}
		}
	}
	if !found {
		t.Fatalf("expected the ROFS condition to be set unconditionally on Job creation, even with " +
			"workingDir unset")
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

// TestReconcilePostRestartJob_ROFSConditionFalseWhenDisabled covers review
// id 3807285633 (#3c): the ConditionFalse branch — an explicit
// readOnlyRootFilesystem: false override (the documented prod escape hatch
// for scripts like `npm install -g`) must report the ROFS condition as
// False with a message explaining the whole filesystem is writable, not
// silently fall back to the True (hardened) message.
func TestReconcilePostRestartJob_ROFSConditionFalseWhenDisabled(t *testing.T) {
	gw := makeGWWithJob("npm install -g rdme")
	gw.Spec.PostRestartJob.SecurityContext = &corev1.SecurityContext{
		ReadOnlyRootFilesystem: new(false),
	}
	dep := makeConvergedDeployment(gw, "abc123")
	c := fakeClientBuilder().WithObjects(gw, dep).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, cond := range gw.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPostRestartJobReadOnlyRootFilesystem {
			found = true
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("expected ROFS condition False (explicit opt-out), got %v", cond.Status)
			}
			if cond.Reason != v1alpha1.ReasonPostRestartJobROFSDisabled {
				t.Errorf("expected reason %q, got %q", v1alpha1.ReasonPostRestartJobROFSDisabled, cond.Reason)
			}
			if !strings.Contains(cond.Message, "writable") {
				t.Errorf("expected message to mention the filesystem is writable, got: %q", cond.Message)
			}
		}
	}
	if !found {
		t.Fatalf("expected the ROFS condition to be set on create even when disabled")
	}
}

// TestReconcileExistingPostRestartRevision_StillRunningSkipsNoMutation
// covers review id 3807285657 (#8): the running-Job skip branch
// (postRestartJobFailed == false, postRestartJobSucceeded == false — i.e.
// no terminal status.conditions at all) had no dedicated test. Mirrors
// TestReconcileExistingPostRestartRevision_FailedSpecUnchangedSkips: the
// Job must survive untouched (no delete/recreate), verified via
// ResourceVersion.
func TestReconcileExistingPostRestartRevision_StillRunningSkipsNoMutation(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	jobName := resources.PostRestartJobName(gw, jobChecksum)

	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
	resources.BuildPostRestartJob(existing, gw, "abc123", jobChecksum)
	// No status.conditions at all — the Job is still running (neither
	// Complete nor Failed observed yet).

	gw.Status.LastPostRestartJobChecksum = jobChecksum
	c := fakeClientBuilder().WithObjects(gw, existing).Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly one job (no delete/recreate while running), got %d", len(jobs.Items))
	}
	if jobs.Items[0].ResourceVersion != existing.ResourceVersion {
		t.Fatalf("expected the ORIGINAL running job object to survive untouched (no delete+recreate)")
	}

	found := false
	for _, cond := range gw.Status.Conditions {
		if cond.Type == v1alpha1.ConditionPostRestartJobSkipped {
			found = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected PostRestartJobSkipped=True while still running, got %v", cond.Status)
			}
		}
	}
	if !found {
		t.Fatalf("expected a PostRestartJobSkipped condition")
	}
}

// TestPostRestartJobExecutionKnobsChanged_DefaultOnlyDiffDoesNotTrigger
// covers review id 3807285637 (#4): a knob difference that comes ONLY from
// comparing against an operator DEFAULT — not a user spec change — must
// never be reported as "changed". The user's spec never sets BackoffLimit
// (nil throughout), simulating a Job built under a PAST (or simply
// different) operator default; only the actual EXISTING Job's stored value
// differs from what the CURRENT default would produce.
func TestPostRestartJobExecutionKnobsChanged_DefaultOnlyDiffDoesNotTrigger(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	existing := &batchv1.Job{}
	resources.BuildPostRestartJob(existing, gw, "abc123", "chk")
	// Simulate a Job built under a different backoffLimit default — the
	// user's spec.PostRestartJob.BackoffLimit is (and remains) nil.
	existing.Spec.BackoffLimit = new(int32(99))

	if postRestartJobExecutionKnobsChanged(gw.Spec.PostRestartJob, existing) {
		t.Fatal("expected a default-only difference (user spec knob unset) to NOT trigger a recreate")
	}
}

// TestReconcileExistingPostRestartRevision_CreateFailsAfterDeleteClearsChecksum
// covers review id 3807285616 (#1a): if Create fails for a transient reason
// AFTER a successful Delete during a failed-Job re-create, the persisted
// gw.Status.LastPostRestartJobChecksum must be cleared (flushed via
// Status().Update BEFORE the error is returned) — otherwise the next
// reconcile would find the Job gone but status still claiming this
// revision already ran, and skip forever (the TTL-GC branch), stranding
// the gateway with no post-restart Job ever running again for this
// revision.
func TestReconcileExistingPostRestartRevision_CreateFailsAfterDeleteClearsChecksum(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	jobName := resources.PostRestartJobName(gw, jobChecksum)

	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
	resources.BuildPostRestartJob(existing, gw, "abc123", jobChecksum)
	setJobCondition(existing, batchv1.JobFailed)

	gw.Status.LastPostRestartJobChecksum = jobChecksum
	gw.Spec.PostRestartJob.BackoffLimit = new(int32(9)) // forces the recreate path

	createCalls := 0
	failingCreate := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				createCalls++
				if createCalls == 1 {
					return fmt.Errorf("simulated transient API error")
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	c := fakeClientBuilder().
		WithStatusSubresource(&v1alpha1.KrakenDGateway{}).
		WithObjects(gw, existing).
		WithInterceptorFuncs(failingCreate).
		Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err == nil {
		t.Fatal("expected the simulated transient Create error to propagate")
	}

	// The Delete succeeded before the failed Create, so no Job survives.
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected the failed Job to be gone (Delete succeeded before Create failed), got %d", len(jobs.Items))
	}

	// In-memory: cleared immediately (before the error return).
	if gw.Status.LastPostRestartJobChecksum != "" {
		t.Fatalf("expected in-memory checksum cleared after a failed re-create, got %q",
			gw.Status.LastPostRestartJobChecksum)
	}

	// Persisted: fetch a FRESH copy to prove the clear was flushed via
	// Status().Update, not just mutated on the caller's in-memory pointer.
	var persisted v1alpha1.KrakenDGateway
	if err := c.Get(context.Background(), types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &persisted); err != nil {
		t.Fatalf("getting persisted gateway: %v", err)
	}
	if persisted.Status.LastPostRestartJobChecksum != "" {
		t.Fatalf("expected the PERSISTED checksum cleared before the Create error was returned, got %q",
			persisted.Status.LastPostRestartJobChecksum)
	}

	// Recovery: the next reconcile (Create now succeeds) must take the
	// top-level create path (checksum no longer matches) and actually
	// create a fresh Job — proving the gateway is not stranded.
	dep := makeConvergedDeployment(gw, "abc123")
	if err := c.Create(context.Background(), dep); err != nil {
		t.Fatalf("creating converged deployment for recovery reconcile: %v", err)
	}
	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("unexpected error on recovery reconcile: %v", err)
	}
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatalf("listing jobs after recovery: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected the recovery reconcile to create exactly one Job, got %d", len(jobs.Items))
	}
	if gw.Status.LastPostRestartJobChecksum != jobChecksum {
		t.Fatalf("expected the checksum restored after the recovery create: got %q want %q",
			gw.Status.LastPostRestartJobChecksum, jobChecksum)
	}
}

// TestReconcileExistingPostRestartRevision_RecreateToleratesAlreadyExists
// covers review id 3807285616 (#1c): tolerating AlreadyExists on the
// recreate Create closes the background-delete propagation race
// (DeletePropagationBackground returns as soon as the delete is accepted,
// not once the Job object is actually gone), symmetric with the original
// create path's existing AlreadyExists handling.
func TestReconcileExistingPostRestartRevision_RecreateToleratesAlreadyExists(t *testing.T) {
	gw := makeGWWithJob("echo ok")
	jobChecksum, err := resources.PostRestartJobChecksum(gw.Spec.PostRestartJob, "abc123")
	if err != nil {
		t.Fatalf("computing job checksum: %v", err)
	}
	jobName := resources.PostRestartJobName(gw, jobChecksum)

	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: gw.Namespace}}
	resources.BuildPostRestartJob(existing, gw, "abc123", jobChecksum)
	setJobCondition(existing, batchv1.JobFailed)

	gw.Status.LastPostRestartJobChecksum = jobChecksum
	gw.Spec.PostRestartJob.BackoffLimit = new(int32(9)) // forces the recreate path

	alreadyExistsCreate := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, obj.GetName())
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	c := fakeClientBuilder().
		WithStatusSubresource(&v1alpha1.KrakenDGateway{}).
		WithObjects(gw, existing).
		WithInterceptorFuncs(alreadyExistsCreate).
		Build()
	r := &KrakenDGatewayReconciler{Client: c, Scheme: testScheme(), Recorder: fakeRecorder()}

	if err := r.reconcilePostRestartJob(context.Background(), gw, "abc123"); err != nil {
		t.Fatalf("expected AlreadyExists on re-create to be tolerated, got error: %v", err)
	}

	// Symmetric with the create path (~L941-945): AlreadyExists is treated
	// as success without setting status/conditions here — a subsequent
	// reconcile observes the object and reconciles status then.
	if gw.Status.LastPostRestartJobChecksum != "" {
		t.Fatalf("expected the checksum to remain cleared (not re-set) after tolerating AlreadyExists, got %q",
			gw.Status.LastPostRestartJobChecksum)
	}
}
