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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestMergeDragonflyPodSecurityContext_NilUserUsesHardenedDefaults covers
// the nil-spec path: no user override at all must produce exactly the
// hardened defaults (runAsNonRoot:true, runAsUser/runAsGroup:999,
// fsGroup:999).
func TestMergeDragonflyPodSecurityContext_NilUserUsesHardenedDefaults(t *testing.T) {
	merged := mergeDragonflyPodSecurityContext(nil)

	want := defaultDragonflyPodSecurityContext()
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("expected nil user to produce the hardened defaults unchanged: got %+v, want %+v",
			merged, want)
	}
}

// TestMergeDragonflyContainerSecurityContext_NilUserUsesHardenedDefaults is
// the container-scope counterpart.
func TestMergeDragonflyContainerSecurityContext_NilUserUsesHardenedDefaults(t *testing.T) {
	merged := mergeDragonflyContainerSecurityContext(nil)

	want := defaultDragonflyContainerSecurityContext()
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("expected nil user to produce the hardened defaults unchanged: got %+v, want %+v",
			merged, want)
	}
}

// TestMergeDragonflyPodSecurityContext_FSGroupPreservedOnPartialOverride is
// the headline regression test for this fix: a user setting ONLY
// runAsUser in spec.dragonfly.podSecurityContext must NOT lose the
// hardened fsGroup:999 default (or runAsNonRoot:true / runAsGroup:999) —
// previously this was a full-replace, discarding fsGroup:999 and creating
// a live ownership hazard for PVC-backed Dragonfly instances (the uid/gid
// 999 "dfly" process losing group-write access to
// --dir=/dragonfly/snapshots and crashlooping).
func TestMergeDragonflyPodSecurityContext_FSGroupPreservedOnPartialOverride(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser: new(int64(1234)),
	}
	merged := mergeDragonflyPodSecurityContext(user)

	if merged.RunAsUser == nil || *merged.RunAsUser != 1234 {
		t.Fatalf("user-set runAsUser:1234 not applied: %+v", merged.RunAsUser)
	}
	if merged.FSGroup == nil || *merged.FSGroup != 999 {
		t.Fatalf("hardened fsGroup:999 lost by partial user override: %+v", merged.FSGroup)
	}
	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("hardened runAsNonRoot:true lost by partial user override: %+v", merged.RunAsNonRoot)
	}
	if merged.RunAsGroup == nil || *merged.RunAsGroup != 999 {
		t.Fatalf("hardened runAsGroup:999 lost by partial user override: %+v", merged.RunAsGroup)
	}
}

// TestMergeDragonflyPodSecurityContext_UserOverrideWins verifies the
// simple override case: a user-set runAsUser is applied verbatim.
func TestMergeDragonflyPodSecurityContext_UserOverrideWins(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser: new(int64(1234)),
	}
	merged := mergeDragonflyPodSecurityContext(user)

	if merged.RunAsUser == nil || *merged.RunAsUser != 1234 {
		t.Fatalf("expected overridden runAsUser:1234, got %+v", merged.RunAsUser)
	}
}

// TestMergeDragonflyContainerSecurityContext_UserOverrideWins is the
// container-scope counterpart.
func TestMergeDragonflyContainerSecurityContext_UserOverrideWins(t *testing.T) {
	user := &corev1.SecurityContext{
		RunAsUser: new(int64(1234)),
	}
	merged := mergeDragonflyContainerSecurityContext(user)

	if merged.RunAsUser == nil || *merged.RunAsUser != 1234 {
		t.Fatalf("expected overridden runAsUser:1234, got %+v", merged.RunAsUser)
	}
	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("hardened runAsNonRoot:true lost by partial user override: %+v", merged.RunAsNonRoot)
	}
	if merged.RunAsGroup == nil || *merged.RunAsGroup != 999 {
		t.Fatalf("hardened runAsGroup:999 lost by partial user override: %+v", merged.RunAsGroup)
	}
}

// TestMergeDragonflyContainerSecurityContext_RunAsNonRootFalseSurvivesMerge
// covers item 5d of fix-round review 1: a user who explicitly sets
// containerSecurityContext.runAsNonRoot: false (independent of runAsUser)
// must have that value survive the strategic merge over the hardened
// default of true — ordinary single-field override semantics, verified
// directly (not just implicitly via the SingleFieldRoundTrip table) since
// this is the exact field the container-scope uid0 fixup exists to protect.
func TestMergeDragonflyContainerSecurityContext_RunAsNonRootFalseSurvivesMerge(t *testing.T) {
	user := &corev1.SecurityContext{
		RunAsNonRoot: new(false),
	}
	merged := mergeDragonflyContainerSecurityContext(user)

	if merged.RunAsNonRoot == nil || *merged.RunAsNonRoot {
		t.Fatalf("expected user-set runAsNonRoot:false to survive the merge over the "+
			"hardened runAsNonRoot:true default, got %+v", merged.RunAsNonRoot)
	}
	// RunAsUser/RunAsGroup are unrelated and must keep their hardened
	// defaults.
	if merged.RunAsUser == nil || *merged.RunAsUser != 999 {
		t.Fatalf("hardened runAsUser:999 lost by unrelated runAsNonRoot override: %+v", merged.RunAsUser)
	}
	if merged.RunAsGroup == nil || *merged.RunAsGroup != 999 {
		t.Fatalf("hardened runAsGroup:999 lost by unrelated runAsNonRoot override: %+v", merged.RunAsGroup)
	}
}

