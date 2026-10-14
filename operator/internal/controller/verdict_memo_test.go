package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/mycarrier-devops/krakend-operator/internal/configcheck"
)

var memoOwner = types.NamespacedName{Namespace: "ns", Name: "gw"}

func TestVerdictMemo_KeepsWhatTheLastPassUsed(t *testing.T) {
	var m verdictMemo
	first := m.begin(memoOwner)
	first.Store("a", configcheck.Verdict{OK: true})
	first.Store("b", configcheck.Verdict{Output: "bad"})
	m.end(memoOwner, first, false)

	second := m.begin(memoOwner)
	if v, ok := second.Lookup("b"); !ok || v.Output != "bad" {
		t.Fatalf("Lookup(b) = %+v, %v; want the verdict the last pass stored", v, ok)
	}
	m.end(memoOwner, second, false)

	third := m.begin(memoOwner)
	if _, ok := third.Lookup("a"); ok {
		t.Error("a verdict the last pass did not use is still kept")
	}
	if _, ok := third.Lookup("b"); !ok {
		t.Error("a verdict the last pass used was dropped")
	}
}

func TestVerdictMemo_AFailedPassKeepsWhatTheLastPassKept(t *testing.T) {
	var m verdictMemo
	first := m.begin(memoOwner)
	first.Store("root", configcheck.Verdict{OK: true})
	m.end(memoOwner, first, false)

	failing := m.begin(memoOwner)
	failing.Store("unit-1", configcheck.Verdict{OK: true})
	m.end(memoOwner, failing, true)

	next := m.begin(memoOwner)
	for _, key := range []string{"root", "unit-1"} {
		if _, ok := next.Lookup(key); !ok {
			t.Errorf("Lookup(%s) missed after a failed pass", key)
		}
	}
}

func TestVerdictMemo_ForgetDropsTheOwnerAndOwnersAreApart(t *testing.T) {
	var m verdictMemo
	other := types.NamespacedName{Namespace: "ns", Name: "other"}
	for _, owner := range []types.NamespacedName{memoOwner, other} {
		p := m.begin(owner)
		p.Store("k", configcheck.Verdict{OK: true})
		m.end(owner, p, false)
	}

	m.forget(memoOwner)

	if _, ok := m.begin(memoOwner).Lookup("k"); ok {
		t.Error("a forgotten owner's verdict is still kept")
	}
	if _, ok := m.begin(other).Lookup("k"); !ok {
		t.Error("forgetting one owner dropped another's verdict")
	}
}

func TestCountedPass_CountsEachFreshRejection(t *testing.T) {
	var m verdictMemo
	rejections := 0
	p := countedPass{passMemo: m.begin(memoOwner), rejected: func() { rejections++ }}

	p.Store("ok", configcheck.Verdict{OK: true})
	p.Store("bad", configcheck.Verdict{Output: "bad"})
	if _, ok := p.Lookup("bad"); !ok {
		t.Fatal("the stored rejection is not remembered")
	}

	if rejections != 1 {
		t.Errorf("counted %d rejections, want 1", rejections)
	}
}
