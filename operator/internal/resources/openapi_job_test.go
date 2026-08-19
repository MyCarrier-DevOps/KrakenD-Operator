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

package resources

import (
	"reflect"
	"strings"
	"testing"

	v1alpha1 "github.com/mycarrier-devops/krakend-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPostRestartJobName_StableAndShort(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: "gw"}}
	n1 := PostRestartJobName(gw, "abcdef0123456789deadbeef")
	n2 := PostRestartJobName(gw, "fedcba0123456789cafebabe")
	if n1 == n2 {
		t.Fatalf("expected distinct names for different checksums")
	}
	if !strings.HasPrefix(n1, "gw-postrestart-abcdef012345") {
		t.Fatalf("unexpected job name: %s", n1)
	}
}

func TestPostRestartJobName_LongGatewayName(t *testing.T) {
	longName := strings.Repeat("a", 80)
	gw := &v1alpha1.KrakenDGateway{ObjectMeta: metav1.ObjectMeta{Name: longName}}
	name := PostRestartJobName(gw, "abcdef0123456789deadbeef")
	if len(name) > 63 {
		t.Fatalf("job name exceeds 63 chars: len=%d, name=%s", len(name), name)
	}
	if len(name) != 63 {
		t.Fatalf("expected exactly 63 chars for truncated name, got %d", len(name))
	}
	// Must still embed the checksum for uniqueness.
	if !strings.HasSuffix(name, "-abcdef012345") {
		t.Fatalf("truncated name lost checksum suffix: %s", name)
	}
}

func TestBuildPostRestartJob_Defaults(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo hello",
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "checksum1", "combined1")

	if job.Annotations[PostRestartJobChecksumAnnotation] != "checksum1" {
		t.Fatalf("config checksum annotation missing")
	}
	if job.Annotations[PostRestartJobCombinedChecksumAnnotation] != "combined1" {
		t.Fatalf("combined checksum annotation missing")
	}
	if *job.Spec.BackoffLimit != 2 {
		t.Fatalf("default backoffLimit not applied: %d", *job.Spec.BackoffLimit)
	}
	if *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("default activeDeadline not applied")
	}
	if *job.Spec.TTLSecondsAfterFinished != 86400 {
		t.Fatalf("default TTL not applied")
	}
	if job.Spec.Template.Spec.ServiceAccountName != "gw" {
		t.Fatalf("expected default SA to match gateway name, got %q",
			job.Spec.Template.Spec.ServiceAccountName)
	}
	if job.Spec.Template.Spec.Containers[0].Image != DefaultPostRestartJobImage {
		t.Fatalf("expected default image %q", DefaultPostRestartJobImage)
	}
	cmd := job.Spec.Template.Spec.Containers[0].Command
	if len(cmd) < 3 || cmd[len(cmd)-1] != "echo hello" {
		t.Fatalf("script not passed to bash: %v", cmd)
	}
	// Default command should be PATH-resolved "bash", not "/bin/bash".
	if cmd[0] != "bash" || cmd[1] != "-c" {
		t.Fatalf("expected default command [bash -c ...], got %v", cmd)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Fatalf("restart policy not OnFailure")
	}
	if job.Spec.Template.Annotations[PostRestartJobChecksumAnnotation] != "checksum1" {
		t.Fatalf("pod config checksum annotation missing")
	}
	if job.Spec.Template.Annotations[PostRestartJobCombinedChecksumAnnotation] != "combined1" {
		t.Fatalf("pod combined checksum annotation missing")
	}
	if job.Spec.Template.Spec.Containers[0].WorkingDir != "/tmp" {
		t.Fatalf("expected default WorkingDir /tmp, got %q",
			job.Spec.Template.Spec.Containers[0].WorkingDir)
	}
}

