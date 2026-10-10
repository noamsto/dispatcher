package jqrun

import (
	"math"
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
	g, err := toGo(v)
	if err != nil {
		t.Fatal(err)
	}
	f, isFloat := g.(float64)
	if !isFloat || !math.IsNaN(f) {
		t.Errorf("toGo(NaN) = %#v", g)
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
