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