func TestBuildPostRestartJob_CustomFields(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:                 true,
				Script:                  "do-work",
				Image:                   "custom:1",
				ServiceAccountName:      "custom-sa",
				Env:                     []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
				PodAnnotations:          map[string]string{"team": "payments"},
				BackoffLimit:            new(int32(5)),
				ActiveDeadlineSeconds:   new(int64(120)),
				TTLSecondsAfterFinished: new(int32(60)),
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "custom:1" {
		t.Fatalf("custom image not applied")
	}
	if job.Spec.Template.Spec.ServiceAccountName != "custom-sa" {
		t.Fatalf("custom SA not applied")
	}
	if len(c.Env) != 1 || c.Env[0].Name != "FOO" {
		t.Fatalf("env not applied")
	}
	if job.Spec.Template.Annotations["team"] != "payments" {
		t.Fatalf("custom pod annotation missing")
	}
	if *job.Spec.BackoffLimit != 5 || *job.Spec.ActiveDeadlineSeconds != 120 ||
		*job.Spec.TTLSecondsAfterFinished != 60 {
		t.Fatalf("custom scheduling fields not applied")
	}
	if job.Spec.Template.Spec.Containers[0].WorkingDir != "/tmp" {
		t.Fatalf("expected forced WorkingDir /tmp regardless of custom fields, got %q",
			job.Spec.Template.Spec.Containers[0].WorkingDir)
	}
}

func TestBuildPostRestartJob_CustomCommand(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo done",
				Command: []string{"/bin/sh", "-c"},
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	cmd := job.Spec.Template.Spec.Containers[0].Command
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[2] != "echo done" {
		t.Fatalf("custom command not applied: %v", cmd)
	}
}

func TestBuildPostRestartJob_CustomSecurityContext(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: new(false),
					RunAsUser:    new(int64(0)),
				},
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	csc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if csc.RunAsUser == nil || *csc.RunAsUser != 0 {
		t.Fatalf("custom container security context not applied: %+v", csc)
	}
	psc := job.Spec.Template.Spec.SecurityContext
	if psc.RunAsNonRoot == nil || *psc.RunAsNonRoot != false {
		t.Fatalf("custom pod security context not applied: %+v", psc)
	}
	if psc.RunAsUser == nil || *psc.RunAsUser != 0 {
		t.Fatalf("pod runAsUser not applied: %+v", psc)
	}
}

// TestBuildPostRestartJob_SecurityContextMergesNotReplaces covers 8qln: a
// user overriding only runAsUser (e.g. prod's uid0 for `npm install -g`)
// must NOT discard the hardened defaults for fields they did not set.
func TestBuildPostRestartJob_SecurityContextMergesNotReplaces(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "npm install -g rdme",
				SecurityContext: &corev1.SecurityContext{
					RunAsUser: new(int64(0)),
				},
				PodSecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: new(false),
					RunAsUser:    new(int64(0)),
				},
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	csc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if csc.RunAsUser == nil || *csc.RunAsUser != 0 {
		t.Fatalf("user-set runAsUser:0 not applied: %+v", csc)
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("hardened drop:ALL discarded by partial user override: %+v", csc)
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Fatalf("hardened readOnlyRootFilesystem discarded by partial user override: %+v", csc)
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Fatalf("hardened allowPrivilegeEscalation:false discarded by partial user override: %+v", csc)
	}

	psc := job.Spec.Template.Spec.SecurityContext
	if psc.RunAsUser == nil || *psc.RunAsUser != 0 {
		t.Fatalf("user-set pod runAsUser:0 not applied: %+v", psc)
	}
	if psc.RunAsNonRoot == nil || *psc.RunAsNonRoot {
		t.Fatalf("user-set pod runAsNonRoot:false not applied: %+v", psc)
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("hardened seccompProfile discarded by partial user override: %+v", psc)
	}
	if psc.RunAsGroup == nil || *psc.RunAsGroup != 1000 {
		t.Fatalf("hardened runAsGroup:1000 discarded by partial user override: %+v", psc)
	}
}

// TestBuildPostRestartJob_SecurityContextNilUsesHardenedDefaults covers the
// nil-spec path for both container and pod securityContext (no user
// override at all): must equal the same hardened defaults as before.
func TestBuildPostRestartJob_SecurityContextNilUsesHardenedDefaults(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo ok",
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	csc := job.Spec.Template.Spec.Containers[0].SecurityContext
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Fatalf("expected default allowPrivilegeEscalation:false, got %+v", csc)
	}
	psc := job.Spec.Template.Spec.SecurityContext
	if psc.RunAsUser == nil || *psc.RunAsUser != 1000 {
		t.Fatalf("expected default pod runAsUser:1000, got %+v", psc)
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Fatalf("expected default pod runAsNonRoot:true, got %+v", psc)
	}
}

