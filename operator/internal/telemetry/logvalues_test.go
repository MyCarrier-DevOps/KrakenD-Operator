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

package telemetry_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	otellog "go.opentelemetry.io/otel/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// Named string, bool, int and float types, which the log bridge cannot convert
// on its own.
type (
	namedString string
	namedBool   bool
	namedInt    int
	namedFloat  float64
)

// level is a named int that reads as its name.
type level int

func (l level) String() string { return [...]string{"info", "debug"}[l] }

// codeError is an error whose underlying kind is a string.
type codeError string

func (e codeError) Error() string { return "code " + string(e) }

// namedComplex is a complex number type the bridge does not know.
type namedComplex complex128

// point is a struct with exported fields and no methods.
type point struct {
	A string
	B int
}

// label is a Stringer whose value receiver panics on a nil pointer.
type label struct{ name string }

func (l label) String() string { return l.name }

// marshaled is a logr.Marshaler that logs as the value it holds.
type marshaled struct{ as any }

func (m marshaled) MarshalLog() any { return m.as }

// stringValue is how a string reads in a stdout JSON record inside a list or
// a map.
func stringValue(s string) map[string]any {
	return map[string]any{"Type": "STRING", "Value": s}
}

// complexValue is how a complex number reads in a stdout JSON record.
func complexValue(r, i float64) []any {
	return []any{
		map[string]any{"Key": "r", "Value": map[string]any{"Type": "FLOAT64", "Value": r}},
		map[string]any{"Key": "i", "Value": map[string]any{"Type": "FLOAT64", "Value": i}},
	}
}

func ptrTo[T any](v T) *T { return &v }

// A value logged per call or bound with WithValues reaches the record as the
// readable value, never as the log bridge's "unhandled:" fallback.
func TestLogger_ValuesAreConvertedToWhatTheyRead(t *testing.T) {
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "nm"}}
	tests := []struct {
		name  string
		value any
		want  any
	}{
		{"types.UID", types.UID("3f0c-uid"), "3f0c-uid"},
		{"klog.ObjectRef", klog.KObj(configMap), "ns/nm"},
		{"types.NamespacedName", types.NamespacedName{Namespace: "ns", Name: "nm"}, "ns/nm"},
		{"named string", namedString("s"), "s"},
		{"named bool", namedBool(true), true},
		{"named int", namedInt(4), float64(4)},
		{"named float", namedFloat(1.5), 1.5},
		{"Stringer of kind int", level(1), "debug"},
		{"error of kind string", codeError("E1"), "code E1"},
		{"time.Duration", 3 * time.Second, float64(3e9)},
		{"time.Time", time.Unix(1700000000, 0), float64(1.7e18)},
		{"slice of UIDs", []types.UID{"u1", "u2"}, []any{stringValue("u1"), stringValue("u2")}},
		{"pointer to a UID", ptrTo(types.UID("u1")), "u1"},
		{"complex128", complex128(1 + 2i), complexValue(1, 2)},
		{"named complex", namedComplex(1 + 2i), complexValue(1, 2)},
		{"nil pointer to a Stringer", (*label)(nil), "<nil>"},
		{"struct", point{"a", 1}, "{A:a B:1}"},
		{"pointer to a struct", &point{"a", 1}, "{A:a B:1}"},
		{"slice of strings", []string{"a"}, []any{stringValue("a")}},
		{"map of strings", map[string]string{"a": "b"},
			[]any{map[string]any{"Key": "a", "Value": stringValue("b")}}},
		{"marshaler of a UID", marshaled{types.UID("u1")}, "u1"},
		{"marshaler of a map", marshaled{map[string]any{"id": types.UID("u1")}},
			[]any{map[string]any{"Key": "id", "Value": stringValue("u1")}}},
	}
	ways := map[string]func(l logr.Logger, value any){
		"per call":   func(l logr.Logger, value any) { l.Info("m", "key", value) },
		"WithValues": func(l logr.Logger, value any) { l.WithValues("key", value).Info("m") },
	}
	for _, tt := range tests {
		for way, emit := range ways {
			t.Run(tt.name+"/"+way, func(t *testing.T) {
				logger, out := newStdoutLogger(t, otellog.SeverityInfo)
				emit(logger, tt.value)
				got := records(t, out)
				if len(got) != 1 || len(got[0].Attributes) != 1 {
					t.Fatalf("records = %+v, want one record with one attribute", got)
				}
				if v := got[0].Attributes[0].Value.Value; !reflect.DeepEqual(v, tt.want) || strings.Contains(out.String(), "unhandled:") {
					t.Errorf("value = %#v, want %#v\n%s", v, tt.want, out)
				}
			})
		}
	}
}

// A named logger keeps converting values, and an error record converts them
// too.
func TestLogger_ChildLoggersAndErrorsConvertValues(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)

	child := logger.WithName("child").WithValues("bound", types.UID("u1"))
	child.Info("m", "call", types.UID("u2"))
	child.Error(nil, "failed", "call", types.UID("u3"))

	got := records(t, out)
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	for i, call := range []string{"u2", "u3"} {
		attrs := map[string]any{}
		for _, a := range got[i].Attributes {
			attrs[a.Key] = a.Value.Value
		}
		if got[i].Scope.Name != "test/child" || attrs["bound"] != "u1" || attrs["call"] != call {
			t.Errorf("record %d = scope %q, attributes %v; want scope test/child, bound u1, call %s",
				i, got[i].Scope.Name, attrs, call)
		}
	}
}

// A value that contains itself is cut off rather than followed forever.
func TestLogger_ASelfContainingValueDoesNotOverflow(t *testing.T) {
	logger, out := newStdoutLogger(t, otellog.SeverityInfo)
	loop := map[string]any{}
	loop["self"] = loop

	logger.Info("m", "loop", loop)

	if got := records(t, out); len(got) != 1 {
		t.Errorf("got %d records, want 1", len(got))
	}
}
