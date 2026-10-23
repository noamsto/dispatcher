package stall

import (
	"strings"
	"testing"
)

// Quota whole-watch tests: the rows a watch leaves on the bus over a frozen
// quota frame — that each variant posts its own `quota:` detail, that the
// episode never escalates and is never superseded by `quiet:` however long it
// is held, and that the two claude quota frames hand the episode over cleanly.
// The classification itself — which frame is which kind, and the far-up-screen
// text that is not quota: at all — is the frame fixtures' tables in
// frame_detectors_test.go.

// One blocked row per quota frame, with the exact detail the dispatcher acts on.
func TestQuotaWatchEpisode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frame  string
		engine string
		want   string
	}{
		{"the rate-limit prompt posts quota:, not prompt:", "prompt_quota.txt", "claude", quotaDetail},
		{"the session-limit refusal posts quota:, not quiet:", "session_limit_refusal.txt", "claude", sessionDetail},
		{"cursor's monthly frame posts quota:", "cursor_monthly_limit.txt", "cursor", cursorDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := watchRun(t, watchScript{
				frames: []string{tc.frame},
				req:    Request{Engine: ptr(tc.engine), MaxLife: ptr("3")},
			})
			wantRows(t, h, tc.want)
		})
	}
}

// The roster cuts a detail to 120 codepoints, so the session-limit row's
// actionable half has to sit inside that cut: a dispatcher reading only the
// roster would otherwise see the limit and nothing telling it not to re-dispatch.
func TestQuotaSessionDetailSurvivesRosterCut(t *testing.T) {
	h, _ := watchRun(t, watchScript{
		frames: []string{"session_limit_refusal.txt"},
		req:    Request{MaxLife: ptr("3")},
	})
	wantRows(t, h, sessionDetail)
	detail := strings.TrimPrefix(pdRows(t, h)[0], "blocked|")
	if r := []rune(detail); len(r) > 120 {
		detail = string(r[:120])
	} else {
		t.Fatalf("detail is %d codepoints, want one the roster cuts", len(r))
	}
	if !strings.Contains(detail, "do not re-dispatch") {
		t.Errorf("the first 120 codepoints drop the guidance: %q", detail)
	}
}

// A quota: episode is exempt from every escalation path: the frozen frame is
// byte-identical, so it satisfies D3 as well, and quiet: must neither supersede
// it nor launder it into a failed — even with the pane's engine gone and --dead
// long past. (The GONE sample that ended the bats runs is TestPaneGoneQuorum.)
func TestQuotaWatchNeverEscalates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frame  string
		engine string
		want   string
	}{
		{"rate-limit", "prompt_quota.txt", "claude", quotaDetail},
		{"session-limit", "session_limit_refusal.txt", "claude", sessionDetail},
		{"cursor monthly", "cursor_monthly_limit.txt", "cursor", cursorDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := watchRun(t, watchScript{
				frames:  []string{tc.frame},
				paneCmd: "bash",
				req: Request{Engine: ptr(tc.engine),
					Idle: ptr("2"), Dead: ptr("2"), MaxLife: ptr("8")},
			})
			// The whole run's rows: nothing quiet: superseding the episode, and
			// nothing failed: laundering it.
			wantRows(t, h, tc.want)
		})
	}
}

// The two claude quota frames share the `quota:` prefix but not the detector:
// each one's own frame leaving the screen clears the episode under its own
// hand, and the other re-opens it with its own detail.
func TestQuotaWatchKindTransitions(t *testing.T) {
	h, _ := watchRun(t, watchScript{
		frames: []string{"prompt_quota.txt", "prompt_quota.txt",
			"session_limit_refusal.txt", "session_limit_refusal.txt",
			"prompt_quota.txt", "prompt_quota.txt"},
		req: Request{MaxLife: ptr("18")},
	})
	wantRows(t, h,
		quotaDetail, "working|quota: cleared", sessionDetail,
		"working|quota: cleared", quotaDetail)
}