// TestBuildPostRestartJob_HardenedContainerDefaults covers ifc4: the
// container must default to readOnlyRootFilesystem + drop:ALL, with a /tmp
// emptyDir (bounded sizeLimit) mounted so WorkingDir stays writable.
func TestBuildPostRestartJob_HardenedContainerDefaults(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "curl -o openapi.json http://example/openapi.json",
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	c := job.Spec.Template.Spec.Containers[0]
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatalf("expected readOnlyRootFilesystem:true by default, got %+v", c.SecurityContext)
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("expected capabilities.drop:[ALL] by default, got %+v", c.SecurityContext.Capabilities)
	}

	var tmpMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "tmp" {
			tmpMount = &c.VolumeMounts[i]
		}
	}
	if tmpMount == nil {
		t.Fatalf("expected a tmp volume mount, got %+v", c.VolumeMounts)
	}
	if tmpMount.MountPath != "/tmp" {
		t.Fatalf("expected tmp mount path /tmp, got %q", tmpMount.MountPath)
	}

	var tmpVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "tmp" {
			tmpVol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if tmpVol == nil {
		t.Fatalf("expected a tmp emptyDir volume, got %+v", job.Spec.Template.Spec.Volumes)
	}
	if tmpVol.EmptyDir == nil {
		t.Fatalf("expected tmp volume to be an emptyDir, got %+v", tmpVol)
	}
	if tmpVol.EmptyDir.SizeLimit == nil || tmpVol.EmptyDir.SizeLimit.String() != "256Mi" {
		t.Fatalf("expected tmp emptyDir default sizeLimit 256Mi, got %+v", tmpVol.EmptyDir.SizeLimit)
	}
}

// TestBuildPostRestartJob_TmpSizeLimitOverride covers review id 3805157467
// (#6): spec.postRestartJob.tmpSizeLimit must override the hardcoded 256Mi
// default on the built /tmp emptyDir volume.
func TestBuildPostRestartJob_TmpSizeLimitOverride(t *testing.T) {
	qty := resource.MustParse("2Gi")
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:      true,
				Script:       "echo ok",
				TmpSizeLimit: &qty,
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	var tmpVol *corev1.Volume
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == "tmp" {
			tmpVol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if tmpVol == nil || tmpVol.EmptyDir == nil {
		t.Fatalf("expected a tmp emptyDir volume, got %+v", job.Spec.Template.Spec.Volumes)
	}
	if tmpVol.EmptyDir.SizeLimit == nil || tmpVol.EmptyDir.SizeLimit.String() != "2Gi" {
		t.Fatalf("expected overridden tmp emptyDir sizeLimit 2Gi, got %+v", tmpVol.EmptyDir.SizeLimit)
	}
}

// TestMergeContainerSecurityContext_CapabilitiesAddOnlyPreservesDropAll
// covers review id 3804144405 (#2): a user who sets only
// capabilities.add must NOT lose the hardened capabilities.drop:[ALL]
// baseline, even though Capabilities.Add/Drop are plain slices (no
// patchMergeKey) and strategic-merge-patch therefore treats each of them,
// individually, as replace-if-present. Because the user's JSON omits `drop`
// entirely (omitempty + nil slice), the patch does not touch it and the
// base's `drop` survives — verified here directly.
func TestMergeContainerSecurityContext_CapabilitiesAddOnlyPreservesDropAll(t *testing.T) {
	user := &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"NET_BIND_SERVICE"},
		},
	}
	merged := mergeContainerSecurityContext(user)

	if merged.Capabilities == nil {
		t.Fatalf("expected non-nil capabilities")
	}
	if len(merged.Capabilities.Add) != 1 || merged.Capabilities.Add[0] != "NET_BIND_SERVICE" {
		t.Fatalf("user-set capabilities.add not applied: %+v", merged.Capabilities.Add)
	}
	if len(merged.Capabilities.Drop) != 1 || merged.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("hardened capabilities.drop:[ALL] baseline lost by add-only override: %+v",
			merged.Capabilities.Drop)
	}
}

