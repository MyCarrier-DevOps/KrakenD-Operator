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
	merged := mergeDragonflyPodSecurityContext(nil, nil)

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
	merged := mergeDragonflyPodSecurityContext(user, nil)

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
	merged := mergeDragonflyPodSecurityContext(user, nil)

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

// TestMergeDragonflyPodSecurityContext_PodScopeRootAloneRestoresMainParity
// covers fix-round review 2 (T1/R1): a user PodSecurityContext setting ONLY
// runAsUser:0 (podUid0, no ContainerSecurityContext at all) must have the
// inherited runAsNonRoot:true default DROPPED — restoring the render main
// produced for this exact shape before the strategic-merge change (full
// replace meant the default was never in the picture), rather than the
// kubelet-invalid {0, true} pair the merge change would otherwise
// introduce. Supersedes the now-removed
// TestMergeDragonflyPodSecurityContext_RootUserNoLongerSelfHeals, whose
// premise (no pod-scope fixup exists at all) fix-round review 2 reversed —
// see mergeDragonflyPodSecurityContext's doc for the full rationale
// (this fixup exists for GRANDFATHERED/webhook-bypassed CR parity, not as
// a new admission-time self-heal; validateDragonflyRunAsRoot still rejects
// this shape for any NEW or CHANGED spec — see webhook_test.go's
// TestGatewayValidator_DragonflyPodScopeRunAsUserZeroUnsetRunAsNonRootRejected).
func TestMergeDragonflyPodSecurityContext_PodScopeRootAloneRestoresMainParity(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser: new(int64(0)),
	}
	merged := mergeDragonflyPodSecurityContext(user, nil)

	if merged.RunAsUser == nil || *merged.RunAsUser != 0 {
		t.Fatalf("user-set runAsUser:0 not applied: %+v", merged.RunAsUser)
	}
	if merged.RunAsNonRoot != nil {
		t.Fatalf("expected the inherited runAsNonRoot:true default to be dropped "+
			"(main-parity fixup), got %+v", merged.RunAsNonRoot)
	}
	// fsGroup is unrelated and must survive regardless.
	if merged.FSGroup == nil || *merged.FSGroup != 999 {
		t.Fatalf("hardened fsGroup:999 lost: %+v", merged.FSGroup)
	}
}

// TestMergeDragonflyPodSecurityContext_ContainerScopeRootAloneTriggersPodFixup
// covers the INDIRECT (containerUid0) branch: a user ContainerSecurityContext
// setting runAsUser:0 (with its own RunAsNonRoot left unset — so the
// container-scope fixup already clears the container-level default) must
// also clear the POD-level inherited default, given a non-nil (but
// otherwise unrelated) user PodSecurityContext — otherwise the container's
// per-field fallback to pod scope would re-introduce the exact {0, true}
// pair this fix-round set out to avoid one level up.
func TestMergeDragonflyPodSecurityContext_ContainerScopeRootAloneTriggersPodFixup(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsGroup: new(int64(999)), // non-nil pod ctx, unrelated field set
	}
	userContainer := &corev1.SecurityContext{
		RunAsUser: new(int64(0)),
	}
	merged := mergeDragonflyPodSecurityContext(user, userContainer)

	if merged.RunAsNonRoot != nil {
		t.Fatalf("expected the inherited runAsNonRoot:true default to be dropped when the "+
			"container scope alone requests root, got %+v", merged.RunAsNonRoot)
	}
}

// TestMergeDragonflyPodSecurityContext_BothScopesRootTriggersPodFixup covers
// the combined branch: both scopes independently requesting root (with
// RunAsNonRoot unset at both) must still drop the pod-level default exactly
// once (podUid0 and containerUid0 are both true; the fixup is idempotent).
func TestMergeDragonflyPodSecurityContext_BothScopesRootTriggersPodFixup(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsUser: new(int64(0)),
	}
	userContainer := &corev1.SecurityContext{
		RunAsUser: new(int64(0)),
	}
	merged := mergeDragonflyPodSecurityContext(user, userContainer)

	if merged.RunAsNonRoot != nil {
		t.Fatalf("expected the inherited runAsNonRoot:true default to be dropped when both "+
			"scopes request root, got %+v", merged.RunAsNonRoot)
	}
}

