package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
// property TestHoldRowsInterleaveWithBusWritersIntoAllJSON asserts across writers.
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

// The bats "bus: concurrent ..." cases, ported: a splice is invisible to a
// line count alone — every append contributes exactly one newline either way —
// so these race real concurrent writers and assert one parseable record per
// line, each starting with exactly one {"ts".
func TestAppendConcurrentNeverSplices(t *testing.T) {
	const writers, perWriter = 8, 5
	buildRow := func(w int, body string) string {
		q, _ := json.Marshal(body)
		return fmt.Sprintf(`{"ts":1700000000000,"crew_id":"c1","kind":"status","from":"worker:b%d","body":{"state":"working","detail":%s}}`, w, q)
	}
	race := func(t *testing.T, linesOf func() []string) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		lines := linesOf()
		var wg sync.WaitGroup
		for _, line := range lines {
			wg.Add(1)
			go func(line string) {
				defer wg.Done()
				for range perWriter {
					if err := Append(path, line); err != nil {
						t.Errorf("append: %v", err)
						return
					}
				}
			}(line)
		}
		wg.Wait()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if len(got) != writers*perWriter {
			t.Fatalf("%d lines, want %d", len(got), writers*perWriter)
		}
		starts := 0
		for i, line := range got {
			if err := json.Unmarshal([]byte(line), new(any)); err != nil {
				t.Fatalf("line %d does not parse: %v\n%s", i+1, err, line)
			}
			starts += strings.Count(line, `{"ts"`)
		}
		if starts != writers*perWriter {
			t.Errorf("%d record starts, want %d: a splice merged two records", starts, writers*perWriter)
		}
	}

	// The oversized case: an 8192-x detail, shrunk by FitLine to the cap, so
	// every finished line lands at the 4 KiB boundary a splitting write would
	// break apart under concurrency.
	t.Run("oversized", func(t *testing.T) {
		race(t, func() []string {
			lines := make([]string, writers)
			for w := range lines {
				lines[w] = FitLine(func(text string) string { return buildRow(w, text) }, strings.Repeat("x", 8192))
				if len(lines[w]) > LineMax {
					t.Fatalf("FitLine left %d bytes, over the cap", len(lines[w]))
				}
			}
			return lines
		})
	})

	// The reply case at the exact boundary: its 4002 x's landed the finished
	// line one byte over the cap — the fit loop measures without the newline
	// Append adds, sees LineMax, and never engages, while the write itself
	// crosses 4096 + 1.
	t.Run("line at the cap", func(t *testing.T) {
		race(t, func() []string {
			lines := make([]string, writers)
			for w := range lines {
				lines[w] = buildRow(w, strings.Repeat("x", LineMax-len(buildRow(w, ""))))
				if len(lines[w]) != LineMax {
					t.Fatalf("the line is %d bytes, want exactly the %d-byte cap", len(lines[w]), LineMax)
				}
			}
			return lines
		})
	})
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

// A JSON body is shortened leaf by leaf and re-encoded, keeping the record's
// shape; a non-string leaf is untouched. Keys come out sorted.
func TestShrinkKeepsJSONShape(t *testing.T) {
	got := Shrink(`{"id":"abcdefghijklmnop","released":true,"n":1234567890,"k":"ab"}`, 3)
	want := `{"id":"abc` + elided + `","k":"ab","n":1234567890,"released":true}`
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
		"leading BOM":      {"\xef\xbb\xbf" + `{"a":"aaaaaaaa"}`, `{"a":"aa` + elided + `"}`},
		"empty text":       {"", elided},
		"scalar":           {"5", "5" + elided},
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

// `cut -c1-keep` is line-oriented, and _shrink's blob branch runs the text
// through it inside $(...): each line is capped on its own, and every trailing
// newline is stripped. A whole-string cut of the same width is a different text
// — and a hold row built from it is a different row.
func TestShrinkCutsEachLine(t *testing.T) {
	cases := []struct {
		name string
		text string
		keep int
		want string
	}{
		{"one line", "abcdef", 3, "abc" + elided},
		{"each line capped", "abcdef\nghijkl", 3, "abc\nghi" + elided},
		{"short lines survive", "ab\ncdefgh\n", 4, "ab\ncdef" + elided},
		{"trailing newlines go", "ab\n\n\n", 5, "ab" + elided},
		{"runes, not bytes", "漢字abcdef", 3, "漢字a" + elided},
		{"keep 0 is cut's invalid range", "abcdef\nghij", 0, elided},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Shrink(tc.text, tc.keep); got != tc.want {
				t.Errorf("Shrink(%q, %d) = %q, want %q", tc.text, tc.keep, got, tc.want)
			}
		})
	}
}

// A multi-line text that never shrinks a single line (each line is shorter than
// keep) still loses nothing but its trailing newline — the loop then keeps
// cutting until keep reaches the line width.
func TestFitLineConvergesOnAMultiLineText(t *testing.T) {
	full := strings.Repeat("a\n", 5000)
	got := FitLine(func(text string) string { return `{"t":"` + text + `"}` }, full)
	if len(got) > LineMax {
		t.Fatalf("%d bytes, over the cap", len(got))
	}
	want := `{"t":"` + Shrink(full, 0) + `"}`
	if got != want {
		t.Errorf("the loop stopped early: %d bytes, want the keep-0 line (%d)", len(got), len(want))
	}
}