// TestMergeContainerSecurityContext_CapabilitiesDropOverride verifies the
// symmetric case: a user who explicitly sets drop (to something other than
// ALL) does override the baseline, since that field IS present in the
// user's patch JSON.
func TestMergeContainerSecurityContext_CapabilitiesDropOverride(t *testing.T) {
	user := &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"NET_RAW"},
		},
	}
	merged := mergeContainerSecurityContext(user)

	if len(merged.Capabilities.Drop) != 1 || merged.Capabilities.Drop[0] != "NET_RAW" {
		t.Fatalf("user-set capabilities.drop not applied: %+v", merged.Capabilities.Drop)
	}
}

// TestMergePodSecurityContext_RootUserDropsInheritedRunAsNonRoot covers
// review id 3804144382 (#1, important/security): setting only
// podSecurityContext.runAsUser: 0 (no explicit runAsNonRoot) must not leave
// the contradictory pair {runAsUser:0, runAsNonRoot:true} — the kubelet
// rejects that combination at container start
// (CreateContainerConfigError), and because the pod never reaches phase
// Failed, backoffLimit is never consumed: the Job simply hangs Pending
// until activeDeadlineSeconds. strategicpatch cannot catch this on its own
// since RunAsUser and RunAsNonRoot are independent fields; it is handled as
// an explicit post-merge fixup in mergePodSecurityContext.
func TestMergePodSecurityContext_RootUserDropsInheritedRunAsNonRoot(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser: new(int64(0)),
	}
	merged := mergePodSecurityContext(user)

	if merged.RunAsUser == nil || *merged.RunAsUser != 0 {
		t.Fatalf("user-set runAsUser:0 not applied: %+v", merged.RunAsUser)
	}
	if merged.RunAsNonRoot != nil {
		t.Fatalf("expected inherited runAsNonRoot default to be dropped when "+
			"runAsUser:0 is set without an explicit runAsNonRoot, got %+v", merged.RunAsNonRoot)
	}
}

// TestMergePodSecurityContext_RootUserWithExplicitRunAsNonRootHonored
// verifies the escape hatch this fixup relies on: a user who sets BOTH
// runAsUser:0 AND runAsNonRoot explicitly (prod's actual manifest, per the
// review comment) keeps their explicit value untouched.
func TestMergePodSecurityContext_RootUserWithExplicitRunAsNonRootHonored(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser:    new(int64(0)),
		RunAsNonRoot: new(false),
	}
	merged := mergePodSecurityContext(user)

	if merged.RunAsNonRoot == nil || *merged.RunAsNonRoot != false {
		t.Fatalf("explicit user runAsNonRoot:false not honored: %+v", merged.RunAsNonRoot)
	}
}

// fillNonZero recursively sets every settable field reachable from v to a
// non-zero value: pointers are allocated, structs are recursed into, slices
// get a single non-zero element, bools become true, strings (including
// named string kinds like Capability/ProcMountType/SeccompProfileType)
// become a fixed sentinel, and integers become a fixed non-zero constant.
// Used by the reflective round-trip merge tests (review id 3804144473,
// #11) so the test automatically covers any field added to
// SecurityContext/PodSecurityContext in a future k8s API bump without the
// test itself needing to be updated.
func fillNonZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillNonZero(v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if f.CanSet() {
				fillNonZero(f)
			}
		}
	case reflect.Slice:
		if v.Len() == 0 {
			elem := reflect.New(v.Type().Elem()).Elem()
			fillNonZero(elem)
			v.Set(reflect.Append(v, elem))
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.String:
		v.SetString("nonzero-sentinel")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	}
}

// TestMergeContainerSecurityContext_ReflectiveRoundTrip covers review id
// 3804144473 (#11): every field of a fully-populated user SecurityContext
// must survive the merge unchanged (the user explicitly set every field, so
// every field must win over the hardened default). Iterating fields via
// reflection means this test keeps covering the FULL field set even after a
// future k8s API bump adds new fields, unlike a hand-picked list of
// assertions.
func TestMergeContainerSecurityContext_ReflectiveRoundTrip(t *testing.T) {
	user := &corev1.SecurityContext{}
	fillNonZero(reflect.ValueOf(user).Elem())

	merged := mergeContainerSecurityContext(user)

	uv := reflect.ValueOf(user).Elem()
	mv := reflect.ValueOf(merged).Elem()
	ut := uv.Type()
	for i := 0; i < uv.NumField(); i++ {
		name := ut.Field(i).Name
		if !reflect.DeepEqual(uv.Field(i).Interface(), mv.Field(i).Interface()) {
			t.Errorf("field %s did not survive the merge: user=%+v merged=%+v",
				name, uv.Field(i).Interface(), mv.Field(i).Interface())
		}
	}
}

