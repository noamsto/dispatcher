package jqrun

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// decode parses a stream of JSON values the way crew reads its inputs:
// numbers stay json.Number.
func decode(t *testing.T, s string) []any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var out []any
	for dec.More() {
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func compact(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunOverridesNow(t *testing.T) {
	out, err := RunAny(`{got: .[0].a, b: $b, m: $m.x, age: (now - 10)}`,
		decode(t, `{"a": 1}`), 42,
		map[string]any{"b": "br", "m": map[string]any{"x": "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(t, out); got != `{"age":32,"b":"br","got":1,"m":"y"}` {
		t.Errorf("got %s", got)
	}
}

func TestRunNowKeepsStringLiterals(t *testing.T) {
	out, err := RunAny(`{msg: "right now", n: now}`, decode(t, `{}`), 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(t, out); got != `{"msg":"right now","n":42}` {
		t.Errorf("got %s", got)
	}
}

func TestRunReturnsRuntimeError(t *testing.T) {
	_, err := RunAny(`.[0].a`, decode(t, `5`), 0, nil)
	if err == nil {
		t.Fatal("want the jq type error")
	}
}

func TestRunKeepsWordsAndComments(t *testing.T) {
	// The freeze touches only the parsed AST: `last-known` in a comment and
	// `now` in a string literal survive verbatim, the bare call is frozen.
	out, err := RunAny("# last-known now\n.[0] | {age: ((now*1000) - .ts), known: \"known\", msg: \"right now\"}\n", decode(t, `{"ts":1}`), 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(t, out); got != `{"age":41999,"known":"known","msg":"right now"}` {
		t.Errorf("got %s", got)
	}
}

func TestNumberLiteralsRoundTripExactly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"v":12345678901234567891}`, `{"v":12345678901234567891}`}, // beyond float64
		{`{"v":1699999000000.50}`, `{"v":1699999000000.50}`},
		{`{"v":1e1000}`, `{"v":1e1000}`},
		{`{"v":1700000000000}`, `{"v":1700000000000}`},
	} {
		out, err := RunAny(`.[0]`, decode(t, tc.in), 0, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got := compact(t, out); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestComputedNumbersStayComputed(t *testing.T) {
	out, err := RunAny(`.[0].v | floor`, decode(t, `{"v":1.7}`), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(t, out); got != "1" {
		t.Errorf("got %s", got)
	}
	out, err = RunAny(`.[0].big | now - .`, decode(t, `{"big":1e1000}`), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := out.(float64); !ok || !math.IsInf(f, -1) {
		t.Errorf("got %#v, want float64 -Inf", out)
	}
}

func TestJQFromJSON(t *testing.T) {
	out, err := RunAny(`.[0] | _jqfromjson`, []any{`{"n":12345678901234567891,"s":"x"}`}, 0, nil, WithJQFromJSON())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"n": json.Number("12345678901234567891"), "s": "x"}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("got %#v, want %#v", out, want)
	}

	out, err = RunAny(`.[0] | _jqfromjson`, []any{"\xef\xbb\xbf" + `{"a":1}`}, 0, nil, WithJQFromJSON())
	if err != nil {
		t.Fatalf("BOM-prefixed text: %v", err)
	}
	if want := map[string]any{"a": json.Number("1")}; !reflect.DeepEqual(out, want) {
		t.Errorf("BOM-prefixed text: got %#v, want %#v", out, want)
	}
}

func TestJQFromJSONRejects(t *testing.T) {
	for name, in := range map[string]any{
		"trailing data": `1 2`,
		"empty":         ``,
		"invalid":       `{"a":`,
		"NaN literal":   `{"n":NaN}`,
		"non-string":    json.Number("5"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RunAny(`.[0] | _jqfromjson`, []any{in}, 0, nil, WithJQFromJSON()); err == nil {
				t.Error("want an error")
			}
			out, err := RunAny(`[.[0] | try _jqfromjson catch "err"]`, []any{in}, 0, nil, WithJQFromJSON())
			if err != nil {
				t.Fatal(err)
			}
			if got := compact(t, out); got != `["err"]` {
				t.Errorf("got %s", got)
			}
			out, err = RunAny(`[.[0] | _jqfromjson?]`, []any{in}, 0, nil, WithJQFromJSON())
			if err != nil {
				t.Fatal(err)
			}
			if got := compact(t, out); got != `[]` {
				t.Errorf("got %s", got)
			}
		})
	}
}

func TestJQFromJSONIsOptIn(t *testing.T) {
	if _, err := RunAny(`"1" | _jqfromjson`, nil, 0, nil); err == nil {
		t.Error("_jqfromjson defined without WithJQFromJSON")
	}
}

func TestRunAnyForm(t *testing.T) {
	nan, negInf := math.NaN(), math.Inf(-1)
	for _, tc := range []struct {
		name  string
		prog  string
		input []any
		now   float64
		want  any
	}{
		{name: "fraction literal", prog: `.[0].v`, input: []any{map[string]any{"v": json.Number("1699999000000.50")}}, want: json.Number("1699999000000.50")},
		{name: "big int", prog: `.[0].v`, input: []any{map[string]any{"v": json.Number("12345678901234567891")}}, want: json.Number("12345678901234567891")},
		{name: "computed NaN", prog: `.[0].v | . - .`, input: []any{map[string]any{"v": nan}}, want: nan},
		{name: "-Inf passthrough", prog: `.[0].v`, input: []any{map[string]any{"v": negInf}}, want: negInf},
		{name: "huge exponent", prog: `.[0].v`, input: []any{map[string]any{"v": json.Number("1e1000")}}, want: json.Number("1e1000")},
		{name: "integer result", prog: `now - 10`, now: 42, want: json.Number("32")},
		{name: "nested passthrough", prog: `.[0]`,
			input: []any{map[string]any{"a": []any{json.Number("1"), "s", nil, true, map[string]any{"k": 2.5}}}},
			want:  map[string]any{"a": []any{json.Number("1"), "s", nil, true, map[string]any{"k": 2.5}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RunAny(tc.prog, tc.input, tc.now, nil)
			if err != nil {
				t.Fatal(err)
			}
			if f, ok := tc.want.(float64); ok && math.IsNaN(f) {
				if g, ok := got.(float64); !ok || !math.IsNaN(g) {
					t.Errorf("got %#v, want float64 NaN", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}
