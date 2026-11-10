package rolewatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a captured frame as the bash sees it: `$(…)` strips trailing newlines.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "frame", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\n")
}

const rule = "──────────────────────────────"

func claudeBox(row string) string {
	return "✻ Churned for 36s · done 11:20 AM\n" + rule + "\n" + row + "\n" + rule + "\n  -- INSERT -- ⏵⏵ auto mode on"
}

func piBox(row string) string {
	return " pi v0.87.1\n" + rule + "\n" + row + "\n" + rule + "\n~/git/dispatcher\n0.0%/0 (auto)      unknown"
}

func TestPaneReady(t *testing.T) {
	tests := []struct {
		name, engine, frame string
		colored             string
		own                 bool
		want                bool
	}{
		{"claude idle", "claude", "frames/rw_idle.txt", "", false, true},
		{"claude live turn", "claude", "frames/rw_live.txt", "", false, false},
		{"claude permission dialog", "claude", "frames/rw_permission.txt", "", false, false},
		{"claude option select", "claude", "frames/rw_select.txt", "", false, false},
		{"claude draft without colour", "claude", "frames/rw_claude_draft.txt", "", false, false},
		{"claude draft with colour", "claude", "frames/rw_claude_draft.txt", "frames/rw_claude_draft.color.txt", false, false},
		{"claude ghost with colour", "claude", "frames/rw_claude_ghost.txt", "frames/rw_claude_ghost.color.txt", false, true},
		{"claude own text", "claude", "frames/rw_claude_held.txt", "", true, true},
		{"claude held text is not ready", "claude", "frames/rw_claude_held.txt", "", false, false},
		{"claude unknown frame", "claude", "frames/rw_unknown.txt", "", false, false},
		{"claude empty capture", "claude", "frames/edge_empty.txt", "", false, false},
		{"codex idle", "codex", "composer/codex_idle_empty.txt", "", false, true},
		{"codex unsent", "codex", "composer/codex_idle_unsent.txt", "", false, false},
		{"codex live turn", "codex", "composer/codex_live_turn.txt", "", false, false},
		{"codex dialog", "codex", "composer/codex_dialog.txt", "", false, false},
		{"codex hook review", "codex", "frames/codex_hooks_review.txt", "", false, false},
		{"cursor idle", "cursor", "composer/cursor_idle_empty.txt", "", false, true},
		{"cursor unsent", "cursor", "composer/cursor_idle_unsent.txt", "", false, false},
		{"cursor live turn", "cursor", "composer/cursor_live_turn.txt", "", false, false},
		{"cursor dialog", "cursor", "composer/cursor_dialog.txt", "", false, false},
		{"cursor monthly limit", "cursor", "frames/rw_cursor_monthly_limit.txt", "", false, false},
		{"pi idle", "pi", "frames/rw_pi_idle.txt", "", false, true},
		{"pi vim idle", "pi", "frames/rw_pi_idle_vim.txt", "", false, true},
		{"pi live turn", "pi", "frames/rw_pi_live.txt", "", false, false},
		{"pi unknown frame", "pi", "frames/rw_unknown.txt", "", false, false},
		{"unknown engine on an idle claude frame", "bogus", "frames/rw_idle.txt", "", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			colored := ""
			if tc.colored != "" {
				colored = fixture(t, tc.colored)
			}
			if got := paneReady(tc.engine, fixture(t, tc.frame), colored, tc.own); got != tc.want {
				t.Errorf("paneReady(%s, %s) = %v, want %v", tc.engine, tc.frame, got, tc.want)
			}
		})
	}
}

func TestAssignmentConfirmed(t *testing.T) {
	tests := []struct {
		name, engine, frame, assignment string
		want                            bool
	}{
		{"codex typed", "codex", "frames/rw_codex_after_assignment.txt", "go", true},
		{"codex other text", "codex", "frames/rw_codex_after_assignment.txt", "stop", false},
		{"codex empty composer", "codex", "composer/codex_idle_empty.txt", "go", false},
		{"codex dialog overlay", "codex", "frames/rw_codex_after_assignment_dialog_overlay.txt", "go", false},
		{"codex historical text", "codex", "frames/rw_codex_historical_assignment_arbitrary.txt", "go", false},
		{"cursor typed", "cursor", "frames/rw_cursor_after_assignment.txt", "go", true},
		{"cursor other text", "cursor", "frames/rw_cursor_after_assignment.txt", "stop", false},
		{"cursor dialog overlay", "cursor", "frames/rw_cursor_after_assignment_dialog_overlay.txt", "go", false},
		{"claude own text", "claude", "frames/rw_claude_held.txt", "go", true},
		{"claude dialog", "claude", "frames/rw_permission.txt", "go", false},
		{"pi idle box", "pi", "frames/rw_pi_held.txt", "go", true},
		{"pi select prompt", "pi", "frames/rw_select.txt", "go", false},
		{"unknown engine", "bogus", "frames/rw_idle.txt", "go", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := assignmentConfirmed(tc.engine, fixture(t, tc.frame), tc.assignment, ""); got != tc.want {
				t.Errorf("assignmentConfirmed(%s, %s) = %v, want %v", tc.engine, tc.frame, got, tc.want)
			}
		})
	}
}