// TestMergePodSecurityContext_ReflectiveRoundTrip is the PodSecurityContext
// counterpart of TestMergeContainerSecurityContext_ReflectiveRoundTrip (#11).
// RunAsUser is deliberately filled with a non-zero value (7, via
// fillNonZero) so the #1 root-uid fixup in mergePodSecurityContext does not
// interfere with this test's "every field survives" assertion.
func TestMergePodSecurityContext_ReflectiveRoundTrip(t *testing.T) {
	user := &corev1.PodSecurityContext{}
	fillNonZero(reflect.ValueOf(user).Elem())

	merged := mergePodSecurityContext(user)

	uv := reflect.ValueOf(user).Elem()
	mv := reflect.ValueOf(merged).Elem()
	ut := uv.Type()
	for i := 0; i < uv.NumField(); i++ {
		name := ut.Field(i).Name
		if !reflect.DeepEqual(uv.Field(i).Interface(), mv.Field(i).Interface()) {
			t.Errorf("field %s did not survive the merge: user=%+v merged=%+v",
				name, uv.Field(i).Interface(), mv.Field(i).Interface())
		}
	}
}

// TestBuildPostRestartJob_WorkingDirDefault covers aul3: unset workingDir
// keeps the previous forced "/tmp" behavior (backward compatible).
func TestBuildPostRestartJob_WorkingDirDefault(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled: true,
				Script:  "echo ok",
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	if got := job.Spec.Template.Spec.Containers[0].WorkingDir; got != "/tmp" {
		t.Fatalf("expected default workingDir /tmp, got %q", got)
	}
}

// TestBuildPostRestartJob_WorkingDirOverride covers aul3: an explicit
// workingDir override is honored instead of the forced "/tmp" default.
func TestBuildPostRestartJob_WorkingDirOverride(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			PostRestartJob: &v1alpha1.PostRestartJobSpec{
				Enabled:    true,
				Script:     "echo ok",
				WorkingDir: "/custom-dir",
			},
		},
	}
	job := &batchv1.Job{}
	BuildPostRestartJob(job, gw, "cs", "job-cs")

	if got := job.Spec.Template.Spec.Containers[0].WorkingDir; got != "/custom-dir" {
		t.Fatalf("expected overridden workingDir /custom-dir, got %q", got)
	}
}

// TestPostRestartJobChecksum_ChangesWithSpec covers nhig root cause 2: the
// Job identity checksum must change when the postRestartJob spec changes,
// even when the rendered krakend.json configChecksum stays the same —
// previously spec-only edits (script, securityContext, workingDir) were
// invisible to Job naming/re-trigger logic.
func TestPostRestartJobChecksum_ChangesWithSpec(t *testing.T) {
	base := &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo v1"}
	edited := &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo v2"}

	baseSum, err := PostRestartJobChecksum(base, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing base checksum: %v", err)
	}
	editedSum, err := PostRestartJobChecksum(edited, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing edited checksum: %v", err)
	}
	if baseSum == editedSum {
		t.Fatalf("expected checksum to change when postRestartJob spec changes, got %q for both", baseSum)
	}

	// Sanity: an unrelated config checksum change must also still change
	// the combined checksum (existing behavior preserved).
	configChangedSum, err := PostRestartJobChecksum(base, "different-config-checksum")
	if err != nil {
		t.Fatalf("computing config-changed checksum: %v", err)
	}
	if baseSum == configChangedSum {
		t.Fatalf("expected checksum to change when configChecksum changes, got %q for both", baseSum)
	}

	// Determinism: same inputs must produce the same checksum.
	baseSumAgain, err := PostRestartJobChecksum(base, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing base checksum again: %v", err)
	}
	if baseSum != baseSumAgain {
		t.Fatalf("expected deterministic checksum, got %q then %q", baseSum, baseSumAgain)
	}
}

