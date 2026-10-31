package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newRateCLI is a bus-seeded repo whose ratings store lives under a temp
// XDG root, so no test reads or writes the developer's own.
func newRateCLI(t *testing.T) (*holdCLI, *busSeed, string) {
	t.Helper()
	h, seed := newRetroSeed(t)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	// Keep the burn-table lookup off whatever dispatch-config the host has.
	t.Setenv("DISPATCH_CONFIG_BIN", filepath.Join(data, "no-such-dispatch-config"))
	return h, seed, filepath.Join(data, "crew", "ratings.jsonl")
}

func TestRateJSONAndPooledWithoutReportAreRefused(t *testing.T) {
	h, _, _ := newRateCLI(t)
	for _, flag := range []string{"--json", "--pooled"} {
		out, errOut, code := h.run("rate", flag)
		if code != 1 || out != "" || !strings.Contains(errOut, rateUsage) {
			t.Errorf("rate %s: code %d, stdout %q, stderr %q", flag, code, out, errOut)
		}
	}
}

// #194 Gap 3: a grid role that never gets the lead's {"final":true} release
// idles until its watcher posts status `failed` with detail `no assignment` —
// on the ROLE identity. Only `worker:` rows fold into a run's classification,
// so a run whose worker said `done` stays done in both rate and retro.
func TestRateRoleIdleTimeoutFailedRowIsNoWorkerFailure(t *testing.T) {
	h, seed, store := newRateCLI(t)
	seed.dispatch("feat/role-timeout", 1000, nil)
	seed.status("worker:feat/role-timeout#s1-1", 1200, "done")
	seed.add(map[string]any{"ts": 300000, "from": "role:feat/role-timeout:plan-critic", "to": "dispatcher:c1",
		"kind": "status", "body": map[string]any{"state": "failed", "detail": "no assignment"}})
	t.Setenv("CREW_ID", "c1")

	if _, errOut, code := h.run("rate"); code != 0 {
		t.Fatalf("rate: code %d, stderr %q", code, errOut)
	}
	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &row); err != nil {
		t.Fatalf("store %q: %v", data, err)
	}
	if row["outcome"] != "done" || row["terminal_state"] != "done" {
		t.Errorf("outcome/terminal_state = %v/%v, want done/done", row["outcome"], row["terminal_state"])
	}

	// crew retro renders only runs carrying a note: seed one so the row exists.
	if _, errOut, code := h.run("msg", "worker:feat/role-timeout#s1-1", "retro:c1",
		`{"seam":"review","tag":"other","detail":"layout note"}`); code != 0 {
		t.Fatalf("msg: code %d, stderr %q", code, errOut)
	}
	out := retroOK(t, h)
	if !strings.Contains(out, "done") || strings.Contains(out, "failed") {
		t.Errorf("retro =\n%s\nwant the run classified done, never failed", out)
	}
}

