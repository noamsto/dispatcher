package jsonv_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

func one(t *testing.T, src string) jsonv.Value {
	t.Helper()
	vs := decode(t, src)
	if len(vs) != 1 {
		t.Fatalf("%q: %d values, want 1", src, len(vs))
	}
	return vs[0]
}

func TestCompareGolden(t *testing.T) {
	rows := strings.Split(strings.TrimSuffix(readFile(t, "testdata/compare.tsv"), "\n"), "\n")
	for _, row := range rows {
		cols := strings.Split(row, "\t")
		if len(cols) != 3 {
			t.Fatalf("bad row %q", row)
		}
		t.Run(cols[0]+" vs "+cols[1], func(t *testing.T) {
			a, b := one(t, cols[0]), one(t, cols[1])
			want := map[string]int{"-1": -1, "0": 0, "1": 1}[cols[2]]
			if got := jsonv.Compare(a, b); got != want {
				t.Errorf("Compare(a, b) = %d, want %d", got, want)
			}
			if got := jsonv.Compare(b, a); got != -want {
				t.Errorf("Compare(b, a) = %d, want %d", got, -want)
			}
			if got := a.Equal(b); got != (want == 0) {
				t.Errorf("Equal = %v, want %v", got, want == 0)
			}
		})
	}
}

func TestCompareTypeOrder(t *testing.T) {
	ordered := []jsonv.Value{
		jsonv.Null(), jsonv.Bool(false), jsonv.Bool(true), jsonv.Num(-1), jsonv.Num(0),
		jsonv.Str(""), jsonv.Str("a"), jsonv.Array(), jsonv.Array(jsonv.Null()), jsonv.Object(),
		jsonv.Object(jsonv.Member{Key: "a", Val: jsonv.Null()}),
	}
	for i, a := range ordered {
		for j, b := range ordered {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := jsonv.Compare(a, b); got != want {
				t.Errorf("Compare(#%d, #%d) = %d, want %d", i, j, got, want)
			}
		}
	}
}

