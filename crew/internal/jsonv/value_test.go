package jsonv_test

import (
	"errors"
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
	o.Delete("b")
	o.Delete("missing")
	if got := compact([]jsonv.Value{o}); got != `{"a":"x","c":null}` {
		t.Errorf("Delete: %s", got)
	}
	if v, ok := o.Get("c"); !ok || !v.IsNull() {
		t.Errorf("Get(c) = %v, %v", v, ok)
	}
	if _, ok := o.Get("b"); ok {
		t.Error("Get(b) found a deleted key")
	}
	if o.Len() != 2 || len(o.Members()) != 2 {
		t.Errorf("Len = %d", o.Len())
	}
}

func TestArrayAccess(t *testing.T) {
	a := jsonv.Array(jsonv.Num(1))
	a.Push(jsonv.Str("x"))
	if got := compact([]jsonv.Value{a}); got != `[1,"x"]` {
		t.Errorf("Push: %s", got)
	}
	if v, ok := a.At(1); !ok || !v.Equal(jsonv.Str("x")) {
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
	for name, f := range map[string]func(){
		"Set":    func() { v := jsonv.Num(1); v.Set("a", jsonv.Null()) },
		"Delete": func() { v := jsonv.Array(); v.Delete("a") },
		"Push":   func() { v := jsonv.Object(); v.Push(jsonv.Null()) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s on the wrong kind did not panic", name)
				}
			}()
			f()
		}()
	}
}

func TestIndex(t *testing.T) {
	o := one(t, `{"a":1,"b":null}`)
	for key, want := range map[string]string{"a": "1", "b": "null", "missing": "null"} {
		v, err := o.Index(key)
		if err != nil || compact([]jsonv.Value{v}) != want {
			t.Errorf("Index(%q) = %v, %v; want %s", key, v, err, want)
		}
	}
	if v, err := jsonv.Null().Index("a"); err != nil || !v.IsNull() {
		t.Errorf("null.a = %v, %v", v, err)
	}
	for _, src := range []string{`1`, `"s"`, `true`, `false`, `[]`} {
		var te *jsonv.TypeError
		if _, err := one(t, src).Index("a"); !errors.As(err, &te) {
			t.Errorf("%s.a: want TypeError, got %v", src, err)
		}
	}
}

func TestSliceString(t *testing.T) {
	tests := []struct {
		in       string
		from, to int
		want     string
		bad      bool
	}{
		{`"héllo"`, 0, 2, `"hé"`, false},
		{`"héllo"`, 0, 120, `"héllo"`, false},
		{`"😀😀😀"`, 0, 2, `"😀😀"`, false},
		{`"abc"`, 5, 9, `""`, false},
		{`"abc"`, -2, 3, `"bc"`, false},
		{`"abc"`, 2, 1, `""`, false},
		{`[1,2,3]`, 0, 2, `[1,2]`, false},
		{`[1,2,3]`, 1, 120, `[2,3]`, false},
		{`null`, 0, 120, `null`, false},
		{`5`, 0, 120, ``, true},
		{`true`, 0, 120, ``, true},
		{`{}`, 0, 120, ``, true},
		{`false`, 0, 120, ``, true},
	}
	for _, tc := range tests {
		got, err := one(t, tc.in).SliceString(tc.from, tc.to)
		var te *jsonv.TypeError
		if tc.bad {
			if !errors.As(err, &te) {
				t.Errorf("%s[%d:%d]: want TypeError, got %v", tc.in, tc.from, tc.to, err)
			}
			continue
		}
		if err != nil || compact([]jsonv.Value{got}) != tc.want {
			t.Errorf("%s[%d:%d] = %v, %v; want %s", tc.in, tc.from, tc.to, got, err, tc.want)
		}
	}
}

func TestTruthyAndAlt(t *testing.T) {
	def := jsonv.Str("d")
	for src, want := range map[string]bool{"null": false, "false": false, "true": true, "0": true, `""`: true, "[]": true, "{}": true} {
		v := one(t, src)
		if v.Truthy() != want {
			t.Errorf("Truthy(%s) = %v", src, v.Truthy())
		}
		if got := v.Or(def); got.Equal(def) == want {
			t.Errorf("%s // \"d\" = %v", src, got)
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
	for src, want := range map[string]string{"null": "null", "false": "boolean", "true": "boolean", "1": "number", `""`: "string", "[]": "array", "{}": "object"} {
		if got := one(t, src).Kind().String(); got != want {
			t.Errorf("type of %s = %s, want %s", src, got, want)
		}
	}
}