// TestMergeDragonflyPodSecurityContext_NeitherScopeRootFixupDoesNotFire
// verifies the fixup only fires for an actual uid0 request: a non-nil user
// PodSecurityContext with an unrelated field set, and a non-root
// ContainerSecurityContext, must leave the inherited runAsNonRoot:true
// default untouched.
func TestMergeDragonflyPodSecurityContext_NeitherScopeRootFixupDoesNotFire(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsGroup: new(int64(999)),
	}
	userContainer := &corev1.SecurityContext{
		RunAsUser: new(int64(1234)),
	}
	merged := mergeDragonflyPodSecurityContext(user, userContainer)

	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("expected the inherited runAsNonRoot:true default to survive when neither "+
			"scope requests root, got %+v", merged.RunAsNonRoot)
	}
}

// TestMergeDragonflyPodSecurityContext_ContainerExplicitRunAsNonRootFixupDoesNotFire
// verifies the containerUid0 branch requires the CONTAINER's own
// RunAsNonRoot to be nil, not just the pod's: a container that explicitly
// sets RunAsUser:0 AND RunAsNonRoot (its own explicit, if
// self-contradictory, choice) must not trigger the pod-level fixup on its
// behalf.
func TestMergeDragonflyPodSecurityContext_ContainerExplicitRunAsNonRootFixupDoesNotFire(t *testing.T) {
	user := &corev1.PodSecurityContext{
		RunAsGroup: new(int64(999)),
	}
	userContainer := &corev1.SecurityContext{
		RunAsUser:    new(int64(0)),
		RunAsNonRoot: new(true),
	}
	merged := mergeDragonflyPodSecurityContext(user, userContainer)

	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("expected the inherited runAsNonRoot:true default to survive when the "+
			"container scope's own RunAsNonRoot is explicit, got %+v", merged.RunAsNonRoot)
	}
}

// TestMergeDragonflyPodSecurityContext_PodNilWithContainerRootLeavesLegacyShapeBroken
// covers the deliberate exclusion: a nil user PodSecurityContext with a
// root-requesting ContainerSecurityContext must NOT trigger the fixup — this
// is the legacy "container-0 + pod-nil" shape, which already rendered the
// kubelet-broken {0, true} pair on main (full-replace only ever applied to a
// NON-nil user PodSecurityContext; a nil one always fell through to the
// hardcoded default). Parity means leaving it exactly as broken as main did,
// not silently granting root the fixup was never asked to grant. See
// external_crd_test.go's
// TestBuildDragonfly_ContainerRootUserPodNilLegacyShapeMainParity for the
// build-level counterpart.
func TestMergeDragonflyPodSecurityContext_PodNilWithContainerRootLeavesLegacyShapeBroken(t *testing.T) {
	userContainer := &corev1.SecurityContext{
		RunAsUser: new(int64(0)),
	}
	merged := mergeDragonflyPodSecurityContext(nil, userContainer)

	if merged.RunAsNonRoot == nil || !*merged.RunAsNonRoot {
		t.Fatalf("expected the inherited runAsNonRoot:true default to survive unchanged for "+
			"a nil user PodSecurityContext (deliberately excluded), got %+v", merged.RunAsNonRoot)
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
	merged := mergeDragonflyPodSecurityContext(user, nil)

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
// (7, via fillNonZero) same as every other field. fillNonZero also sets
// RunAsNonRoot non-nil (true), which alone keeps fix-round review 2's
// cross-scope pod fixup (mergeDragonflyPodSecurityContext) from firing
// regardless of RunAsUser's value — the fixup only ever inspects
// runAsUser:0 requests where RunAsNonRoot is left UNSET, and this test sets
// every field explicitly. userContainer is passed nil: the containerUid0
// branch is exercised by the dedicated cross-scope tests above, not this
// full-field round-trip.
func TestMergeDragonflyPodSecurityContext_ReflectiveRoundTrip(t *testing.T) {
	user := &corev1.PodSecurityContext{}
	fillNonZero(reflect.ValueOf(user).Elem())

	merged := mergeDragonflyPodSecurityContext(user, nil)

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
// every settable field including RunAsUser. RunAsUser is safe to include
// unskipped: fillNonZero fills ints with 7, not 0, so fix-round review 2's
// cross-scope pod fixup (which only inspects runAsUser:0 with RunAsNonRoot
// unset) never fires when this test isolates the RunAsUser field alone —
// see the dedicated cross-scope tests above for that branch. userContainer
// is passed nil throughout, so the containerUid0 branch is inert here too.
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

			merged := mergeDragonflyPodSecurityContext(user, nil)
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