func TestCompareNumbers(t *testing.T) {
	tests := []struct {
		name string
		a, b jsonv.Value
		want int
	}{
		{"big literals differ exactly", one(t, "12345678901234567891"), one(t, "12345678901234567890"), 1},
		{"1.0 equals 1", one(t, "1.0"), one(t, "1"), 0},
		{"literal against computed falls back to float", one(t, "12345678901234567891"), jsonv.Num(12345678901234567890), 0},
		{"computed pair", jsonv.Num(12345678901234567891), jsonv.Num(12345678901234567890), 0},
		{"computed less", jsonv.Num(0.1), jsonv.Num(0.2), -1},
		{"literal against computed less", one(t, "1.5"), jsonv.Num(2), -1},
		{"nan is below everything", jsonv.Num(math.NaN()), jsonv.Num(math.Inf(-1)), -1},
		{"nan is below itself", jsonv.Num(math.NaN()), jsonv.Num(math.NaN()), -1},
		{"huge exponents differ", one(t, "1e1000"), one(t, "1e1001"), -1},
		{"negative zero equals zero", one(t, "-0"), one(t, "0"), 0},
		{"trailing zeros are equal", one(t, "0.10"), one(t, "0.1"), 0},
		{"digits longer by non-zero", one(t, "1.0000000000000000000001"), one(t, "1"), 1},
		{"negatives reverse", one(t, "-12345678901234567891"), one(t, "-12345678901234567890"), -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonv.Compare(tc.a, tc.b); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func byK(v jsonv.Value) (jsonv.Value, error) { return v.Index("k") }

func ids(vs []jsonv.Value) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		id, err := v.Index("id")
		if err != nil {
			panic(err)
		}
		parts[i] = string(jsonv.Append(nil, id, jsonv.Options{}))
	}
	return strings.Join(parts, ",")
}

const rows = `[{"id":1,"k":2},{"id":2,"k":1},{"id":3,"k":2},{"id":4,"k":1},{"id":5,"k":null},{"id":6,"k":2.0}]`

func TestSortBy(t *testing.T) {
	in := one(t, rows).Elems()
	got, err := jsonv.SortBy(in, byK)
	if err != nil {
		t.Fatal(err)
	}
	if want := "5,2,4,1,3,6"; ids(got) != want {
		t.Errorf("sorted ids %s, want %s (stable, null first)", ids(got), want)
	}
	if ids(in) != "1,2,3,4,5,6" {
		t.Errorf("input mutated: %s", ids(in))
	}
}

func TestGroupBy(t *testing.T) {
	got, err := jsonv.GroupBy(one(t, rows).Elems(), byK)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, g := range got {
		parts = append(parts, ids(g))
	}
	if want := "5|2,4|1,3,6"; strings.Join(parts, "|") != want {
		t.Errorf("groups %s, want %s", strings.Join(parts, "|"), want)
	}
}

func TestMaxMinBy(t *testing.T) {
	in := one(t, rows).Elems()
	max, err := jsonv.MaxBy(in, byK)
	if err != nil {
		t.Fatal(err)
	}
	if ids([]jsonv.Value{max}) != "6" {
		t.Errorf("MaxBy tie should be the last: %s", ids([]jsonv.Value{max}))
	}
	min, err := jsonv.MinBy(in[1:4], byK)
	if err != nil {
		t.Fatal(err)
	}
	if ids([]jsonv.Value{min}) != "2" {
		t.Errorf("MinBy tie should be the first: %s", ids([]jsonv.Value{min}))
	}
	for name, f := range map[string]func([]jsonv.Value, func(jsonv.Value) (jsonv.Value, error)) (jsonv.Value, error){"MaxBy": jsonv.MaxBy, "MinBy": jsonv.MinBy} {
		got, err := f(nil, byK)
		if err != nil || !got.IsNull() {
			t.Errorf("%s of nothing = %v, %v; want null", name, got, err)
		}
	}
}

func TestByPropagatesKeyErrors(t *testing.T) {
	in := decode(t, `{"k":1} 5`)
	var te *jsonv.TypeError
	if _, err := jsonv.SortBy(in, byK); !errorsAs(err, &te) {
		t.Errorf("SortBy: want TypeError, got %v", err)
	}
	if _, err := jsonv.GroupBy(in, byK); !errorsAs(err, &te) {
		t.Errorf("GroupBy: want TypeError, got %v", err)
	}
	if _, err := jsonv.MaxBy(in, byK); !errorsAs(err, &te) {
		t.Errorf("MaxBy: want TypeError, got %v", err)
	}
	if _, err := jsonv.MinBy(in, byK); !errorsAs(err, &te) {
		t.Errorf("MinBy: want TypeError, got %v", err)
	}
}

func TestUnique(t *testing.T) {
	tests := []struct{ in, want string }{
		{`3 1 2 1 3`, `1 2 3`},
		{`"b" "a" "b" null`, `null "a" "b"`},
		{`1.0 1`, `1.0`},
		{`1 1.0`, `1`},
		{`{"a":1,"b":2} {"b":2,"a":1}`, `{"a":1,"b":2}`},
		{`12345678901234567891 12345678901234567890`, `12345678901234567890 12345678901234567891`},
		{``, ``},
	}
	for _, tc := range tests {
		got := compact(jsonv.Unique(decode(t, tc.in)))
		if got != tc.want {
			t.Errorf("Unique(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func errorsAs(err error, target **jsonv.TypeError) bool { return errors.As(err, target) }

func TestCompareLargeObjectsIsLinearish(t *testing.T) {
	const n = 200_000
	a := one(t, bigObject(n, false))
	b := one(t, bigObject(n, true))
	start := time.Now()
	if c := jsonv.Compare(a, b); c != 0 {
		t.Errorf("same members in another order compare %d, want 0", c)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("comparing %d-key objects took %v, want well under 2s", n, d)
	}
}