// A `metrics:` message is a synthetic sink: it wakes no watch.
func TestMetricsAddressedMessageDoesNotWakeTheWatch(t *testing.T) {
	h, _, _ := newRateCLI(t)
	t.Setenv("CREW_ID", "c1")
	if _, errOut, code := h.run("msg", "worker", "metrics:c1", `{"tier":"deep"}`); code != 0 {
		t.Fatalf("msg: code %d, stderr %q", code, errOut)
	}
	out, errOut, code := h.run("watch", "--since", "0", "--timeout", "1", "--interval", "1")
	if code != 0 || out != "" {
		t.Errorf("watch: code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// The five `rate:` cases of the bats suite, ported: each drives
// `crew rate` and reads back the ratings rows it appended.

// rateRows reads the ratings store: one object per line, one per swept run.
func rateRows(t *testing.T, store string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("store line %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// rateSweep runs the sweep, requiring a clean exit and empty stdout, and returns
// the rows it appended. Its stderr carries the burn-table warning whenever
// DISPATCH_CONFIG_BIN is unresolvable, which none of these pin.
func rateSweep(t *testing.T, h *holdCLI, store string) []map[string]any {
	t.Helper()
	out, errOut, code := h.run("rate")
	if code != 0 || out != "" {
		t.Fatalf("rate: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	return rateRows(t, store)
}

// rateRow is the one row swept for a branch, failing on any other count.
func rateRow(t *testing.T, rows []map[string]any, branch string) map[string]any {
	t.Helper()
	var got []map[string]any
	for _, row := range rows {
		if row["branch"] == branch {
			got = append(got, row)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%s: %d rows, want exactly one of %v", branch, len(got), rows)
	}
	return got[0]
}

// `replanned` is projected as a real boolean, and a metrics snapshot written
// before the field existed keeps the key with a null value: a reader has to be
// able to tell "never replanned" from "too old to say".
func TestRateProjectsReplannedBooleansAndLegacyNull(t *testing.T) {
	h, seed, store := newRateCLI(t)
	cases := []struct {
		branch string
		ts     int
		body   string
		// replanned is what the field must hold; rework what it must not disturb.
		replanned any
		rework    float64
	}{
		{"feat/replan-true", 1000, `{"replanned":true,"rework_count":3}`, true, 3},
		{"feat/replan-false", 2000, `{"replanned":false,"rework_count":1}`, false, 1},
		{"feat/replan-legacy", 3000, `{"rework_count":2}`, nil, 2},
	}
	for _, tc := range cases {
		seed.dispatch(tc.branch, tc.ts, nil)
		seed.status("worker:"+tc.branch, tc.ts+1, "done")
		seed.msg("worker:"+tc.branch, "metrics:c1", tc.ts+2, tc.body)
	}
	t.Setenv("CREW_ID", "c1")

	rows := rateSweep(t, h, store)

	for _, tc := range cases {
		row := rateRow(t, rows, tc.branch)
		v, ok := row["replanned"]
		if !ok || v != tc.replanned {
			t.Errorf("%s: replanned = %v (present %v), want %v", tc.branch, v, ok, tc.replanned)
		}
		if row["rework_count"] != tc.rework {
			t.Errorf("%s: rework_count = %v, want %v", tc.branch, row["rework_count"], tc.rework)
		}
	}
}

// A sessioned worker id (`worker:<branch>#<session>`) is still the branch's run,
// and the session the dispatch recorded lands on the row beside it.
func TestRateSweepsSessionedWorkerAndRecordsItsSession(t *testing.T) {
	h, seed, store := newRateCLI(t)
	seed.dispatch("feat/x", 1000, map[string]any{"session": "s1-1", "title": "T"})
	t.Setenv("CREW_ID", "c1")
	if out, errOut, code := h.run("status", "worker:feat/x#s1-1", "pr_open", "", "https://example.com/pr/1"); code != 0 {
		t.Fatalf("status: code %d, stdout %q, stderr %q", code, out, errOut)
	}

	row := rateRow(t, rateSweep(t, h, store), "feat/x")

	if row["session"] != "s1-1" || row["reached_pr"] != true {
		t.Errorf("session/reached_pr = %v/%v, want s1-1/true", row["session"], row["reached_pr"])
	}
}

// One unparseable metrics body folds to null metrics for its own run; it must
// not abort the sweep and take every other run of the window with it (#25).
func TestRateOneUnparseableMetricsBodyDoesNotKillTheSweep(t *testing.T) {
	h, seed, store := newRateCLI(t)
	seed.dispatch("feat/bad", 1000, nil)
	seed.status("worker:feat/bad", 1001, "done")
	seed.msg("worker:feat/bad", "metrics:c1", 1002, `{"rework_count":3,"detail":"trunc …[elided]`)
	seed.dispatch("feat/good", 2000, nil)
	seed.status("worker:feat/good", 2001, "done")
	seed.msg("worker:feat/good", "metrics:c1", 2002, `{"rework_count":7}`)
	t.Setenv("CREW_ID", "c1")

	rows := rateSweep(t, h, store)

	if got := rateRow(t, rows, "feat/bad")["rework_count"]; got != nil {
		t.Errorf("feat/bad: rework_count = %v, want null", got)
	}
	if got := rateRow(t, rows, "feat/good")["rework_count"]; got != float64(7) {
		t.Errorf("feat/good: rework_count = %v, want 7", got)
	}
}

// `blocked_count` counts only the worker's own blocks; a watchdog block goes to
// `watchdog_blocked_count`, except a `budget:` row, which is a host-wide quota
// crossing and counts in neither.
func TestRateBlockedCountExcludesWatchdogBlocks(t *testing.T) {
	h, seed, store := newRateCLI(t)
	seed.dispatch("feat/x", 1, map[string]any{"model": "opus", "tier": "deep", "effort": "high", "title": "t"})
	worker := "worker:feat/x"
	seed.statusBody(worker, 2, map[string]any{"state": "blocked", "detail": "which approach?"})
	seed.statusBody(worker, 3, map[string]any{"state": "blocked", "detail": "prompt: interactive prompt in pane %9 — waiting", "source": "watchdog"})
	seed.statusBody(worker, 4, map[string]any{"state": "blocked", "detail": "quiet: pane unchanged for 1800s", "source": "watchdog"})
	seed.statusBody(worker, 5, map[string]any{"state": "blocked", "detail": "budget: codex 5h at 100% (resets x)", "source": "watchdog"})
	t.Setenv("CREW_ID", "c1")

	row := rateRow(t, rateSweep(t, h, store), "feat/x")

	if row["blocked_count"] != float64(1) || row["watchdog_blocked_count"] != float64(2) {
		t.Errorf("blocked/watchdog_blocked = %v/%v, want 1/2", row["blocked_count"], row["watchdog_blocked_count"])
	}
}

// Contract pin, not a behaviour test: rate already passes any review_mode string
// through, so this passed before the value existed. It stops a later rate edit
// swallowing `unavailable` — it does not verify a worker emits it.
func TestRateReviewModeUnavailablePassesThrough(t *testing.T) {
	h, seed, store := newRateCLI(t)
	seed.dispatch("feat/review-unavailable", 1000, map[string]any{
		"engine": "codex", "model": "terra", "title": "review gate unavailable"})
	seed.status("worker:feat/review-unavailable", 1001, "failed")
	seed.msg("worker:feat/review-unavailable", "metrics:c1", 1002, `{"review_mode":"unavailable","review_high":null}`)
	t.Setenv("CREW_ID", "c1")

	row := rateRow(t, rateSweep(t, h, store), "feat/review-unavailable")

	if row["review_mode"] != "unavailable" || row["outcome"] != "failed" || row["reached_pr"] != false {
		t.Errorf("review_mode/outcome/reached_pr = %v/%v/%v, want unavailable/failed/false",
			row["review_mode"], row["outcome"], row["reached_pr"])
	}
}