// TestMergeDragonflyPodSecurityContext_RootUserNoLongerSelfHeals covers
// fix-round review 1, change #2: unlike job.go's pod-scope fixup (which
// self-heals podSecurityContext.runAsUser:0 by dropping the inherited
// runAsNonRoot default), mergeDragonflyPodSecurityContext no longer inspects
// runAsUser at all — the pod-scope self-heal fixup was removed because
// dragonfly's container-scope default PINS RunAsNonRoot:true regardless of
// what pod scope says (container-scope always wins per-field at the
// kubelet), so there was never a real capability for the pod-scope fixup to
// preserve. A bare podSecurityContext.runAsUser:0 with no explicit
// runAsNonRoot now merges straight through with the inherited
// runAsNonRoot:true default intact (unchanged) — validateDragonflyRunAsRoot
// is what now rejects this combination at admission instead (see
// webhook_test.go's TestGatewayValidator_DragonflyPodScopeRunAsUserZeroUnsetRunAsNonRootRejected).
func TestMergeDragonflyPodSecurityContext_RootUserNoLongerSelfHeals(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser: new(int64(0)),
	}
	merged := mergeDragonflyPodSecurityContext(user)

	if merged.RunAsUser == nil || *merged.RunAsUser != 0 {
		t.Fatalf("user-set runAsUser:0 not applied: %+v", merged.RunAsUser)
	}
	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("expected the inherited runAsNonRoot:true default to survive unchanged "+
			"(the pod-scope self-heal fixup was removed), got %+v", merged.RunAsNonRoot)
	}
	// fsGroup is unrelated and must survive regardless.
	if merged.FSGroup == nil || *merged.FSGroup != 999 {
		t.Fatalf("hardened fsGroup:999 lost: %+v", merged.FSGroup)
	}
}

// TestMergeDragonflyPodSecurityContext_RootUserWithExplicitRunAsNonRootHonored
// verifies a user who sets BOTH runAsUser:0 AND runAsNonRoot explicitly at
// pod scope keeps their explicit value untouched (ordinary strategic-merge
// override semantics, independent of any fixup).
func TestMergeDragonflyPodSecurityContext_RootUserWithExplicitRunAsNonRootHonored(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser:    new(int64(0)),
		RunAsNonRoot: new(false),
	}
	merged := mergeDragonflyPodSecurityContext(user)

	if merged.RunAsNonRoot == nil || *merged.RunAsNonRoot {
		t.Fatalf("explicit user runAsNonRoot:false not honored: %+v", merged.RunAsNonRoot)
	}
}

// TestMergeDragonflyContainerSecurityContext_RootUserDropsInheritedRunAsNonRoot
// covers the BLOCKER fix (fix-round review 1, change #1): setting only
// containerSecurityContext.runAsUser:0 (no explicit runAsNonRoot) must not
// leave the contradictory pair {runAsUser:0, runAsNonRoot:true} at
// container scope — the kubelet rejects that combination at container start
// (CreateContainerConfigError), hanging the pod Pending forever (no
// activeDeadlineSeconds unlike the Job).
func TestMergeDragonflyContainerSecurityContext_RootUserDropsInheritedRunAsNonRoot(t *testing.T) {
	user := &corev1.SecurityContext{
		RunAsUser: new(int64(0)),
	}
	merged := mergeDragonflyContainerSecurityContext(user)

	if merged.RunAsUser == nil || *merged.RunAsUser != 0 {
		t.Fatalf("user-set runAsUser:0 not applied: %+v", merged.RunAsUser)
	}
	if merged.RunAsNonRoot != nil {
		t.Fatalf("expected inherited runAsNonRoot default to be dropped when "+
			"runAsUser:0 is set without an explicit runAsNonRoot, got %+v", merged.RunAsNonRoot)
	}
	// runAsGroup is unrelated to the runAsNonRoot fixup and must survive.
	if merged.RunAsGroup == nil || *merged.RunAsGroup != 999 {
		t.Fatalf("hardened runAsGroup:999 lost by the runAsUser:0 fixup: %+v", merged.RunAsGroup)
	}
}