// TestPostRestartJobChecksum_UnsetVsExplicitDefaultWorkingDirConverge covers
// the round-3 robustness finding: BuildPostRestartJob defaults an unset
// WorkingDir to "/tmp" (postRestartWorkingDir), so a CR that leaves
// WorkingDir unset and a CR that explicitly sets it to "/tmp" render a
// byte-identical Job. The checksum must hash the EFFECTIVE (post-default)
// value, not the raw spec field, so both cases converge on one checksum
// instead of over-triggering a spurious re-create.
func TestPostRestartJobChecksum_UnsetVsExplicitDefaultWorkingDirConverge(t *testing.T) {
	unset := &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo ok"}
	explicitDefault := &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo ok", WorkingDir: "/tmp"}

	unsetSum, err := PostRestartJobChecksum(unset, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing unset checksum: %v", err)
	}
	explicitSum, err := PostRestartJobChecksum(explicitDefault, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing explicit-default checksum: %v", err)
	}
	if unsetSum != explicitSum {
		t.Fatalf("expected WorkingDir unset and WorkingDir=/tmp (the default) to produce the same "+
			"checksum, got %q vs %q", unsetSum, explicitSum)
	}

	// Sanity: a genuinely different WorkingDir must still change the checksum.
	different := &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo ok", WorkingDir: "/custom-dir"}
	differentSum, err := PostRestartJobChecksum(different, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing different-workingDir checksum: %v", err)
	}
	if differentSum == unsetSum {
		t.Fatalf("expected a non-default WorkingDir to change the checksum, got %q for both", differentSum)
	}
}

// TestPostRestartJobChecksum_UnsetVsExplicitDefaultCommandConverge mirrors
// TestPostRestartJobChecksum_UnsetVsExplicitDefaultWorkingDirConverge for
// Command: BuildPostRestartJob defaults an unset/empty Command to
// ["bash", "-c"], so a CR that leaves Command unset and a CR that
// explicitly sets it to ["bash", "-c"] must hash to the same checksum.
func TestPostRestartJobChecksum_UnsetVsExplicitDefaultCommandConverge(t *testing.T) {
	unset := &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo ok"}
	explicitDefault := &v1alpha1.PostRestartJobSpec{
		Enabled: true, Script: "echo ok", Command: []string{"bash", "-c"},
	}

	unsetSum, err := PostRestartJobChecksum(unset, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing unset checksum: %v", err)
	}
	explicitSum, err := PostRestartJobChecksum(explicitDefault, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing explicit-default checksum: %v", err)
	}
	if unsetSum != explicitSum {
		t.Fatalf("expected Command unset and Command=[bash -c] (the default) to produce the same "+
			"checksum, got %q vs %q", unsetSum, explicitSum)
	}

	// Sanity: a genuinely different Command must still change the checksum.
	different := &v1alpha1.PostRestartJobSpec{Enabled: true, Script: "echo ok", Command: []string{"sh", "-c"}}
	differentSum, err := PostRestartJobChecksum(different, "same-config-checksum")
	if err != nil {
		t.Fatalf("computing different-command checksum: %v", err)
	}
	if differentSum == unsetSum {
		t.Fatalf("expected a non-default Command to change the checksum, got %q for both", differentSum)
	}
}

func TestBuildService_WithOpenAPIPort(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			OpenAPI: &v1alpha1.OpenAPIExportSpec{Enabled: true, Port: 9090},
		},
	}
	svc := &corev1.Service{}
	BuildService(svc, gw)

	var names []string
	for _, p := range svc.Spec.Ports {
		names = append(names, p.Name)
	}
	if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[1].Name != "openapi" ||
		svc.Spec.Ports[1].Port != 9090 {
		t.Fatalf("openapi port not exposed: %v", names)
	}
}

func TestBuildService_OpenAPIDisabled(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw"},
		Spec:       v1alpha1.KrakenDGatewaySpec{},
	}
	svc := &corev1.Service{}
	BuildService(svc, gw)
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected single http port, got %d", len(svc.Spec.Ports))
	}
}

