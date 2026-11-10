package frame

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func composerFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "frames", "rw_"+name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\n")
}

const (
	wrappedHead = `{"role":"reviewer","question":"Check the intentionally long assignment that wraps across terminal `
	wrappedTail = `rows without an editor newline"}`
)

func TestCodexComposer(t *testing.T) {
	empty := composerFixture(t, "codex_idle_empty")
	unsent := composerFixture(t, "codex_idle_unsent")
	assigned := strings.Replace(empty, "› Ask Codex to do anything", "› Assignment: x", 1)
	wrapped := strings.Replace(empty, "› Ask Codex to do anything", "› Assignment: "+wrappedHead+"\n"+wrappedTail, 1)
	footer := "  GPT-5.6-Sol low · ~/x Vim: Insert"
	status := "  ? for shortcuts   ⚠ 2 warnings · f2 to view"
	historical := strings.Replace(empty, "Tip: Use /title to choose what appears in your terminal’s title.\n",
		"Tip: Use /title to choose what appears in your terminal’s title.\n› Assignment: x\n", 1)

	tests := []struct {
		name     string
		text     string
		composer string
		want     bool
	}{
		{"idle empty", empty, "Ask Codex to do anything", true},
		{"assignment", assigned, "Assignment: x", true},
		{"assignment is not the placeholder", assigned, "Ask Codex to do anything", false},
		{"wrapped composer", wrapped, "Assignment: " + wrappedHead + wrappedTail, true},
		{"wrapped composer mismatch", strings.Replace(wrapped, "intentionally ", "different ", 1), "Assignment: " + wrappedHead + wrappedTail, false},
		{"expected holds a newline", assigned, "Assignment: x\ny", false},
		{"live turn", composerFixture(t, "codex_live_turn"), "Ask Codex to do anything", false},
		{"esc to interrupt alone vetoes", empty + "\nesc to interrupt", "Ask Codex to do anything", false},
		{"dialog", composerFixture(t, "codex_folder_trust_dialog"), "Ask Codex to do anything", false},
		{"unsent draft", unsent, "Ask Codex to do anything", false},
		{"two footers", empty + "\n" + footer + "\n" + status, "Ask Codex to do anything", false},
		{"footer-like line not terminating", empty + "\n  Dialog overlay: Enter to confirm", "Ask Codex to do anything", false},
		{"decoy footer", strings.Replace(assigned, "Vim: Insert", "Vim: Insert (historical)", 1), "Assignment: x", false},
		{"prefix only above a different block", historical, "Assignment: x", false},
		{"no footer", "› Ask Codex to do anything", "Ask Codex to do anything", false},
		{"empty capture", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexComposer(tc.text, tc.composer); got != tc.want {
				t.Errorf("CodexComposer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexIdleBox(t *testing.T) {
	empty := composerFixture(t, "codex_idle_empty")
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"idle", empty, true},
		{"unsent draft", composerFixture(t, "codex_idle_unsent"), false},
		{"assignment", strings.Replace(empty, "Ask Codex to do anything", "Assignment: x", 1), false},
		{"live turn", composerFixture(t, "codex_live_turn"), false},
		{"dialog", composerFixture(t, "codex_folder_trust_dialog"), false},
		{"boot frame", composerFixture(t, "codex_boot_update"), false},
		{"working marker", strings.Replace(empty, "Tip: Use /title", "• Working (2s)\nTip: Use /title", 1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexIdleBox(tc.text); got != tc.want {
				t.Errorf("CodexIdleBox = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCursorComposer(t *testing.T) {
	empty := composerFixture(t, "cursor_idle_empty")
	assigned := strings.Replace(empty, "  → Plan, search, build anything", "  → Assignment: x", 1)
	wrapped := strings.Replace(empty, "  → Plan, search, build anything", "  → Assignment: "+wrappedHead+"\n"+wrappedTail, 1)
	footer := "  Grok 4.7 256K High                    Run Everything -- INSERT --"
	historical := strings.Replace(empty, "  Tip: Use /debug to instrument and debug complex problems.\n",
		"  Tip: Use /debug to instrument and debug complex problems.\n  → Assignment: x\n", 1)

	tests := []struct {
		name     string
		text     string
		composer string
		want     bool
	}{
		{"idle empty", empty, "Plan, search, build anything", true},
		{"assignment", assigned, "Assignment: x", true},
		{"assignment is not the placeholder", assigned, "Plan, search, build anything", false},
		{"wrapped composer", wrapped, "Assignment: " + wrappedHead + wrappedTail, true},
		{"wrapped composer mismatch", strings.Replace(wrapped, "intentionally ", "different ", 1), "Assignment: " + wrappedHead + wrappedTail, false},
		{"expected holds a newline", assigned, "Assignment: x\ny", false},
		{"live turn", composerFixture(t, "cursor_live_turn"), "Add a follow-up", false},
		{"dialog", composerFixture(t, "cursor_ask_dialog"), "Plan, search, build anything", false},
		{"two footers", empty + "\n" + footer, "Plan, search, build anything", false},
		{"missing version row", strings.Replace(empty, "  v2026.09.26-dd393fe\n", "", 1), "Plan, search, build anything", false},
		{"unknown status row", empty + "\n  something unexpected here", "Plan, search, build anything", false},
		{"no status rows", strings.Join(strings.Split(empty, "\n")[:13], "\n"), "Plan, search, build anything", false},
		{"no path row", strings.Join(strings.Split(empty, "\n")[:13], "\n") + "\n  cratch-repo · main", "Plan, search, build anything", false},
		{"too many status rows", empty + strings.Repeat("\n  more/ · x", 6), "Plan, search, build anything", false},
		{"decoy footer", strings.Replace(assigned, "INSERT --", "INSERT -- (historical)", 1), "Assignment: x", false},
		{"prefix only above a different block", historical, "Assignment: x", false},
		{"thinking row vetoes", empty + "\n Thinking  11 tokens", "Plan, search, build anything", false},
		{"empty capture", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CursorComposer(tc.text, tc.composer); got != tc.want {
				t.Errorf("CursorComposer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCursorIdleBox(t *testing.T) {
	empty := composerFixture(t, "cursor_idle_empty")
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"idle", empty, true},
		{"follow-up placeholder", strings.Replace(empty, "Plan, search, build anything", "Add a follow-up", 1), true},
		{"unsent draft", composerFixture(t, "cursor_idle_unsent"), false},
		{"assignment", strings.Replace(empty, "Plan, search, build anything", "Assignment: x", 1), false},
		{"live turn", composerFixture(t, "cursor_live_turn"), false},
		{"dialog", composerFixture(t, "cursor_ask_dialog"), false},
		{"boot frame", "\n  Cursor Agent\n  v2026.09.26-dd393fe", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CursorIdleBox(tc.text); got != tc.want {
				t.Errorf("CursorIdleBox = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFooterComposerEqual(t *testing.T) {
	footer := posix(`^F$`)
	tests := []struct {
		name     string
		text     string
		sep      int
		expected string
		trailing []string
		want     bool
	}{
		{"block above footer", "> ab\n\nF", 1, "ab", nil, true},
		{"wrapped block", "> a\nb\nc\n\nF", 1, "abc", nil, true},
		{"zero separator rows", "> ab\nF", 0, "ab", nil, true},
		{"footer too high", "\nF", 1, "", nil, false},
		{"separator not blank", "> ab\n \nF", 1, "ab", nil, false},
		{"composer row blank", "> ab\n\n\nF", 1, "ab", nil, false},
		{"prefix on a later block row only", "x\n> ab\n\nF", 1, "ab", nil, false},
		{"prefix missing from block start", "> x\ny\n\nF", 1, "y", nil, false},
		{"trailing row present", "> ab\n\nF\nT", 1, "ab", []string{"^T$"}, true},
		{"trailing row missing", "> ab\n\nF", 1, "ab", []string{"^T$"}, false},
		{"extra row after trailing", "> ab\n\nF\nT\nT", 1, "ab", []string{"^T$"}, false},
		{"unexpected row after footer", "> ab\n\nF\nT", 1, "ab", nil, false},
		{"duplicate footer", "> ab\n\nF\nF", 1, "ab", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trailing := make([]*regexp.Regexp, 0, len(tc.trailing))
			for _, p := range tc.trailing {
				trailing = append(trailing, posix(p))
			}
			if got := FooterComposerEqual(tc.text, "> ", footer, tc.sep, tc.expected, trailing...); got != tc.want {
				t.Errorf("FooterComposerEqual = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLiveTurn(t *testing.T) {
	codexIdle := composerFixture(t, "codex_idle_empty")
	cursorThink := "  ⬢ Thinking 1200 tokens"
	tests := []struct {
		name string
		got  func(string) bool
		text string
		want bool
	}{
		{"codex working row", CodexLiveTurn, "  • Working (3s)\n" + codexIdle, true},
		{"codex interrupt hint", CodexLiveTurn, codexIdle + "\nesc to interrupt", true},
		{"codex idle", CodexLiveTurn, codexIdle, false},
		{"cursor thinking row", CursorLiveTurn, cursorThink, true},
		{"cursor stop hint", CursorLiveTurn, "ctrl+c to stop", true},
		{"cursor idle", CursorLiveTurn, "  Plan, search, build anything", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got(tc.text); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
