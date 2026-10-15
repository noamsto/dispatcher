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
