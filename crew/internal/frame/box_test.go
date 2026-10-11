package frame

import (
	"os"
	"strings"
	"testing"
)

// nudgeKey is the line `crew nudge` types with all whitespace dropped — what
// BoxText's caller compares a lead's input box against.
const nudgeKey = `crewinbox"$CREW_WORKER_ID"`

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/frames/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(data), "\n")
}

// BoxText is what tells a nudge whether the line it typed is sitting in the lead's
// box. Two properties carry the caller: a box the engine wrapped onto more rows still
// reads as the one line typed, and an empty box is an answer rather than a failure.
func TestBoxText(t *testing.T) {
	for _, tc := range []struct {
		file string
		what string
		want string
	}{
		{file: "nudge_typed.txt", what: "the line in the box", want: nudgeKey},
		{file: "nudge_typed_wrapped.txt", what: "the same line, wrapped over two rows", want: nudgeKey},
		{file: "nudge_draft.txt", what: "a draft the lead typed itself", want: "hellodrafttext"},
		{file: "done_idle.txt", what: "an empty box is empty, not a failure", want: ""},
		{
			file: "nudge_pi_typed.txt", what: "a pi box has no claude prompt row, so the claude first-row check refuses it",
			want: "",
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			if got := BoxText(fixture(t, tc.file), FirstRowClaude); got != tc.want {
				t.Errorf("BoxText(%s) = %q, want %q", tc.what, got, tc.want)
			}
		})
	}

	// The engine-agnostic first row reads any engine's box: that is the pi caller.
	if got := BoxText(fixture(t, "nudge_pi_typed.txt"), FirstRowAny); got != nudgeKey {
		t.Errorf("pi box with FirstRowAny = %q, want %q", got, nudgeKey)
	}
	if got := BoxText(fixture(t, "nudge_pi_idle.txt"), FirstRowAny); got != "" {
		t.Errorf("idle pi box = %q, want empty", got)
	}
	// No box at all is empty text, never a panic.
	if got := BoxText("just some output\nno rules here", FirstRowAny); got != "" {
		t.Errorf("frame with no box = %q, want empty", got)
	}
}

// Tail is the sampler's window: the last n non-blank rows, which is what every
// above-the-box predicate scans.
func TestTail(t *testing.T) {
	text := "one\n\ntwo\nthree\n\n"
	if got := strings.Join(Tail(text, 2), "|"); got != "two|three" {
		t.Errorf("Tail(_, 2) = %q, want the last two non-blank rows", got)
	}
	if got := strings.Join(Tail(text, 99), "|"); got != "one|two|three" {
		t.Errorf("Tail(_, 99) = %q, want every row, blanks dropped", got)
	}
	if got := Tail("", 5); len(got) != 0 {
		t.Errorf("Tail(\"\", 5) = %v, want none", got)
	}
}

// HasSpinnerRow is the live-turn half of the post-Enter check: the `…` spinner row
// says the lead took the line and started a turn.
func TestHasSpinnerRow(t *testing.T) {
	if !HasSpinnerRow(Tail(fixture(t, "meter.2s.1.2k.txt"), 30)) {
		t.Error("a mid-turn frame has no spinner row")
	}
	for _, name := range []string{"done_idle.txt", "nudge_typed.txt", "nudge_pi_idle.txt"} {
		if HasSpinnerRow(Tail(fixture(t, name), 30)) {
			t.Errorf("%s has a spinner row", name)
		}
	}
}