func TestBuildDeployment_OpenAPIContainersAndVolume(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Edition: v1alpha1.EditionCE,
			Version: "2.13",
			OpenAPI: &v1alpha1.OpenAPIExportSpec{
				Enabled:        true,
				Audience:       "public",
				SkipJSONSchema: true,
			},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cksum", "", "krakend:2.13")

	if len(dep.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("expected krakend + openapi sidecar, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	var sawExport bool
	for _, ic := range dep.Spec.Template.Spec.InitContainers {
		if ic.Name == "openapi-export" {
			sawExport = true
			joined := strings.Join(ic.Args, " ")
			if !strings.Contains(joined, "--audience public") {
				t.Fatalf("audience flag missing: %v", ic.Args)
			}
			if !strings.Contains(joined, "--skip-jsonschema") {
				t.Fatalf("skip-jsonschema flag missing: %v", ic.Args)
			}
		}
	}
	if !sawExport {
		t.Fatalf("openapi-export init container missing")
	}
	var sawVolume bool
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "openapi" {
			sawVolume = true
		}
	}
	if !sawVolume {
		t.Fatalf("openapi volume missing")
	}
}

func TestBuildDeployment_OpenAPINoAudienceStripsConfig(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Edition: v1alpha1.EditionCE,
			Version: "2.13",
			OpenAPI: &v1alpha1.OpenAPIExportSpec{
				Enabled:        true,
				SkipJSONSchema: true,
			},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cksum", "", "krakend:2.13")

	var exportInit *corev1.Container
	for i := range dep.Spec.Template.Spec.InitContainers {
		if dep.Spec.Template.Spec.InitContainers[i].Name == "openapi-export" {
			exportInit = &dep.Spec.Template.Spec.InitContainers[i]
		}
	}
	if exportInit == nil {
		t.Fatal("openapi-export init container missing")
	}

	// When no audience is configured the container must use a shell
	// script that strips audience arrays before calling the export.
	if len(exportInit.Command) < 2 || exportInit.Command[0] != "sh" {
		t.Fatalf("expected sh -c wrapper, got command=%v", exportInit.Command)
	}
	script := exportInit.Command[2]
	if !strings.Contains(script, "sed") {
		t.Fatalf("expected sed in script, got: %s", script)
	}
	if !strings.Contains(script, "krakend-all.json") {
		t.Fatalf("expected stripped config path in script, got: %s", script)
	}
	if !strings.Contains(script, "--skip-jsonschema") {
		t.Fatalf("skip-jsonschema flag missing from script: %s", script)
	}
	if strings.Contains(script, "--audience") {
		t.Fatalf("audience flag should NOT be present when unset: %s", script)
	}
	if len(exportInit.Args) != 0 {
		t.Fatalf("expected no args when using sh -c, got %v", exportInit.Args)
	}
}

func TestBuildDeployment_OpenAPIEEMountsLicenseAndTmp(t *testing.T) {
	gw := &v1alpha1.KrakenDGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec: v1alpha1.KrakenDGatewaySpec{
			Edition: v1alpha1.EditionEE,
			Version: "2.13",
			License: &v1alpha1.LicenseConfig{
				SecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "lic-secret"},
					Key:                  "LICENSE",
				},
			},
			OpenAPI: &v1alpha1.OpenAPIExportSpec{Enabled: true},
		},
	}
	dep := &appsv1.Deployment{}
	BuildDeployment(dep, gw, "cksum", "", "krakend-ee:2.13")

	var exportInit *corev1.Container
	for i := range dep.Spec.Template.Spec.InitContainers {
		if dep.Spec.Template.Spec.InitContainers[i].Name == "openapi-export" {
			exportInit = &dep.Spec.Template.Spec.InitContainers[i]
		}
	}
	if exportInit == nil {
		t.Fatal("openapi-export init container missing")
	}

	mountNames := map[string]bool{}
	for _, m := range exportInit.VolumeMounts {
		mountNames[m.Name] = true
	}
	if !mountNames["license"] {
		t.Error("expected license volume mount on openapi-export init container")
	}
	if !mountNames["tmp"] {
		t.Error("expected tmp volume mount on openapi-export init container")
	}
	if !mountNames["config"] {
		t.Error("expected config volume mount on openapi-export init container")
	}
}
