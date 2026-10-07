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

package telemetry

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
)

// readableSink wraps the log bridge's sink so every value is converted to a
// type the bridge renders readably before it sees it: the bridge turns a type
// it does not know, such as a named string, into an "unhandled:" fallback.
type readableSink struct {
	logr.LogSink
}

// Info converts keysAndValues and logs through the wrapped sink.
func (s readableSink) Info(level int, msg string, keysAndValues ...any) {
	s.LogSink.Info(level, msg, readableKeysAndValues(keysAndValues)...)
}

// Error converts keysAndValues and logs through the wrapped sink.
func (s readableSink) Error(err error, msg string, keysAndValues ...any) {
	s.LogSink.Error(err, msg, readableKeysAndValues(keysAndValues)...)
}

// WithValues converts keysAndValues and binds them on the wrapped sink.
func (s readableSink) WithValues(keysAndValues ...any) logr.LogSink {
	return readableSink{s.LogSink.WithValues(readableKeysAndValues(keysAndValues)...)}
}

// WithName names the wrapped sink and keeps converting values.
func (s readableSink) WithName(name string) logr.LogSink {
	return readableSink{s.LogSink.WithName(name)}
}

// readableKeysAndValues converts the values of a key/value list, leaving the
// keys alone. A context.Context value is the span the record belongs to, which
// the bridge reads as a context, so it is not converted.
func readableKeysAndValues(keysAndValues []any) []any {
	out := make([]any, len(keysAndValues))
	copy(out, keysAndValues)
	for i := 1; i < len(out); i += 2 {
		if _, ok := out[i].(context.Context); !ok {
			out[i] = readableValue(out[i])
		}
	}
	return out
}

// maxValueNodes bounds the work spent on one value: past it, what is left of
// the value is rendered as "<truncated>".
const maxValueNodes = 10_000

// valueConverter converts one value. It is not safe for concurrent use.
type valueConverter struct {
	// budget is the number of nodes it may still convert.
	budget int
	// open holds the maps, slices and pointers on the path to the node being
	// converted, to cut a value that contains itself.
	open map[uintptr]bool
}

// readableValue returns v, or what it converts to, so the log bridge renders
// it readably:
//   - an error: its Error();
//   - a fmt.Stringer: its String(), which for the object references
//     controller-runtime and klog log is "namespace/name";
//   - a logr.Marshaler: the value it logs as, converted in turn;
//   - a named string, bool, int, float or complex type: its base type;
//   - a map, slice, array or pointer: the same with its elements converted.
//
// A time.Duration and a time.Time stay as they are: the bridge records them as
// numbers. A value that contains itself renders "<cycle>" where it repeats.
func readableValue(v any) any {
	c := valueConverter{budget: maxValueNodes, open: map[uintptr]bool{}}
	return c.convert(v)
}

func (c *valueConverter) convert(v any) any {
	if c.budget--; c.budget < 0 {
		return "<truncated>"
	}
	switch x := v.(type) {
	case nil, time.Duration, time.Time:
		return v
	case error:
		return orPlain(v, x.Error)
	case fmt.Stringer:
		return orPlain(v, x.String)
	case logr.Marshaler:
		if logged, ok := try(x.MarshalLog); ok {
			return c.convert(logged)
		}
		return plain(v)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() { //nolint:exhaustive // every other kind is rendered by plain below
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	case reflect.Complex64, reflect.Complex128:
		return rv.Complex()
	case reflect.Slice, reflect.Array:
		return c.convertList(v, rv)
	case reflect.Pointer:
		if rv.IsNil() {
			return v
		}
		return c.enter(rv, func() any { return c.convert(rv.Elem().Interface()) })
	case reflect.Map:
		return c.enter(rv, func() any { return c.convertMap(rv) })
	case reflect.Struct:
		return v
	}
	return plain(v)
}

// convertList converts the elements of a slice or an array, leaving a byte
// slice to the bridge.
func (c *valueConverter) convertList(v any, rv reflect.Value) any {
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		return v
	}
	if rv.Len() > c.budget {
		return "<truncated>"
	}
	if rv.Kind() == reflect.Array || rv.Len() == 0 {
		return c.convertElements(rv)
	}
	return c.enter(rv, func() any { return c.convertElements(rv) })
}

func (c *valueConverter) convertElements(rv reflect.Value) any {
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = c.convert(rv.Index(i).Interface())
	}
	return out
}

// convertMap converts the values of a map, keyed as the bridge keys a map.
func (c *valueConverter) convertMap(rv reflect.Value) any {
	if rv.Len() > c.budget {
		return "<truncated>"
	}
	out := make(map[string]any, rv.Len())
	for iter := rv.MapRange(); iter.Next(); {
		out[fmt.Sprintf("%+v", iter.Key().Interface())] = c.convert(iter.Value().Interface())
	}
	return out
}

// enter runs convert with rv marked as open, or returns "<cycle>" if it is
// open already.
func (c *valueConverter) enter(rv reflect.Value, convert func() any) any {
	id := rv.Pointer()
	if c.open[id] {
		return "<cycle>"
	}
	c.open[id] = true
	defer delete(c.open, id)
	return convert()
}

// plain renders v as fmt prints it with field names, the form a value the
// converter has no rule for ends up in.
func plain(v any) string { return fmt.Sprintf("%+v", v) }

// orPlain returns what method returns, or plain(v) when calling it panics, as
// it does on a nil pointer whose method has a value receiver.
func orPlain(v any, method func() string) string {
	if s, ok := try(method); ok {
		return s
	}
	return plain(v)
}

// try calls f and reports whether it returned rather than panicked.
func try[T any](f func() T) (result T, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return f(), true
}
