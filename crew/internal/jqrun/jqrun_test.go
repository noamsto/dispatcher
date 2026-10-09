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

func TestRunPassesVarsAndNow(t *testing.T) {
	out, err := Run(`{got: .[0].a, b: $b, m: $m.x, age: (now - 10)} `,
		decode(t, `{"a": 1}`), 42,
		map[string]jsonv.Value{"b": jsonv.Str("br"), "m": jsonv.Object(jsonv.Member{Key: "x", Val: jsonv.Str("y")})})
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(out); got != `{"age":32,"b":"br","got":1,"m":"y"}` {
		t.Errorf("got %s", got)
	}
}

func TestRunReturnsRuntimeError(t *testing.T) {
	_, err := Run(`.[0].a`, decode(t, `5`), 0, nil)
	if err == nil {
		t.Fatal("want the jq type error")
	}
}

func TestInjectNowKeepsWordsAndComments(t *testing.T) {
	got := injectNow("# last-known now\n| (now*1000) - .ts # known\n")
	if !strings.Contains(got, "($now*1000)") {
		t.Errorf("bare now not rewritten: %q", got)
	}
	for _, keep := range []string{"last-known", "known"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q was rewritten: %q", keep, got)
		}
	}
	// the comment's standalone now is rewritten too; comments are inert, so
	// that is harmless — what matters is that no identifier is split.
	if strings.Contains(got, "$nown") || strings.Contains(got, "k$now") {
		t.Errorf("identifier split: %q", got)
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
