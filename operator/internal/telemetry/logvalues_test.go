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

// marshaled is a logr.Marshaler that logs as the value it holds.
type marshaled struct{ as any }

func (m marshaled) MarshalLog() any { return m.as }

// stringValue is how a string reads in a stdout JSON record inside a list or
// a map.
func stringValue(s string) map[string]any {
	return map[string]any{"Type": "STRING", "Value": s}
}

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
