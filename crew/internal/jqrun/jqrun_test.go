package jqrun

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

func decode(t *testing.T, s string) []jsonv.Value {
	t.Helper()
	vs, err := jsonv.DecodeStream(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

func compact(v jsonv.Value) string { return string(jsonv.Append(nil, v, jsonv.Options{})) }

func TestRunOverridesNow(t *testing.T) {
	out, err := Run(`{got: .[0].a, b: $b, m: $m.x, age: (now - 10)}`,
		decode(t, `{"a": 1}`), 42,
		map[string]jsonv.Value{"b": jsonv.Str("br"), "m": jsonv.Object(jsonv.Member{Key: "x", Val: jsonv.Str("y")})})
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(out); got != `{"age":32,"b":"br","got":1,"m":"y"}` {
		t.Errorf("got %s", got)
	}
}

func TestRunNowKeepsStringLiterals(t *testing.T) {
	out, err := Run(`{msg: "right now", n: now}`, decode(t, `{}`), 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(out); got != `{"msg":"right now","n":42}` {
		t.Errorf("got %s", got)
	}
}

func TestRunReturnsRuntimeError(t *testing.T) {
	_, err := Run(`.[0].a`, decode(t, `5`), 0, nil)
	if err == nil {
		t.Fatal("want the jq type error")
	}
}

func TestRunKeepsWordsAndComments(t *testing.T) {
	// The freeze touches only the parsed AST: `last-known` in a comment and
	// `now` in a string literal survive verbatim, the bare call is frozen.
	out, err := Run("# last-known now\n.[0] | {age: ((now*1000) - .ts), known: \"known\", msg: \"right now\"}\n", decode(t, `{"ts":1}`), 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(out); got != `{"age":41999,"known":"known","msg":"right now"}` {
		t.Errorf("got %s", got)
	}
}

func TestNumberLiteralsRoundTripExactly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"v":12345678901234567891}`, `{"v":12345678901234567891}`}, // beyond float64
		{`{"v":1.0e3}`, `{"v":1.0E+3}`},
		{`{"v":1699999000000.50}`, `{"v":1699999000000.50}`},
		{`{"v":1e1000}`, `{"v":1E+1000}`},
		{`{"v":1700000000000}`, `{"v":1700000000000}`},
	} {
		out, err := Run(`.[0]`, decode(t, tc.in), 0, nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got := compact(out); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestComputedNumbersStayComputed(t *testing.T) {
	out, err := Run(`.[0].v | floor`, decode(t, `{"v":1.7}`), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(out); got != "1" {
		t.Errorf("got %s", got)
	}
	// NaN encodes as null and a computed infinity clamps, as jq prints them.
	out, err = Run(`.[0] | {n: .nan, i: (now - .big)}`, decode(t, `{"nan":NaN,"big":1e1000}`), 0, nil)
	if err == nil {
		if got := compact(out); got != `{"i":-1.7976931348623157e+308,"n":null}` {
			t.Errorf("got %s", got)
		}
		return
	}
	t.Fatal(err)
}

func TestFromGoSortsObjectKeys(t *testing.T) {
	out, err := Run(`.[0] as $x | {b: 1, a: $x.k} | to_entries | from_entries`, decode(t, `{"k":2}`), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(out); got != `{"a":2,"b":1}` {
		t.Errorf("got %s", got)
	}
}

func TestToGoNaNStaysFloat(t *testing.T) {
	v := jsonv.Num(math.NaN())
	g := jsonv.ToAny(v)
	f, isFloat := g.(float64)
	if !isFloat || !math.IsNaN(f) {
		t.Errorf("ToAny(NaN) = %#v", g)
	}
}

func TestJQFromJSON(t *testing.T) {
	prog := `.[] | [try _jqfromjson catch "err"], [_jqfromjson?], [(_jqfromjson? // null)]`
	in := []jsonv.Value{
		jsonv.Str(`{"a":"\udc00","n":-nan}`),
		jsonv.Str(`{"a":"\ud83d"}`),
		jsonv.Str(`1 2`),
		jsonv.Str(``),
		jsonv.Num(5),
	}
	out, err := Run(`[`+prog+`]`, in, 0, nil, WithJQFromJSON())
	if err != nil {
		t.Fatal(err)
	}
	want := `[[{"a":"` + "�" + `","n":null}],[{"a":"` + "�" + `","n":null}],[{"a":"` + "�" + `","n":null}],` +
		`["err"],[],[null],["err"],[],[null],["err"],[],[null],["err"],[],[null]]`
	if got := compact(out); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestJQFromJSONIsOptIn(t *testing.T) {
	if _, err := Run(`"1" | _jqfromjson`, nil, 0, nil); err == nil {
		t.Error("_jqfromjson defined without WithJQFromJSON")
	}
}

func toGoAll(vs []jsonv.Value) []any {
	if vs == nil {
		return nil
	}
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = jsonv.ToAny(v)
	}
	return out
}

func toGoVars(vars map[string]jsonv.Value) map[string]any {
	if vars == nil {
		return nil
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		out[k] = jsonv.ToAny(v)
	}
	return out
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, _, err := big.ParseFloat(n.String(), 10, 64, big.ToNearestEven)
		if err != nil {
			return 0, false
		}
		f64, _ := f.Float64()
		return f64, true
	}
	return 0, false
}

// valueEqual compares gojq-form values, treating json.Number and float64 as
// the same number when they denote the same value.
func valueEqual(a, b any) bool {
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !valueEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !valueEqual(xv, yv) {
				return false
			}
		}
		return true
	case json.Number, float64:
		return numEqual(a, b)
	default:
		return reflect.DeepEqual(a, b)
	}
}

func numEqual(a, b any) bool {
	_, aFloat := a.(float64)
	_, bFloat := b.(float64)
	if aFloat || bFloat {
		fa, okA := asFloat(a)
		fb, okB := asFloat(b)
		if !okA || !okB {
			return false
		}
		switch {
		case math.IsNaN(fa) || math.IsNaN(fb):
			return math.IsNaN(fa) && math.IsNaN(fb)
		case math.IsInf(fa, 0) || math.IsInf(fb, 0):
			return fa == fb
		}
	}
	ra, okA := numRat(a)
	rb, okB := numRat(b)
	return okA && okB && ra.Cmp(rb) == 0
}

func numRat(v any) (*big.Rat, bool) {
	r := new(big.Rat)
	switch n := v.(type) {
	case json.Number:
		return r.SetString(n.String())
	case float64:
		return r.SetFloat64(n), true
	}
	return nil, false
}

func onlyNormalised(v any) bool {
	switch t := v.(type) {
	case nil, bool, string, float64, json.Number:
		return true
	case []any:
		for _, e := range t {
			if !onlyNormalised(e) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, e := range t {
			if !onlyNormalised(e) {
				return false
			}
		}
		return true
	}
	return false
}

func TestAnyEntryPointsMatchJSONV(t *testing.T) {
	fromJSON := []jsonv.Value{
		jsonv.Str(`{"a":"\udc00","n":-nan}`),
		jsonv.Str(`{"a":"\ud83d"}`),
		jsonv.Str(`1 2`),
		jsonv.Str(``),
		jsonv.Num(5),
	}
	for _, tc := range []struct {
		name    string
		prog    string
		input   []jsonv.Value
		vars    map[string]jsonv.Value
		opts    []Option
		now     float64
		wantErr bool
	}{
		{name: "overrides now", prog: `{got: .[0].a, b: $b, m: $m.x, age: (now - 10)}`, input: decode(t, `{"a": 1}`), now: 42,
			vars: map[string]jsonv.Value{"b": jsonv.Str("br"), "m": jsonv.Object(jsonv.Member{Key: "x", Val: jsonv.Str("y")})}},
		{name: "now string literals", prog: `{msg: "right now", n: now}`, input: decode(t, `{}`), now: 42},
		{name: "runtime error", prog: `.[0].a`, input: decode(t, `5`), wantErr: true},
		{name: "words and comments", prog: "# last-known now\n.[0] | {age: ((now*1000) - .ts), known: \"known\", msg: \"right now\"}\n", input: decode(t, `{"ts":1}`), now: 42},
		{name: "big int", prog: `.[0]`, input: decode(t, `{"v":12345678901234567891}`)},
		{name: "exponent", prog: `.[0]`, input: decode(t, `{"v":1.0e3}`)},
		{name: "fraction", prog: `.[0]`, input: decode(t, `{"v":1699999000000.50}`)},
		{name: "huge exponent", prog: `.[0]`, input: decode(t, `{"v":1e1000}`)},
		{name: "plain int", prog: `.[0]`, input: decode(t, `{"v":1700000000000}`)},
		{name: "floor", prog: `.[0].v | floor`, input: decode(t, `{"v":1.7}`)},
		{name: "nan and inf", prog: `.[0] | {n: .nan, i: (now - .big), p: (.big * 2)}`, input: decode(t, `{"nan":NaN,"big":1e1000}`)},
		{name: "sorted keys", prog: `.[0] as $x | {b: 1, a: $x.k} | to_entries | from_entries`, input: decode(t, `{"k":2}`)},
		{name: "jqfromjson", prog: `[.[] | [try _jqfromjson catch "err"], [_jqfromjson?], [(_jqfromjson? // null)]]`, input: fromJSON, opts: []Option{WithJQFromJSON()}},
		{name: "jqfromjson opt-in", prog: `"1" | _jqfromjson`, wantErr: true},
		{name: "null passthrough", prog: `.[0]`, input: decode(t, `null`)},
		{name: "nil input slurp", prog: `length`},
		{name: "nested passthrough", prog: `.[0]`, input: decode(t, `{"z":1,"a":{"y":null,"b":2}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := Run(tc.prog, tc.input, tc.now, tc.vars, tc.opts...)
			if (wantErr != nil) != tc.wantErr {
				t.Fatalf("jsonv Run err = %v, wantErr %v", wantErr, tc.wantErr)
			}
			in, goVars := toGoAll(tc.input), toGoVars(tc.vars)
			viaRun, errRun := RunAny(tc.prog, in, tc.now, goVars, tc.opts...)
			var viaProg any
			p, errProg := CompileAny(tc.prog, tc.now, goVars, tc.opts...)
			if errProg == nil {
				viaProg, errProg = p.RunAny(jsonv.ToAny(jsonv.Array(tc.input...)))
			}
			if (errRun != nil) != tc.wantErr || (errProg != nil) != tc.wantErr {
				t.Fatalf("any errs = %v, %v; wantErr %v", errRun, errProg, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			wantGo := jsonv.ToAny(want)
			for name, got := range map[string]any{"RunAny": viaRun, "Program.RunAny": viaProg} {
				if !onlyNormalised(got) {
					t.Errorf("%s: non-normalised output %#v", name, got)
				}
				if !valueEqual(got, wantGo) {
					t.Errorf("%s: got %#v, want %#v", name, got, wantGo)
				}
			}
		})
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