// TestMergeDragonflyContainerSecurityContext_RootUserWithExplicitRunAsNonRootTrueHonored
// verifies the fixup does NOT fire when the user asserts runAsNonRoot: true
// explicitly alongside runAsUser:0 — their explicit (self-contradictory but
// deliberate) choice is left untouched; validateDragonflyRunAsRoot is what
// rejects this combination at admission.
func TestMergeDragonflyContainerSecurityContext_RootUserWithExplicitRunAsNonRootTrueHonored(t *testing.T) {
	user := &corev1.SecurityContext{
		RunAsUser:    new(int64(0)),
		RunAsNonRoot: new(true),
	}
	merged := mergeDragonflyContainerSecurityContext(user)

	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("explicit user runAsNonRoot:true not honored: %+v", merged.RunAsNonRoot)
	}
}

// TestMergeDragonflyContainerSecurityContext_RootUserWithExplicitRunAsNonRootFalseHonored
// verifies the escape hatch: a user who sets BOTH runAsUser:0 AND
// runAsNonRoot:false explicitly gets exactly that — the startable
// {runAsUser:0, runAsNonRoot:false} pair — regardless of the fixup (which
// only fires when RunAsNonRoot is unset).
func TestMergeDragonflyContainerSecurityContext_RootUserWithExplicitRunAsNonRootFalseHonored(t *testing.T) {
	user := &corev1.SecurityContext{
		RunAsUser:    new(int64(0)),
		RunAsNonRoot: new(false),
	}
	merged := mergeDragonflyContainerSecurityContext(user)

	if merged.RunAsNonRoot == nil || *merged.RunAsNonRoot {
		t.Fatalf("explicit user runAsNonRoot:false not honored: %+v", merged.RunAsNonRoot)
	}
}

// TestMergeDragonflyContainerSecurityContext_NonZeroUserFixupDoesNotFire
// verifies the fixup only inspects RunAsUser == 0 — a non-zero user with
// RunAsNonRoot unset must NOT have the inherited runAsNonRoot:true default
// dropped (there is no {uid, runAsNonRoot:true} conflict for a non-root
// uid).
func TestMergeDragonflyContainerSecurityContext_NonZeroUserFixupDoesNotFire(t *testing.T) {
	user := &corev1.SecurityContext{
		RunAsUser: new(int64(1234)),
	}
	merged := mergeDragonflyContainerSecurityContext(user)

	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("expected inherited runAsNonRoot:true default to survive for a non-zero "+
			"runAsUser (fixup must not fire), got %+v", merged.RunAsNonRoot)
	}
}

