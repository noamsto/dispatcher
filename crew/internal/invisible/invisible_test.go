package invisible

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fixtureCase struct {
	Name    string `json:"name"`
	In      string `json:"in"`
	Escaped string `json:"escaped"`
	Dropped string `json:"dropped"`
}

func loadCases(t *testing.T) []fixtureCase {
	t.Helper()
	data, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []fixtureCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestEscape(t *testing.T) {
	for _, tc := range loadCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if got := Escape(tc.In); got != tc.Escaped {
				t.Errorf("Escape(%q) = %q, want %q", tc.In, got, tc.Escaped)
			}
			if got := Drop(tc.In); got != tc.Dropped {
				t.Errorf("Drop(%q) = %q, want %q", tc.In, got, tc.Dropped)
			}
			if got := Escape(Escape(tc.In)); got != Escape(tc.In) {
				t.Errorf("Escape not idempotent for %q: %q", tc.In, got)
			}
			if got := Drop(Drop(tc.In)); got != Drop(tc.In) {
				t.Errorf("Drop not idempotent for %q: %q", tc.In, got)
			}
			if got, want := Has(tc.In), tc.Escaped != tc.In; got != want {
				t.Errorf("Has(%q) = %v, want %v", tc.In, got, want)
			}
		})
	}
}

func TestEscapeLeavesControlsAndPlainText(t *testing.T) {
	for _, s := range []string{"hello", "a\tb", "a\nb", "\u200a", "\u2010", "\U000e0080", "\u061b", "\U0001f680"} {
		if got := Escape(s); got != s {
			t.Errorf("Escape(%q) = %q, want unchanged", s, got)
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
