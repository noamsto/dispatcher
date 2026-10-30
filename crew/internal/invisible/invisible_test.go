package invisible

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEscape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\u061c", `\u{061C}`},
		{"\u180e", `\u{180E}`},
		{"\u200b", `\u{200B}`},
		{"\u200f", `\u{200F}`},
		{"\u202a", `\u{202A}`},
		{"\u202e", `\u{202E}`},
		{"\u2060", `\u{2060}`},
		{"\u2064", `\u{2064}`},
		{"\u2066", `\u{2066}`},
		{"\u2069", `\u{2069}`},
		{"\ufeff", `\u{FEFF}`},
		{"\U000e0000", `\u{E0000}`},
		{"\U000e0041", `\u{E0041}`},
		{"\U000e007f", `\u{E007F}`},
		{"hi\U000e0041x\u200by", `hi\u{E0041}x\u{200B}y`},
		{"hello", "hello"},
		{"café", "café"},
		{"日本語", "日本語"},
		{"🚀", "🚀"},
		{"a\tb", "a\tb"},
		{"\u200a", "\u200a"},
		{"\u2010", "\u2010"},
		{"\u2065", "\u2065"},
		{"\U000e0080", "\U000e0080"},
		{"\u061b", "\u061b"},
	} {
		if got := Escape(tc.in); got != tc.want {
			t.Errorf("Escape(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := Escape(Escape(tc.in)); got != Escape(tc.in) {
			t.Errorf("Escape not idempotent for %q: %q", tc.in, got)
		}
	}
}

func TestValue(t *testing.T) {
	in := map[string]any{
		"k\u200b": "a\u200bb",
		"list":    []any{"x\ufeff", float64(1), json.Number("2"), true, nil},
		"rows":    []map[string]any{{"body": "y\U000e0041"}},
		"nested":  map[string]any{"d": "z\u202e"},
	}
	want := map[string]any{
		`k\u{200B}`: `a\u{200B}b`,
		"list":      []any{`x\u{FEFF}`, float64(1), json.Number("2"), true, nil},
		"rows":      []map[string]any{{"body": `y\u{E0041}`}},
		"nested":    map[string]any{"d": `z\u{202E}`},
	}
	if got := Value(in); !reflect.DeepEqual(got, want) {
		t.Errorf("Value = %#v, want %#v", got, want)
	}
	if in["k\u200b"] != "a\u200bb" || in["nested"].(map[string]any)["d"] != "z\u202e" ||
		in["list"].([]any)[0] != "x\ufeff" || in["rows"].([]map[string]any)[0]["body"] != "y\U000e0041" {
		t.Errorf("Value mutated its input: %#v", in)
	}
}

func TestValueKeepsJSONBodiesDecodable(t *testing.T) {
	body := `{"detail":"a` + "\u200B" + `b","n":12345678901234567890}`
	got, ok := Value(body).(string)
	if !ok {
		t.Fatalf("Value(%q) is not a string", body)
	}
	var decoded map[string]any
	dec := json.NewDecoder(strings.NewReader(got))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("result %q is not JSON: %v", got, err)
	}
	if decoded["detail"] != `a\u{200B}b` {
		t.Errorf("detail = %q", decoded["detail"])
	}
	if n, _ := decoded["n"].(json.Number); n.String() != "12345678901234567890" {
		t.Errorf("n = %v, want the number text preserved", decoded["n"])
	}
}

func TestValueEscapesInvalidJSONLikeText(t *testing.T) {
	in := "{not json" + "\u200B"
	if got, want := Value(in), `{not json\u{200B}`; got != want {
		t.Errorf("Value(%q) = %q, want %q", in, got, want)
	}
}
