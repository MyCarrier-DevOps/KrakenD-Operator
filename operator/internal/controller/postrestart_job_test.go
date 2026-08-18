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
