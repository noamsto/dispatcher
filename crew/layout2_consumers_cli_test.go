package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

const doneRow1 = `{"ts":1785951264000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`
const doneRow2 = `{"ts":1785951265000,"crew_id":"c1","from":"worker:feat/y","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`

// visitingBranchRow is the visiting crew's dispatch row for feat/x, naming the
// remote clone whose crew dir its tmux window is stamped with.
const visitingBranchRow = `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/x","repo":"/some/clone"}` + "\n"

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

	// A visiting crew has no home repo: its window carries the remote clone's
	// crew dir, which its own dispatch row for the branch has to name.
	n := homeCrewCLI(t, false)
	appendTo(t, n.store("c1")+"/events.jsonl", visitingBranchRow)
	t.Setenv("TMUX_WINDOWS", "@1\tfeat/x\t/some/clone/crew\tc1\tsage")
	_, errOut = n.expectIn(n.outside, 1, "nudge", "--crew", "c1", "feat/x")
	if !strings.Contains(errOut, "no pane in the window") {
		t.Fatalf("null home_repo: stderr %q, want the window resolved (no pane in it)", errOut)
	}
}

// visitingTmux stubs tmux ahead of the PATH one with a window and pane list,
// like a remote host whose tmux holds the visiting crew's window. It also
// answers the arm's own display-message read with the server identity.
func visitingTmux(t *testing.T, windows, panes string) {
	t.Helper()
	stubs := t.TempDir()
	writeStub(t, stubs, "tmux", `#!/bin/sh
case "$1 $2" in
"list-windows -a") printf '%s\n' "$TMUX_WINDOWS" ;;
"list-panes -a") printf '%s\n' "$TMUX_PANES" ;;
"display-message -p") printf '%s\n' "$TMUX_SERVER" ;;
esac
exit 0
`)
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_WINDOWS", windows)
	t.Setenv("TMUX_PANES", panes)
	t.Setenv("TMUX_SERVER", tmuxServerIdentity)
}

func TestLayout2ConsumersVisitingCrewWhereAndNudgeOutsideRepo(t *testing.T) {
	const foreign = "/some/clone/crew"
	c := homeCrewCLI(t, false)
	appendTo(t, c.store("c1")+"/events.jsonl", visitingBranchRow)
	visitingTmux(t,
		"@1\tfeat/x\t"+foreign+"\tc1\tsage\tsess\t3\twin\t\t\n@2\tfeat/x\t"+foreign+"\tother\tmuse\tsess\t4\twin2\t\t",
		"@1\t%9\tlead\t1\n@2\t%8\tlead\t1")

	out, errOut := c.expectIn(c.outside, 0, "where", "--crew", "c1", "--", "feat/x")
	if want := `sage — sess:3.1 "win" (lead pane)   jump: ! tmux switch-client -t %9` + "\n"; out != want {
		t.Fatalf("where stdout %q (stderr %q), want %q", out, errOut, want)
	}

	// The dispatch row has no status row: reaching that refusal proves the
	// dispatch-row-bound window match resolved the lead pane.
	_, errOut = c.expectIn(c.outside, 2, "nudge", "--crew", "c1", "--", "feat/x")
	if !strings.Contains(errOut, "lead has not posted its first status yet") {
		t.Fatalf("nudge stderr %q, want the session refusal", errOut)
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

// A reply from outside a repo to a crew with no home repo still starts the one
// wait: the detached `crew nudge` resolves the lead's window by crew id. CREW_SELF
// is a stub, so no real crew process starts.
func TestLayout2ConsumersReplyStartsNudgeWaitWithoutHomeRepo(t *testing.T) {
	c := homeCrewCLI(t, false)
	argvFile := filepath.Join(t.TempDir(), "argv")
	stub := filepath.Join(t.TempDir(), "crew-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"$*\" >\""+argvFile+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREW_SELF", stub)
	appendTo(t, c.store("c1")+"/events.jsonl",
		`{"ts":2,"crew_id":"c1","kind":"dispatch","worker_id":"worker:feat/x#s1-1","engine":"claude","branch":"feat/x"}`+"\n"+
			`{"ts":3,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`+"\n")
	_, errOut := c.expectIn(c.outside, 0, "reply", "--crew", "c1", "worker:feat/x", "go ahead")
	if !strings.Contains(errOut, "reply: nudge wait started for feat/x") {
		t.Fatalf("stderr %q, want the wait started", errOut)
	}
	var got []byte
	for range 100 {
		var err error
		if got, err = os.ReadFile(argvFile); err == nil && len(got) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if want := "nudge --crew c1 --wait -- worker:feat/x#s1-1"; strings.TrimSpace(string(got)) != want {
		t.Fatalf("stub argv %q, want %q", got, want)
	}
}