func TestSubmitState(t *testing.T) {
	tests := []struct {
		name, engine, assignment string
		frame                    string // fixture path; "" uses text
		text                     string
		want                     submit
	}{
		{"claude idle box", "claude", "go", "frames/rw_idle.txt", "", submitted},
		{"claude live turn", "claude", "go", "frames/rw_live.txt", "", submitted},
		{"claude spinner above an empty box", "claude", "go", "frames/rw_claude_spinner_empty.txt", "", submitted},
		{"claude held", "claude", "go", "frames/rw_claude_held.txt", "", held},
		{"claude held leading slice", "claude", "go on", "", claudeBox("❯ Assignment: go"), held},
		{"claude held nbsp", "claude", "go", "", claudeBox("❯ Assignment: go  "), held},
		{"claude pasted marker", "claude", "go", "", claudeBox("❯ [Pasted text #1 +3 lines]"), held},
		{"claude human draft", "claude", "go", "frames/rw_claude_draft.txt", "", unknown},
		{"claude row not a prefix", "claude", "go", "", claudeBox("❯ Assignment: stop"), unknown},
		{"claude permission dialog", "claude", "go", "frames/rw_permission.txt", "", dialog},
		{"claude option select", "claude", "go", "frames/rw_select.txt", "", dialog},
		{"claude unknown frame", "claude", "go", "frames/rw_unknown.txt", "", unknown},
		{"codex held", "codex", "go", "frames/rw_codex_after_assignment.txt", "", held},
		{"codex idle empty", "codex", "go", "composer/codex_idle_empty.txt", "", submitted},
		{"codex live turn", "codex", "go", "composer/codex_live_turn.txt", "", submitted},
		{"codex working row", "codex", "go", "", "  • Working (3s)", submitted},
		{"codex unsent draft", "codex", "go", "composer/codex_idle_unsent.txt", "", unknown},
		{"codex hook review dialog", "codex", "go", "frames/codex_hooks_review.txt", "", dialog},
		{"cursor held", "cursor", "go", "frames/rw_cursor_after_assignment.txt", "", held},
		{"cursor idle empty", "cursor", "go", "composer/cursor_idle_empty.txt", "", submitted},
		{"cursor live turn", "cursor", "go", "composer/cursor_live_turn.txt", "", submitted},
		{"cursor thinking", "cursor", "go", "", "  Thinking  42 tokens", submitted},
		{"cursor unsent draft", "cursor", "go", "composer/cursor_idle_unsent.txt", "", unknown},
		{"cursor monthly limit", "cursor", "go", "frames/rw_cursor_monthly_limit.txt", "", unknown},
		{"cursor option select", "cursor", "go", "frames/rw_select.txt", "", dialog},
		{"pi empty box", "pi", "go", "frames/rw_pi_idle.txt", "", submitted},
		{"pi working rule", "pi", "go", "frames/rw_pi_live.txt", "", submitted},
		{"pi held", "pi", "go", "frames/rw_pi_held.txt", "", held},
		{"pi held slice", "pi", "go on", "", piBox("Assignment: go"), held},
		{"pi paste marker", "pi", "go", "frames/rw_pi_paste_marker.txt", "", held},
		{"pi paste marker wrong length", "pi", "go", "frames/rw_pi_paste_marker_wrong.txt", "", unknown},
		{"pi paste marker counts runes", "pi", "é", "", piBox("[paste #1 13 chars]"), held},
		{"pi human draft", "pi", "go", "", piBox("something else"), unknown},
		{"pi unknown frame", "pi", "go", "frames/rw_unknown.txt", "", unknown},
		{"unknown engine", "bogus", "go", "frames/rw_idle.txt", "", unknown},
		{"unknown engine dialog", "bogus", "go", "frames/rw_select.txt", "", dialog},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text
			if tc.frame != "" {
				text = fixture(t, tc.frame)
			}
			if got := submitState(tc.engine, text, tc.assignment, ""); got != tc.want {
				t.Errorf("submitState(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestSubmitString(t *testing.T) {
	for s, want := range map[submit]string{submitted: "submitted", held: "held", dialog: "dialog", unknown: "unknown"} {
		if got := s.String(); got != want {
			t.Errorf("submit(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}
