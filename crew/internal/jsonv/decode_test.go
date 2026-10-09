package jsonv_test

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

func decode(t *testing.T, src string) []jsonv.Value {
	t.Helper()
	vs, err := jsonv.DecodeStream(strings.NewReader(src))
	if err != nil {
		t.Fatalf("DecodeStream(%q): %v", src, err)
	}
	return vs
}

func compact(vs []jsonv.Value) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = string(jsonv.Append(nil, v, jsonv.Options{}))
	}
	return strings.Join(parts, " ")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDecodeStream(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // compact re-encoding of the values, space separated
	}{
		{"empty", "", ""},
		{"whitespace only", " \t\r\n ", ""},
		{"blank lines", "\n\n", ""},
		{"two values on one line", `{"a":1}{"b":2}`, `{"a":1} {"b":2}`},
		{"one per line", "{\"a\":1}\n{\"b\":2}\n", `{"a":1} {"b":2}`},
		{"pretty multi-line object", "{\n  \"a\": [\n    1,\n    2\n  ]\n}\n", `{"a":[1,2]}`},
		{"bare scalars", `5 "s" null true false`, `5 "s" null true false`},
		{"no separator needed after a string", `"a"1`, `"a" 1`},
		{"duplicate keys keep first position, last value", `{"a":1,"b":2,"a":3}`, `{"a":3,"b":2}`},
		{"nested duplicate keys", `{"x":{"y":1,"y":2},"x":4,"w":0}`, `{"x":4,"w":0}`},
		{"empty containers", `[] {} [[]] {"a":{}}`, `[] {} [[]] {"a":{}}`},
		{"escapes decode", `"\b\f\n\r\t\"\\\/Aé"`, `"\b\f\n\r\t\"\\/Aé"`},
		{"surrogate pair", `"😀"`, `"😀"`},
		{"lone low surrogate is U+FFFD", `"\udc00"`, "\"�\""},
		{"invalid byte becomes U+FFFD", "\"a\xffb\"", "\"a�b\""},
		{"invalid sequence consumed as a unit", "\"\xed\xa0\x80\"", "\"�\""},
		{"truncated sequence at string end swallows its tail", "\"\xe2A\"", "\"�\""},
		{"bad continuation keeps the next byte", "\"\xe2\x82A\"", "\"�A\""},
		{"invalid key bytes", "{\"k\xff\":1}", "{\"k�\":1}"},
		{"BOM at stream start", "\xef\xbb\xbf1", "1"},
		{"leading zeros", "01 00.5", "1 0.5"},
		{"plus sign and bare dot", "+1 .5 1.", "1 0.5 1"},
		{"nan is null", "nan NaN -nan", "null null null"},
		{"infinity is the largest double", "Infinity -Infinity", "1.7976931348623157e+308 -1.7976931348623157e+308"},
		{"huge exponent literal survives", "1e1000", "1E+1000"},
		{"exponent past decNumber's limit is infinite", "1e1000000000 -12e999999999 1e99999999999999999999", "1.7976931348623157e+308 -1.7976931348623157e+308 1.7976931348623157e+308"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compact(decode(t, tc.in)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeStreamCount(t *testing.T) {
	for in, want := range map[string]int{"": 0, " \n\n": 0, "1 2 3": 3, "[1][2]": 2} {
		if got := len(decode(t, in)); got != want {
			t.Errorf("%q: %d values, want %d", in, got, want)
		}
	}
}

func TestDecodeStreamErrors(t *testing.T) {
	tests := []struct{ name, in string }{
		{"torn tail", `{"ts":17`},
		{"torn string", `{"ts":"17`},
		{"torn array", `[1,`},
		{"garbage", `garbage`},
		{"garbage after a value", `{"a":1} garbage`},
		{"trailing comma in array", `[1,]`},
		{"trailing comma in object", `{"a":1,}`},
		{"leading comma", `[,1]`},
		{"top-level comma", `1,2`},
		{"missing separator in array", `[1 2]`},
		{"missing separator between literals", `[true false]`},
		{"missing colon", `{"a" 1}`},
		{"non-string key", `{1:2}`},
		{"glued literals", `truefalse`},
		{"partial literal", `tru`},
		{"unmatched close", `[]]`},
		{"unmatched brace", `{}}`},
		{"single quotes", `'a'`},
		{"bare minus", `-`},
		{"dangling exponent", `1e`},
		{"hex", `0x10`},
		{"double dot", `1.5.5`},
		{"subtraction", `1-1`},
		{"raw tab in string", "\"a\tb\""},
		{"raw newline in string", "\"a\nb\""},
		{"bad escape", `"\x"`},
		{"short unicode escape", `"\u12"`},
		{"lone high surrogate", `"\ud800 x"`},
		{"high surrogate then non-surrogate escape", `"\ud83dA"`},
		{"high surrogate then high", `"\ud83d😀"`},
		{"low then high", `"\udc00\ud800"`},
		{"RS control byte", "\x1e1"},
		{"depth 10001 arrays", strings.Repeat("[", 10001) + strings.Repeat("]", 10001)},
		{"depth 5001 objects", strings.Repeat(`{"a":`, 5001) + "1" + strings.Repeat("}", 5001)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := jsonv.DecodeStream(strings.NewReader(tc.in))
			if err == nil {
				t.Fatalf("want error, got %q", compact(vs))
			}
			var se *jsonv.SyntaxError
			if !errors.As(err, &se) {
				t.Errorf("want *SyntaxError, got %T: %v", err, err)
			}
		})
	}
}

func TestDecodeStreamDepthLimit(t *testing.T) {
	tests := []struct{ name, in string }{
		{"10000 arrays", strings.Repeat("[", 10000) + strings.Repeat("]", 10000)},
		{"5000 objects", strings.Repeat(`{"a":`, 5000) + "1" + strings.Repeat("}", 5000)},
		{"9999 arrays around an object with a scalar", strings.Repeat("[", 9999) + `{"a":1}` + strings.Repeat("]", 9999)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(decode(t, tc.in)); got != 1 {
				t.Errorf("%d values, want 1", got)
			}
		})
	}
}

func TestDecodeStreamReadError(t *testing.T) {
	if _, err := jsonv.DecodeStream(errReader{errBoom}); !errors.Is(err, errBoom) {
		t.Errorf("got %v, want the reader's error", err)
	}
}

var errBoom = errors.New("boom")

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// bigObject renders {"k0":0,...,"k<n-1>":n-1} with the keys in the given order.
func bigObject(n int, reverse bool) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i := range n {
		k := i
		if reverse {
			k = n - 1 - i
		}
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"k%d":%d`, k, k)
	}
	sb.WriteByte('}')
	return sb.String()
}

func TestDecodeStreamManyKeysIsLinearish(t *testing.T) {
	const n = 200_000
	src := bigObject(n, false)
	start := time.Now()
	vs := decode(t, src)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("decoding %d unique keys took %v, want well under 2s", n, d)
	}
	if got := vs[0].Len(); got != n {
		t.Errorf("%d members, want %d", got, n)
	}
}

func TestDecodeStreamDuplicateKeysKeepFirstPositionLastValue(t *testing.T) {
	got := compact(decode(t, `{"a":1,"b":2,"a":3,"c":4,"b":5}`))
	if want := `{"a":3,"b":5,"c":4}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestDecodeStreamPrefix(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		count   int // values returned
		wantErr bool
	}{
		{"clean", `{"a":1} {"a":2}`, 2, false},
		{"empty", ``, 0, false},
		{"torn tail keeps the prefix", "{\"a\":1}\n{\"a\":2}\n{\"ts\":17", 2, true},
		{"break on garbage mid-stream", `1 2 garbage 3`, 2, true},
		{"first value broken", `garbage`, 0, true},
	}
	for _, tc := range tests {
		vs, err := jsonv.DecodeStreamPrefix(strings.NewReader(tc.in))
		if len(vs) != tc.count || (err != nil) != tc.wantErr {
			t.Errorf("%s: got %d values, err %v; want %d, err %v", tc.name, len(vs), err, tc.count, tc.wantErr)
		}
		if !tc.wantErr {
			if vs2, err2 := jsonv.DecodeStream(strings.NewReader(tc.in)); err2 != nil || len(vs2) != len(vs) {
				t.Errorf("%s: DecodeStream disagrees: %v", tc.name, err2)
			}
		}
	}
}
