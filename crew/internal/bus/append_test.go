package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAppendMissingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := Append(path, `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\"a\":1}\n" {
		t.Errorf("wrote %q", got)
	}
}

func TestAppendsStackOnePerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	for _, line := range []string{`{"a":1}`, `{"b":2}`} {
		if err := Append(path, line); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(path)
	if string(got) != "{\"a\":1}\n{\"b\":2}\n" {
		t.Errorf("wrote %q", got)
	}
}

// A log whose last byte is not a newline is a torn tail: the helper prefixes one
// newline so the fragment stays isolated, and an empty log gains no blank line.
func TestAppendTornTailAndEmptyLog(t *testing.T) {
	cases := map[string]struct{ before, want string }{
		"torn tail":      {`{"a":1}`, "{\"a\":1}\n{\"b\":2}\n"},
		"whole line":     {"{\"a\":1}\n", "{\"a\":1}\n{\"b\":2}\n"},
		"empty log":      {"", "{\"b\":2}\n"},
		"bare newline":   {"\n", "\n{\"b\":2}\n"},
		"torn mid-digit": {`{"a":`, "{\"a\":\n{\"b\":2}\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := Append(path, `{"b":2}`); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Every line of a log written through Append parses, torn tail included — the
// property the interleave test in hold.bats asserts across writers.
func TestAppendKeepsEveryLineParseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(`{"crew_id":"c1"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, `{"to":"hold:c1"}`); err != nil {
		t.Fatal(err)
	}
	if err := Append(path, `{"to":"retro:1"}`); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines: %q", lines)
	}
	for i, line := range lines {
		if i > 0 {
			if err := json.Unmarshal([]byte(line), new(any)); err != nil {
				t.Errorf("line %d does not parse: %v", i, err)
			}
		}
	}
}

func TestShrinkCutsCharactersNotBytes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abcdef", "ab" + elided},
		{"héllo wörld", "hé" + elided},
		{"😀🙂😀🙂x", "😀🙂" + elided},
		{"", elided},
	}
	for _, tc := range cases {
		if got := Shrink(tc.in, 2); got != tc.want {
			t.Errorf("Shrink(%q, 2) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A JSON body is shortened leaf by leaf and re-encoded, keeping key order and the
// record's shape; a non-string leaf is untouched.
func TestShrinkKeepsJSONShape(t *testing.T) {
	got := Shrink(`{"id":"abcdefghijklmnop","released":true,"n":1234567890,"k":"ab"}`, 3)
	want := `{"id":"abc` + elided + `","released":true,"n":1234567890,"k":"ab"}`
	if got != want {
		t.Errorf("got %s", got)
	}
	if nested := Shrink(`{"a":{"b":["xyzw","q"]}}`, 1); nested !=
		`{"a":{"b":["x`+elided+`","q"]}}` {
		t.Errorf("nested: %s", nested)
	}
}

// jq -e reads the LAST value of the stream, and its walk rewrites every value it
// parsed — so a two-document text shrinks as two documents, and trailing junk
// sends jq (and Shrink) to the blob cut.
func TestShrinkStreamBranch(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"two documents": {
			`{"a":"aaaaaaaa"} ["bbbbbbbb"]`,
			`{"a":"aa` + elided + `"}` + "\n" + `["bb` + elided + `"]`,
		},
		// The last value is a scalar, so jq -e is false and the blob cut wins.
		"last is a scalar": {`{"a":"aaaaaaaa"} 5`, `{"` + elided},
		"trailing junk":    {`{"a":"aaaaaaaa"} x`, `{"` + elided},
		"empty text":       {"", elided},
		"scalar":           {"5", "5" + elided},
		// jq accepts its NaN extension and writes `nan` back; jsonv encodes a
		// non-finite number as null, the one text difference here (documented).
		"jq NaN literal": {`[NaN]`, "[null]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Shrink(tc.in, 2); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// buildRow is a stand-in for an arm's row builder: the text lands as one JSON
// string leaf, escaped as `jq --arg` escapes it.
func buildRow(text string) string {
	q, _ := json.Marshal(text)
	return `{"kind":"msg","body":` + string(q) + `}`
}

func TestFitLineKeepsTheCap(t *testing.T) {
	cases := map[string]string{
		"ascii":     strings.Repeat("a", 6000),
		"cjk":       strings.Repeat("\u6f22", 3000),
		"emoji":     strings.Repeat("\U0001F642", 3000),
		"combining": strings.Repeat("e\u0301", 4000),
		"json body": `{"id":"` + strings.Repeat("x", 9000) + `","released":true}`,
	}
	for name, full := range cases {
		t.Run(name, func(t *testing.T) {
			if len(buildRow(full)) <= LineMax {
				t.Fatalf("the input already fits (%d bytes); this case must overflow", len(buildRow(full)))
			}
			line := FitLine(buildRow, full)
			if len(line) > LineMax {
				t.Fatalf("%d bytes, over the cap", len(line))
			}
			var row struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("the shrunk line is not JSON: %v", err)
			}
			if !strings.Contains(row.Body, elided) {
				t.Errorf("the line carries no elided marker: %q", line)
			}
		})
	}
}

// keep follows the helper's own recurrence: it starts at the character count of
// the full text and steps by min(keep*LineMax/n, keep*3/4), where n is the byte
// length of the line as last built.
func TestFitLineFollowsTheHelperRecurrence(t *testing.T) {
	full := strings.Repeat("x", 5000)
	line := FitLine(buildRow, full)
	n0 := len(buildRow(full))
	keep := utf8.RuneCountInString(full)
	step := keep * LineMax / n0
	if three := keep * 3 / 4; step >= three {
		step = three
	}
	if want := buildRow(Shrink(full, step)); want != line {
		t.Errorf("one-pass line\n got %q\nwant %q", line, want)
	}
	if n0 <= LineMax || len(line) > LineMax {
		t.Fatalf("this case is one pass: n0=%d, line=%d", n0, len(line))
	}
}

// Every pass must strictly shrink, or the loop never ends.
func TestFitLineTerminatesOnPathologicalText(t *testing.T) {
	full := strings.Repeat("🙂", 3000) // 4 bytes per rune, so bytes/runes differ
	line := FitLine(func(text string) string {
		return `{"a":"` + strings.ReplaceAll(text, "🙂", `\u0001`) + `"}`
	}, full)
	if len(line) > LineMax {
		t.Errorf("%d bytes, over the cap", len(line))
	}
}

// The two copies of the cap and the marker are one contract: crew.sh keeps
// _LINE_MAX and _ELIDED for status/msg/reply, and Go reads them here so a change
// on either side fails a test. Skipped where crew.sh is not in the tree (the Nix
// sandbox builds ./crew alone).
func TestCrewShLineContract(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "adapters", "core", "crew.sh"))
	if err != nil {
		t.Skip("crew.sh is not in this tree")
	}
	want := map[string]string{"_LINE_MAX": fmt.Sprint(LineMax), "_ELIDED": elided}
	for name, wantValue := range want {
		line := crewShLine(string(src), name)
		if line == "" {
			t.Errorf("crew.sh no longer defines %s", name)
			continue
		}
		if line != wantValue {
			t.Errorf("crew.sh %s = %q, Go has %q", name, line, wantValue)
		}
	}
}

func crewShLine(src, name string) string {
	for _, line := range strings.Split(src, "\n") {
		rest, ok := strings.CutPrefix(line, name+"=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimPrefix(rest, "'"), "'")
	}
	return ""
}
