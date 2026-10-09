// Package testjson gives tests a key-order-free JSON comparison under the
// value-equal contract (#821): Compact encodes a value with every object's
// members sorted by key, so a gojq fold (alphabetical keys) and a want
// fixture (program order) compare equal on values.
package testjson

import (
	"sort"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// MustParse decodes exactly one JSON value (jq extensions included).
func MustParse(t *testing.T, s string) jsonv.Value {
	t.Helper()
	vs, err := jsonv.DecodeStream(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parse want: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("want exactly one value, got %d", len(vs))
	}
	return vs[0]
}

// Compact is v with every object's members sorted by key, compact-encoded.
func Compact(v jsonv.Value) string {
	return string(jsonv.Append(nil, sorted(v), jsonv.Options{}))
}

func sorted(v jsonv.Value) jsonv.Value {
	switch v.Kind() {
	case jsonv.KindNumber:
		// jsonv preserves the input's number literal (jq 1.7+ semantics) and
		// want fixtures carry jq's literal text; a gojq fold re-formats numbers
		// through float64. Compare the values, not the literals.
		f, _ := v.AsFloat()
		return jsonv.Num(f)
	case jsonv.KindNull, jsonv.KindFalse, jsonv.KindTrue, jsonv.KindString:
		return v
	case jsonv.KindArray:
		es := v.Elems()
		out := make([]jsonv.Value, len(es))
		for i, e := range es {
			out[i] = sorted(e)
		}
		return jsonv.Array(out...)
	case jsonv.KindObject:
		ms := v.Members()
		out := make([]jsonv.Member, len(ms))
		copy(out, ms)
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		for i := range out {
			out[i].Val = sorted(out[i].Val)
		}
		return jsonv.Object(out...)
	}
	return v
}
