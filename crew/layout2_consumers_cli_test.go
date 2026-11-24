package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

const doneRow1 = `{"ts":1785951264000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`
const doneRow2 = `{"ts":1785951265000,"crew_id":"c1","from":"worker:feat/y","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`

// homeCommon is the real path of the test repo's git common dir.
func homeCommon(t *testing.T, c *lifecycleCLI) string {
	t.Helper()
	common, err := filepath.EvalSymlinks(filepath.Join(c.repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return common
}

// homeCrewCLI is newRemoteCLI with c1's meta home_repo set to the test repo (or
// null), and the process cwd outside any repo.
func homeCrewCLI(t *testing.T, withHome bool) *lifecycleCLI {
	t.Helper()
	c := newRemoteCLI(t)
	if withHome {
		common := homeCommon(t, c)
		m := bus.Meta{Layout: 2, ID: "c1", Created: 1700000000000, HomeHost: "halo", CreatedBy: "register", HomeRepo: &common}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.store("c1")+"/meta.json", append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(c.outside)
	return c
}

func appendTo(t *testing.T, path, rows string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(rows); err != nil {
		t.Fatal(err)
	}
}

func TestLayout2ConsumersReplyOutsideRepo(t *testing.T) {
	c := homeCrewCLI(t, false)
	appendTo(t, c.store("c1")+"/events.jsonl", `{"ts":2,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`+"\n")
	_, errOut := c.expectIn(c.outside, 0, "reply", "--crew", "c1", "--no-nudge", "worker:feat/x", "go ahead")
	log, err := os.ReadFile(c.store("c1") + "/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"go ahead"`) {
		t.Fatalf("crew log %q (stderr %q), want the seeded rows plus the reply", log, errOut)
	}
	if _, err := os.Stat(filepath.Join(c.repo, ".git", "crew")); err == nil {
		t.Errorf("reply created a repo crew dir")
	}
}

func TestLayout2ConsumersMergeRunsInHomeRepo(t *testing.T) {
	c := homeCrewCLI(t, true)
	common := homeCommon(t, c)
	stubs := t.TempDir()
	cwdLog := filepath.Join(stubs, "cwd.log")
	t.Setenv("GH_CWD_LOG", cwdLog)
	writeStub(t, stubs, "gh", "#!/bin/sh\npwd -P >>\"$GH_CWD_LOG\"\nexit 0\n")
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

	c.expectIn(c.outside, 1, "merge", "--dry-run", "--crew", "c1", "12")
	got := strings.Fields(stubLog(t, cwdLog))
	if len(got) == 0 || got[0] != common {
		t.Fatalf("gh cwd %v, want %s", got, common)
	}
}

func TestLayout2ConsumersMergeNullHomeRefusesBeforeGh(t *testing.T) {
	c := homeCrewCLI(t, false)
	_, errOut := c.expectIn(c.outside, 1, "merge", "--dry-run", "--crew", "c1", "12")
	if !strings.Contains(errOut, "no home repo") {
		t.Fatalf("stderr %q, want the no-home-repo refusal", errOut)
	}
	if got := stubLog(t, c.ghLog); got != "" {
		t.Errorf("gh was called: %s", got)
	}
}

func TestLayout2ConsumersNudgeResolvesWindowOutsideRepo(t *testing.T) {
	c := homeCrewCLI(t, true)
	t.Setenv("TMUX_WINDOWS", "@1\tfeat/x\t"+homeCommon(t, c)+"/crew\tc1\tsage")
	_, errOut := c.expectIn(c.outside, 1, "nudge", "--crew", "c1", "feat/x")
	if !strings.Contains(errOut, "no pane in the window") {
		t.Fatalf("stderr %q, want the window resolved (no pane in it)", errOut)
	}

	n := homeCrewCLI(t, false)
	_, errOut = n.expectIn(n.outside, 1, "nudge", "--crew", "c1", "feat/x")
	if !strings.Contains(errOut, "no home repo") {
		t.Fatalf("null home_repo: stderr %q, want the refusal", errOut)
	}
}

func TestLayout2ConsumersWatchOutsideRepoDeliversOncePerRestart(t *testing.T) {
	c := homeCrewCLI(t, false)
	log := c.store("c1") + "/events.jsonl"
	appendTo(t, log, doneRow1+"\n")
	// A layout-1 cursor for the same id must never be read.
	stray := filepath.Join(homeCommon(t, c), "crew", "crews", "c1")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "cursor"), []byte("99999999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	count := func() int {
		out, errOut := c.expectIn(c.outside, 0, "watch", "--crew", "c1", "--timeout", "1", "--interval", "1")
		var batch struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal([]byte(out), &batch); err != nil {
			t.Fatalf("batch %q (stderr %q): %v", out, errOut, err)
		}
		return len(batch.Events)
	}
	if n := count(); n != 1 {
		t.Fatalf("first watch delivered %d events, want 1", n)
	}
	for _, f := range []string{"cursor"} {
		if _, err := os.Stat(filepath.Join(c.store("c1"), f)); err != nil {
			t.Errorf("%s not in the crew dir: %v", f, err)
		}
	}
	appendTo(t, log, doneRow2+"\n")
	if n := count(); n != 1 {
		t.Fatalf("after restart delivered %d events, want only the new one", n)
	}
	if _, err := os.Stat(filepath.Join(c.repo, ".git", "crew", "crews", "c1", "watch.lock.d")); err == nil {
		t.Errorf("watch locked under the repo crew dir")
	}
}

func TestLayout2ConsumersStreamSecondLaneRefused(t *testing.T) {
	c := homeCrewCLI(t, false)
	lockd := filepath.Join(c.store("c1"), "stream.lock.d")
	if !lock.Acquire(lockd, "1") {
		t.Fatal("could not take the lane lock")
	}
	t.Cleanup(func() { lock.Release(lockd) })
	_, errOut := c.expectIn(c.outside, 1, "stream", "--crew", "c1")
	if !strings.Contains(errOut, "another stream is already running") {
		t.Fatalf("stderr %q, want the second-lane refusal", errOut)
	}
}

func TestLayout2ConsumersLayout1WatchCursorPathUnchanged(t *testing.T) {
	c := newLifecycleCLI(t)
	c.appendLog(strings.ReplaceAll(doneRow1, "c1", "legacy") + "\n")
	c.expectIn(c.repo, 0, "watch", "--crew", "legacy", "--timeout", "1", "--interval", "1")
	if _, err := os.Stat(filepath.Join(c.busDir(), "crews", "legacy", "cursor")); err != nil {
		t.Fatalf("layout-1 cursor: %v", err)
	}
}

func TestLayout2ConsumersReplyNudgeWithoutHomeRepoSaysSo(t *testing.T) {
	c := homeCrewCLI(t, false)
	appendTo(t, c.store("c1")+"/events.jsonl",
		`{"ts":2,"crew_id":"c1","kind":"dispatch","worker_id":"worker:feat/x#s1-1","engine":"claude","branch":"feat/x"}`+"\n"+
			`{"ts":3,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`+"\n")
	_, errOut := c.expectIn(c.outside, 0, "reply", "--crew", "c1", "worker:feat/x", "go ahead")
	if !strings.Contains(errOut, "reply: no nudge (start failed: layout-2 crew with no home repo") {
		t.Fatalf("stderr %q, want a no-nudge line instead of a started wait", errOut)
	}
}

func TestReplyNudgeStarterByRepoAndHome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		located bool
		home    bus.Paths
		starts  bool
	}{
		{"in a repo", true, bus.Paths{}, true},
		{"outside a repo with a home repo", false, bus.Paths{Common: "/x"}, true},
		{"outside a repo, no home repo", false, bus.Paths{}, false},
	} {
		started := false
		err := replyNudge(tc.located, tc.home, func([]string, string) error { started = true; return nil })(nil, "")
		if started != tc.starts || (err == nil) != tc.starts {
			t.Errorf("%s: started %v err %v, want started %v", tc.name, started, err, tc.starts)
		}
	}
}
