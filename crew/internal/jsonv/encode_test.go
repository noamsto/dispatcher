package jsonv_test

import (
	"bytes"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

const customPalette = "4;31:0;35:0;36:1;33:0;34:1;35:1;36:4;32" // keep in sync with gen.sh

func encodeAll(vs []jsonv.Value, opts jsonv.Options) string {
	var sb strings.Builder
	for _, v := range vs {
		sb.Write(jsonv.Append(nil, v, opts))
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestEncodeGoldenDocs(t *testing.T) {
	def := jsonv.DefaultPalette()
	custom, ok := jsonv.ParseJQColors(customPalette)
	if !ok {
		t.Fatal("custom palette rejected")
	}
	variants := []struct {
		ext  string
		opts jsonv.Options
	}{
		{".pretty", jsonv.Options{Indent: true}},
		{".compact", jsonv.Options{}},
		{".color", jsonv.Options{Indent: true, Colors: &def}},
		{".colorc", jsonv.Options{Colors: &def}},
		{".custom", jsonv.Options{Colors: &custom}},
	}
	docs, err := filepath.Glob("testdata/docs/*.json")
	if err != nil || len(docs) == 0 {
		t.Fatalf("no docs: %v", err)
	}
	for _, doc := range docs {
		vs := decode(t, readFile(t, doc))
		base := strings.TrimSuffix(doc, ".json")
		for _, vt := range variants {
			t.Run(filepath.Base(base)+vt.ext, func(t *testing.T) {
				if got, want := encodeAll(vs, vt.opts), readFile(t, base+vt.ext); got != want {
					t.Errorf("mismatch\n got: %q\nwant: %q", got, want)
				}
			})
		}
	}
}

func TestEncodeLiteralGolden(t *testing.T) {
	in := strings.Split(strings.TrimSuffix(readFile(t, "testdata/literals.txt"), "\n"), "\n")
	out := strings.Split(strings.TrimSuffix(readFile(t, "testdata/literals.out"), "\n"), "\n")
	if len(in) != len(out) {
		t.Fatalf("%d inputs, %d outputs", len(in), len(out))
	}
	for i, lit := range in {
		t.Run(lit, func(t *testing.T) {
			if got := compact(decode(t, lit)); got != out[i] {
				t.Errorf("got %q, want %q", got, out[i])
			}
		})
	}
}

func TestFormatComputedGolden(t *testing.T) {
	rows := strings.Split(strings.TrimSuffix(readFile(t, "testdata/computed.tsv"), "\n"), "\n")
	for _, row := range rows {
		cols := strings.Split(row, "\t")
		if len(cols) != 3 {
			t.Fatalf("bad row %q", row)
		}
		t.Run(cols[1], func(t *testing.T) {
			x, err := strconv.ParseFloat(cols[0], 64)
			if err != nil {
				t.Fatal(err)
			}
			if got := jsonv.FormatComputed(x); got != cols[2] {
				t.Errorf("FormatComputed(%v) = %q, want %q", x, got, cols[2])
			}
			if got := string(jsonv.Append(nil, jsonv.Num(x), jsonv.Options{})); got != cols[2] {
				t.Errorf("Append(Num(%v)) = %q, want %q", x, got, cols[2])
			}
		})
	}
}

func TestFormatComputedSpecials(t *testing.T) {
	tests := []struct {
		x    float64
		want string
	}{
		{math.NaN(), "null"},
		{math.Inf(1), "1.7976931348623157e+308"},
		{math.Inf(-1), "-1.7976931348623157e+308"},
		{math.Copysign(0, -1), "-0"},
		{0, "0"},
	}
	for _, tc := range tests {
		if got := jsonv.FormatComputed(tc.x); got != tc.want {
			t.Errorf("FormatComputed(%v) = %q, want %q", tc.x, got, tc.want)
		}
	}
}

func TestParseJQColorsGolden(t *testing.T) {
	const doc = `[null,false,true,1,"s",[],{},{"k":0}]`
	rows := strings.Split(strings.TrimSuffix(readFile(t, "testdata/jqcolors.tsv"), "\n"), "\n")
	for _, row := range rows {
		cols := strings.SplitN(row, "\t", 3)
		if len(cols) != 3 {
			t.Fatalf("bad row %q", row)
		}
		t.Run(cols[0], func(t *testing.T) {
			pal, ok := jsonv.ParseJQColors(cols[0])
			if wantOK := cols[1] == "1"; ok != wantOK {
				t.Errorf("ok = %v, want %v", ok, wantOK)
			}
			got := string(jsonv.Append(nil, decode(t, doc)[0], jsonv.Options{Colors: &pal}))
			if got != cols[2] {
				t.Errorf("got %q, want %q", got, cols[2])
			}
		})
	}
}

func TestParseJQColors(t *testing.T) {
	def := jsonv.DefaultPalette()
	with := func(set map[int]string) jsonv.Palette {
		p := def
		for i, s := range set {
			p[i] = s
		}
		return p
	}
	const (
		null  = 0
		fals  = 1
		obj   = 6
		empty = "\x1b[m"
	)
	tests := []struct {
		in   string
		want jsonv.Palette
		ok   bool
	}{
		{"", def, true},
		{"1;31", with(map[int]string{null: "\x1b[1;31m"}), true},
		{"1;31::", with(map[int]string{null: "\x1b[1;31m", fals: empty}), true},
		{":", with(map[int]string{null: empty}), true},
		{"0;31:bad", def, false},
		{"x", def, false},
		{"1 ;2", def, false},
		{"1:2:3:4:5:6:7:8:9", jsonv.Palette{"\x1b[1m", "\x1b[2m", "\x1b[3m", "\x1b[4m", "\x1b[5m", "\x1b[6m", "\x1b[7m", "\x1b[8m"}, true},
		{"1:2:3:4:5:6:7:8:x", jsonv.Palette{"\x1b[1m", "\x1b[2m", "\x1b[3m", "\x1b[4m", "\x1b[5m", "\x1b[6m", "\x1b[7m", "\x1b[8m"}, true},
		{strings.Repeat("4;31;", 8) + "4;31", with(map[int]string{null: "\x1b[" + strings.Repeat("4;31;", 8) + "4;31m"}), true},
		{"::::::1", with(map[int]string{null: empty, fals: empty, 2: empty, 3: empty, 4: empty, 5: empty, obj: "\x1b[1m"}), true},
	}
	for _, tc := range tests {
		got, ok := jsonv.ParseJQColors(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseJQColors(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEncodeBuiltValues(t *testing.T) {
	tests := []struct {
		name string
		v    jsonv.Value
		opts jsonv.Options
		want string
	}{
		{"computed in array", jsonv.Array(jsonv.Num(1e16), jsonv.Num(0.5), jsonv.Num(math.Copysign(0, -1))), jsonv.Options{}, `[1e+16,0.5,-0]`},
		{"invalid UTF-8 is repaired on construction", jsonv.Array(jsonv.Str("a\xffb")), jsonv.Options{}, "[\"a�b\"]"},
		{"empty pretty containers", jsonv.Object(jsonv.Member{Key: "a", Val: jsonv.Array()}, jsonv.Member{Key: "b", Val: jsonv.Object()}), jsonv.Options{Indent: true}, "{\n  \"a\": [],\n  \"b\": {}\n}"},
		{"string with every short escape", jsonv.Str("\b\f\t\n\r\"\\\x01\x7f"), jsonv.Options{}, `"\b\f\t\n\r\"\\\u0001\u007f"`},
	}
	for _, tc := range tests {
		var buf bytes.Buffer
		if err := jsonv.Encode(&buf, tc.v, tc.opts); err != nil {
			t.Fatal(err)
		}
		if buf.String() != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, buf.String(), tc.want)
		}
	}
}

func TestEncodeWriteError(t *testing.T) {
	if err := jsonv.Encode(errWriter{}, jsonv.Null(), jsonv.Options{}); err == nil {
		t.Error("want the writer's error")
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errBoom }
