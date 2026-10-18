package jsonval

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestEncode(t *testing.T) {
	nan, inf, ninf := math.NaN(), math.Inf(1), math.Inf(-1)
	const max = "1.7976931348623157e+308"
	tests := []struct {
		name   string
		in     any
		indent bool
		want   string
	}{
		{"nan", nan, false, "null\n"},
		{"+inf", inf, false, max + "\n"},
		{"-inf", ninf, false, "-" + max + "\n"},
		{"finite", 1.5, false, "1.5\n"},
		{"array", []any{nan, inf, 2.0}, false, "[null," + max + ",2]\n"},
		{"map", map[string]any{"a": nan, "b": ninf}, false, `{"a":null,"b":-` + max + "}\n"},
		{"nested", map[string]any{"a": []any{map[string]any{"b": inf}}}, false, `{"a":[{"b":` + max + "}]}\n"},
		{"number 1.50", json.Number("1.50"), false, "1.50\n"},
		{"big int", json.Number("123456789012345678901234567890"), false, "123456789012345678901234567890\n"},
		{"huge exponent", json.Number("1E+1000"), false, "1E+1000\n"},
		{"number in array", []any{json.Number("1.50"), nan}, false, "[1.50,null]\n"},
		{"html unescaped", "<&>", false, "\"<&>\"\n"},
		{"html in map", map[string]any{"k<": "a&b>"}, false, `{"k<":"a&b>"}` + "\n"},
		{"indent", map[string]any{"a": []any{nan, 1.0}}, true, "{\n  \"a\": [\n    null,\n    1\n  ]\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := Encode(&buf, tt.in, tt.indent); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestText(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{math.NaN(), "null"},
		{math.Inf(1), "1.7976931348623157e+308"},
		{math.Inf(-1), "-1.7976931348623157e+308"},
		{1.5, "1.5"},
		{0, "0"},
		{1e21, "1e+21"},
		{-2, "-2"},
	}
	for _, tt := range tests {
		if got := Text(tt.in); got != tt.want {
			t.Errorf("Text(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestClampDoesNotMutate(t *testing.T) {
	in := map[string]any{"a": []any{math.NaN(), math.Inf(1)}, "b": map[string]any{"c": math.Inf(-1)}}
	got := Clamp(in)
	if !math.IsNaN(in["a"].([]any)[0].(float64)) || !math.IsInf(in["a"].([]any)[1].(float64), 1) ||
		!math.IsInf(in["b"].(map[string]any)["c"].(float64), -1) {
		t.Errorf("input mutated: %v", in)
	}
	want := map[string]any{"a": []any{nil, math.MaxFloat64}, "b": map[string]any{"c": -math.MaxFloat64}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []any{true, 0.0, "", json.Number("0"), []any{}} {
		if !Truthy(v) {
			t.Errorf("Truthy(%#v) = false", v)
		}
	}
	if Truthy(nil) || Truthy(false) {
		t.Error("null or false is truthy")
	}
}

func TestRawText(t *testing.T) {
	inf, nan := math.Inf(1), math.NaN()
	for _, c := range []struct {
		in   any
		want string
	}{
		{"a b", "a b"}, {nil, "null"}, {true, "true"}, {1e-7, "1e-07"}, {json.Number("73.0"), "73.0"},
		{inf, "1.7976931348623157e+308"}, {nan, "null"}, {[]any{inf, nan}, "[1.7976931348623157e+308,null]"},
		{map[string]any{"x": nan}, `{"x":null}`},
	} {
		if got := RawText(c.in); got != c.want {
			t.Errorf("RawText(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
}