// Scope note (fix-round review 1, item 5e): the reflective/single-field
// round-trip tests below (TestMergeDragonflyContainerSecurityContext_
// ReflectiveRoundTrip, TestMergeDragonflyPodSecurityContext_
// ReflectiveRoundTrip, and both *_SingleFieldRoundTrip tables) verify MERGE
// semantics only — that mergeDragonfly{Container,Pod}SecurityContext
// preserves every corev1.SecurityContext/PodSecurityContext field the user
// sets, for the FULL upstream field set. They do NOT verify what actually
// reaches the Dragonfly CR: BuildDragonfly's buildSecurityContext/
// buildPodSecurityContext project only a WHITELIST of fields onto the
// emitted CRD map — pod: runAsNonRoot, runAsUser, runAsGroup, fsGroup;
// container: runAsNonRoot, runAsUser, runAsGroup, allowPrivilegeEscalation
// — silently dropping everything else (e.g. capabilities, seccompProfile)
// even though the merge step above preserves them. See
// external_crd_test.go's TestBuildDragonfly_* tests for the build-level
// (actual CRD contract) assertions this whitelist implies.
//
// TestMergeDragonflyContainerSecurityContext_ReflectiveRoundTrip covers
// every field of a fully-populated user SecurityContext: it must survive
// the merge unchanged (the user explicitly set every field, so every field
// must win over the hardened default). Iterating fields via reflection
// (fillNonZero, defined in openapi_job_test.go) keeps this test covering
// the FULL field set even after a future k8s API bump adds new fields.
func TestMergeDragonflyContainerSecurityContext_ReflectiveRoundTrip(t *testing.T) {
	user := &corev1.SecurityContext{}
	fillNonZero(reflect.ValueOf(user).Elem())

	merged := mergeDragonflyContainerSecurityContext(user)

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

// TestMergeDragonflyPodSecurityContext_ReflectiveRoundTrip is the
// PodSecurityContext counterpart. RunAsUser is filled with a non-zero value
// (7, via fillNonZero) same as every other field — mergeDragonflyPodSecurity
// Context no longer has ANY runAsUser-keyed fixup at pod scope (fix-round
// review 1, change #2 removed it), so there is nothing special about
// RunAsUser here anymore; a zero value would work just as well for this
// particular test, but 7 keeps the comment consistent with fillNonZero's
// general int-filling behavior.
func TestMergeDragonflyPodSecurityContext_ReflectiveRoundTrip(t *testing.T) {
	user := &corev1.PodSecurityContext{}
	fillNonZero(reflect.ValueOf(user).Elem())

	merged := mergeDragonflyPodSecurityContext(user)

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

// TestMergeDragonflyContainerSecurityContext_SingleFieldRoundTrip is a
// per-field table test: for EACH settable field of corev1.SecurityContext,
// set only that field in user input and assert the merged output has the
// user's value for that field AND all hardened defaults preserved for the
// others. Complements the all-fields-at-once ReflectiveRoundTrip test above
// by isolating each field individually.
func TestMergeDragonflyContainerSecurityContext_SingleFieldRoundTrip(t *testing.T) {
	base := reflect.TypeOf(corev1.SecurityContext{})
	for i := 0; i < base.NumField(); i++ {
		fieldName := base.Field(i).Name
		t.Run(fieldName, func(t *testing.T) {
			user := &corev1.SecurityContext{}
			uv := reflect.ValueOf(user).Elem()
			f := uv.Field(i)
			if !f.CanSet() {
				t.Skip("unexported field")
			}
			fillNonZero(f)

			merged := mergeDragonflyContainerSecurityContext(user)
			mv := reflect.ValueOf(merged).Elem()

			if !reflect.DeepEqual(f.Interface(), mv.Field(i).Interface()) {
				t.Fatalf("user-set field %s not applied: user=%+v merged=%+v",
					fieldName, f.Interface(), mv.Field(i).Interface())
			}

			def := defaultDragonflyContainerSecurityContext()
			dv := reflect.ValueOf(def).Elem()
			dt := dv.Type()
			for j := 0; j < dv.NumField(); j++ {
				if j == i {
					continue
				}
				otherName := dt.Field(j).Name
				if !reflect.DeepEqual(dv.Field(j).Interface(), mv.Field(j).Interface()) {
					t.Errorf("setting only %s must not disturb hardened default for %s: default=%+v merged=%+v",
						fieldName, otherName, dv.Field(j).Interface(), mv.Field(j).Interface())
				}
			}
		})
	}
}

// TestMergeDragonflyPodSecurityContext_SingleFieldRoundTrip is the
// PodSecurityContext counterpart of
// TestMergeDragonflyContainerSecurityContext_SingleFieldRoundTrip, covering
// every settable field including RunAsUser. RunAsUser was previously
// skipped here with the justification "it triggers the runAsUser:0/7 fixup
// path" — that justification was false even before this fix-round
// (fillNonZero fills ints with 7, not 0, so the then-existing
// runAsUser:0-keyed pod fixup never fired for this test), and is doubly
// moot now that mergeDragonflyPodSecurityContext no longer has ANY
// runAsUser-keyed fixup at pod scope (fix-round review 1, change #2) — a
// user-set RunAsUser now merges straight through like any other field, with
// no special-case interference to guard against.
func TestMergeDragonflyPodSecurityContext_SingleFieldRoundTrip(t *testing.T) {
	base := reflect.TypeOf(corev1.PodSecurityContext{})
	for i := 0; i < base.NumField(); i++ {
		fieldName := base.Field(i).Name
		t.Run(fieldName, func(t *testing.T) {
			user := &corev1.PodSecurityContext{}
			uv := reflect.ValueOf(user).Elem()
			f := uv.Field(i)
			if !f.CanSet() {
				t.Skip("unexported field")
			}
			fillNonZero(f)

			merged := mergeDragonflyPodSecurityContext(user)
			mv := reflect.ValueOf(merged).Elem()

			if !reflect.DeepEqual(f.Interface(), mv.Field(i).Interface()) {
				t.Fatalf("user-set field %s not applied: user=%+v merged=%+v",
					fieldName, f.Interface(), mv.Field(i).Interface())
			}

			def := defaultDragonflyPodSecurityContext()
			dv := reflect.ValueOf(def).Elem()
			dt := dv.Type()
			for j := 0; j < dv.NumField(); j++ {
				if j == i {
					continue
				}
				otherName := dt.Field(j).Name
				if !reflect.DeepEqual(dv.Field(j).Interface(), mv.Field(j).Interface()) {
					t.Errorf("setting only %s must not disturb hardened default for %s: default=%+v merged=%+v",
						fieldName, otherName, dv.Field(j).Interface(), mv.Field(j).Interface())
				}
			}
		})
	}
}
