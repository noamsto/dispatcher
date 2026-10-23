package stall

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// Prompt-family whole-watch tests: the rows a watch leaves on the bus over a
// parked option-select, workspace-trust or tool-permission frame — the exact
// detail, one row per episode, and the frames that leave the bus empty. Which
// frame is which kind is the classifiers' business (TestD1Prompt,
// TestPermissionDetail); these are the runs whose `blocked` rows a dispatcher
// acts on.

const (
	leadID = "worker:feat/x"

	// The generic prompt row: the detail names the pane, so the row spells %9.
	promptRow = leadID + "|blocked|watchdog|prompt: interactive prompt in pane %9 — " +
		"worker is waiting on input nobody can give"
	// permission_subagent.txt's own detail: the relay row and the request line.
	permissionRow = leadID + "|blocked|watchdog|prompt: permission — Bash command: " +
		"bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30 — pane %9"
)

func TestPromptWatchRows(t *testing.T) {
	for _, tc := range []struct {
		name    string
		frame   string
		maxLife string
		// window and stall are set together for the frames held inside D0's
		// startup window, where D0 has to leave the frame to D1.
		window string
		stall  string
		want   []string
		// capped checks the one detail the run posted against the permission
		// detail contract (one line, no escape characters, 160 characters).
		capped bool
		// emptyBus is not one status row but no row of any kind.
		emptyBus bool
	}{
		{
			name:    "the option-select frame posts prompt: once",
			frame:   "prompt_select.txt",
			maxLife: "3",
			want:    []string{promptRow},
		},
		{
			// The widened footer set lives in D1's own signature set, not only in
			// D0's classifier, and the permission detector leaves the frame alone.
			name:    "the workspace-trust frame is prompt:, not permission: or quota:",
			frame:   "prompt_trust.txt",
			maxLife: "3",
			want:    []string{promptRow},
		},
		{
			// The measured 3/3 false positive: a byte-static trust frame inside
			// --window belongs to D1 — never failed:, never D0's stalled:.
			name:    "a static trust frame is prompt:, never stalled:",
			frame:   "prompt_trust.txt",
			window:  "60",
			stall:   "1",
			maxLife: "8",
			want:    []string{promptRow},
		},
		{
			// #435: this frame's footer is `Esc to cancel · Tab to amend`, which
			// the plain prompt signature does not carry.
			name:    "the real tool-permission frame posts its own detail",
			frame:   "permission_subagent.txt",
			maxLife: "4",
			want:    []string{permissionRow},
		},
		{
			// D0 defers to D1 for permission frames: held inside --window a static
			// one would otherwise also raise `stalled: no output`.
			name:    "a static permission frame is prompt:, never stalled:",
			frame:   "permission_subagent.txt",
			window:  "60",
			stall:   "1",
			maxLife: "8",
			want:    []string{permissionRow},
		},
		{
			name:     "a permission dialog scrolled into the transcript posts nothing",
			frame:    "permission_scrollback.txt",
			maxLife:  "4",
			emptyBus: true,
		},
		{
			// The header is the LAST `· from the … agent` line: a prior permission
			// block scrolled into the transcript never gets reported while a live
			// dialog is parked at the bottom.
			name:    "a stale dialog above the live one does not poison the detail",
			frame:   "permission_stale.txt",
			maxLife: "4",
			want:    []string{permissionRow},
		},
		{
			name:    "a permission frame with no header detail still fires (unparsed)",
			frame:   "permission_unparsed.txt",
			maxLife: "4",
			want: []string{leadID +
				"|blocked|watchdog|prompt: permission — (unparsed) — pane %9"},
		},
		{
			name:    "a permission detail is one line, ESC-free and capped at 160",
			frame:   "permission_sanitize.txt",
			maxLife: "4",
			// The body is cut at 160 - len(" — pane %9") = 150 characters, so the
			// 220-x command leaves 115 of them behind the 35-character lead-in.
			want: []string{leadID + "|blocked|watchdog|prompt: permission — Bash command: " +
				strings.Repeat("x", 115) + " — pane %9"},
			capped: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Pane: ptr("%9"), MaxLife: ptr(tc.maxLife)}
			if tc.window != "" {
				req.Window, req.Stall = ptr(tc.window), ptr(tc.stall)
			}
			h, rows := watchRun(t, watchScript{frames: []string{tc.frame}, req: req})
			if tc.capped {
				cappedDetail(t, h, rows)
			}
			if !slices.Equal(rows, tc.want) {
				t.Errorf("rows\n %q\nwant\n %q", rows, tc.want)
			}
			if tc.emptyBus {
				if n := lineCount(h); n != 0 {
					t.Errorf("bus has %d rows, want none", n)
				}
			}
		})
	}
}

// cappedDetail checks a posted permission detail against the permission detail
// contract: one line, no escape or control character left, the pane suffix
// intact, 160 characters all told. The bus is read raw as well, because `_post`
// JSON-encodes a surviving ESC as \u001b and a raw-ESC grep over the row would
// be a tautology.
func cappedDetail(t *testing.T, h *harness, rows []string) {
	t.Helper()
	const prefix = leadID + "|blocked|watchdog|"
	if len(rows) != 1 || !strings.HasPrefix(rows[0], prefix) {
		t.Fatalf("rows %q, want one watchdog blocked row", rows)
	}
	detail := strings.TrimPrefix(rows[0], prefix)
	if !strings.HasPrefix(detail, "prompt: permission — Bash command: ") ||
		!strings.HasSuffix(detail, " — pane %9") {
		t.Errorf("detail %q", detail)
	}
	if n := len([]rune(detail)); n != 160 {
		t.Errorf("detail is %d characters, want the 160 cap", n)
	}
	if strings.ContainsAny(detail, "\x1b\n\t\r") {
		t.Errorf("detail carries control characters: %q", detail)
	}
	raw, err := os.ReadFile(h.paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "u001b"); n != 0 {
		t.Errorf("bus carries %d JSON-escaped ESCs, want none", n)
	}
}
