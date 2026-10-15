package rate

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncode(t *testing.T) {
	var buf bytes.Buffer
	v := map[string]any{
		"nan":  math.NaN(),
		"pos":  math.Inf(1),
		"neg":  []any{math.Inf(-1), map[string]any{"x": math.Inf(1)}},
		"num":  json.Number("1.50"),
		"html": "<b>&",
	}
	if err := encode(&buf, v, false); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		`"nan":null`,
		`"pos":1.7976931348623157e+308`,
		`[-1.7976931348623157e+308,{"x":1.7976931348623157e+308}]`,
		`"num":1.50`,
		`"html":"<b>&"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "}\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("want exactly one trailing newline, got %q", got)
	}

	buf.Reset()
	if err := encode(&buf, []any{map[string]any{"a": json.Number("1")}}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "[\n  {\n") {
		t.Errorf("indent output = %q", buf.String())
	}
}

func TestDecodeAll(t *testing.T) {
	if _, err := decodeAll(strings.NewReader("{\"a\":1}\n{\"b\":")); err == nil {
		t.Error("torn stream: want error")
	}
	vs, err := decodeAll(strings.NewReader("1 2"))
	if err != nil || len(vs) != 2 {
		t.Errorf("1 2: got %v, %v", vs, err)
	}
	vs, err = decodeAll(strings.NewReader("18446744073709551617"))
	if err != nil || len(vs) != 1 || vs[0] != json.Number("18446744073709551617") {
		t.Errorf("big int: got %#v, %v", vs, err)
	}
	vs, err = decodeAll(strings.NewReader(""))
	if err != nil || len(vs) != 0 {
		t.Errorf("empty: got %v, %v", vs, err)
	}
}

func TestParseOne(t *testing.T) {
	if v, ok := parseOne("{\"a\":1}\n"); !ok || v.(map[string]any)["a"] != json.Number("1") {
		t.Errorf("object: got %#v, %v", v, ok)
	}
	for _, s := range []string{"1 2", "", "garbage"} {
		if v, ok := parseOne(s); ok || v != nil {
			t.Errorf("parseOne(%q) = %#v, %v; want not ok", s, v, ok)
		}
	}
	if v, ok := parseOne("1.50"); !ok || v != json.Number("1.50") {
		t.Errorf("1.50: got %#v, %v", v, ok)
	}
}

func TestAppendBatchEmptyCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.jsonl")
	if err := appendBatch(path, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("want empty file, got %q", b)
	}
}
