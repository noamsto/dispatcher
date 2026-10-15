package jsonv_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

func TestObjectOrder(t *testing.T) {
	o := jsonv.Object(
		jsonv.Member{Key: "a", Val: jsonv.Num(1)},
		jsonv.Member{Key: "b", Val: jsonv.Num(2)},
		jsonv.Member{Key: "a", Val: jsonv.Num(3)},
	)
	if got := compact([]jsonv.Value{o}); got != `{"a":3,"b":2}` {
		t.Errorf("Object: %s", got)
	}
	o.Set("c", jsonv.Null())
	o.Set("a", jsonv.Str("x"))
	if got := compact([]jsonv.Value{o}); got != `{"a":"x","b":2,"c":null}` {
		t.Errorf("Set: %s", got)
	}
	if v, ok := o.Get("c"); !ok || !v.IsNull() {
		t.Errorf("Get(c) = %v, %v", v, ok)
	}
	if _, ok := o.Get("missing"); ok {
		t.Error("Get(missing) found an absent key")
	}
	if o.Len() != 3 || len(o.Members()) != 3 {
		t.Errorf("Len = %d", o.Len())
	}
}

func TestArrayAccess(t *testing.T) {
	a := jsonv.Array(jsonv.Num(1), jsonv.Str("x"))
	if got := compact([]jsonv.Value{a}); got != `[1,"x"]` {
		t.Errorf("Array: %s", got)
	}
	if v, ok := a.At(1); !ok || compact([]jsonv.Value{v}) != `"x"` {
		t.Errorf("At(1) = %v, %v", v, ok)
	}
	if _, ok := a.At(2); ok {
		t.Error("At(2) in range")
	}
	if _, ok := a.At(-1); ok {
		t.Error("At(-1) in range")
	}
	if _, ok := jsonv.Null().At(0); ok {
		t.Error("At on null")
	}
	if a.Len() != 2 || len(a.Elems()) != 2 {
		t.Errorf("Len = %d", a.Len())
	}
}

func TestMutatorsRejectWrongKind(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Set on the wrong kind did not panic")
			}
		}()
		v := jsonv.Num(1)
		v.Set("a", jsonv.Null())
	}()
}

func TestTruthyAndAlt(t *testing.T) {
	def := jsonv.Str("d")
	defStr := compact([]jsonv.Value{def})
	for src, want := range map[string]bool{"null": false, "false": false, "true": true, "0": true, `""`: true, "[]": true, "{}": true} {
		v := one(t, src)
		if v.Truthy() != want {
			t.Errorf("Truthy(%s) = %v", src, v.Truthy())
		}
		if got, isDef := compact([]jsonv.Value{v.Or(def)}), compact([]jsonv.Value{v.Or(def)}) == defStr; isDef == want {
			t.Errorf("%s // \"d\" = %s", src, got)
		}
	}
}

func TestExtraction(t *testing.T) {
	if s, ok := jsonv.Str("x").AsString(); !ok || s != "x" {
		t.Errorf("AsString = %q, %v", s, ok)
	}
	if _, ok := jsonv.Num(1).AsString(); ok {
		t.Error("AsString on a number")
	}
	if f, ok := one(t, "1.50").AsFloat(); !ok || f != 1.5 {
		t.Errorf("AsFloat = %v, %v", f, ok)
	}
	if _, ok := jsonv.Str("1").AsFloat(); ok {
		t.Error("AsFloat on a string")
	}
	if s := one(t, `1.0E+3`).NumberText(); s != "1.0E+3" {
		t.Errorf("NumberText = %q, want 1.0E+3", s)
	}
	if s := jsonv.Num(1).NumberText(); s != "" {
		t.Errorf("NumberText on a computed number = %q, want empty", s)
	}
	if v, ok := jsonv.ParseNumber("12345678901234567891"); !ok || string(jsonv.Append(nil, v, jsonv.Options{})) != "12345678901234567891" {
		t.Errorf("ParseNumber round-trip: %v %v", v, ok)
	}
	for src, want := range map[string]string{"null": "null", "false": "boolean", "true": "boolean", "1": "number", `""`: "string", "[]": "array", "{}": "object"} {
		if got := one(t, src).Kind().String(); got != want {
			t.Errorf("type of %s = %s, want %s", src, got, want)
		}
	}
}

func TestToAny(t *testing.T) {
	for _, lit := range []string{"1.50", "12345678901234567890123"} {
		if got := jsonv.ToAny(one(t, lit)); got != json.Number(lit) {
			t.Errorf("ToAny(%s) = %#v, want json.Number", lit, got)
		}
	}
	for _, tc := range []struct {
		src  string
		want func(float64) bool
	}{
		{"NaN", math.IsNaN},
		{"Infinity", func(f float64) bool { return math.IsInf(f, 1) }},
		{"-Infinity", func(f float64) bool { return math.IsInf(f, -1) }},
	} {
		f, ok := jsonv.ToAny(one(t, tc.src)).(float64)
		if !ok || !tc.want(f) {
			t.Errorf("ToAny(%s) = %v (float64 %v)", tc.src, f, ok)
		}
	}

	got := jsonv.ToAny(one(t, `{"a":[1,{"b":null}],"s":"x","t":true,"f":false}`))
	m, ok := got.(map[string]any)
	if !ok || len(m) != 4 {
		t.Fatalf("object = %#v", got)
	}
	arr, ok := m["a"].([]any)
	if !ok || len(arr) != 2 || arr[0] != json.Number("1") {
		t.Fatalf("a = %#v", m["a"])
	}
	inner, ok := arr[1].(map[string]any)
	if v, has := inner["b"]; !ok || !has || v != nil {
		t.Errorf("a[1] = %#v", arr[1])
	}
	if m["s"] != "x" || m["t"] != true || m["f"] != false {
		t.Errorf("leaves = %#v", m)
	}
}
