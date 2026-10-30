package reap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const (
	portAClosingKey = "pr view " + prURL + " --json closingIssuesReferences --jq .closingIssuesReferences[].number"
	portAStateKey   = "pr view " + prURL + " --json state --jq .state"
	portAHeadKey    = "pr view " + prURL + " --json headRefOid --jq .headRefOid"
	portAWinsKey    = "list-windows -a -F #{window_id}\t#{@crew_name}\t#{pane_current_path}"
	portAPanes3Key  = "list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}"
)

func (f *fx) portAExists(path string) bool {
	f.t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

func (f *fx) portABranchExists(branch string) bool {
	return exec.Command("git", "-C", f.main, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// portATip makes the merged PR's head the branch's local tip, so the branch
// delete guard passes.
func (f *fx) portATip() {
	f.t.Helper()
	f.gh[portAHeadKey] = gitOut(f.t, f.main, "rev-parse", "refs/heads/"+rb)
}

func (f *fx) portAWantNoCall(prefix string) {
	f.t.Helper()
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) }) {
		f.t.Fatalf("unexpected %q in calls %q", prefix, f.calls)
	}
}

func (f *fx) portAWantOutLine(line string) {
	f.t.Helper()
	if !slices.Contains(strings.Split(f.stdout.String(), "\n"), line) {
		f.t.Fatalf("stdout lacks %q:\n%s", line, f.stdout.String())
	}
}

// --dry-run reports the reap and leaves the worktree, the bus and the trash
// alone.
func TestPortADryRunKeepsWorktree(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh[portAStateKey] = "MERGED"
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	anchor, _ := wtgit.AnchorPath(f.wt)
	f.wantOut("crew reap: would reap feat/r (MERGED) @ "+f.wt, "crew reap: would prune record "+anchor)
	if !f.portAExists(f.wt) || len(f.appended) != 0 {
		t.Fatalf("dry run acted: appended %q", f.appended)
	}
	f.portAWantNoCall("gtrash")
}

// An engine pane exactly at the worktree keeps it, wrapped or not; a pane at a
// sibling path that merely shares the prefix does not.
func TestPortAEnginePaneAtExactPath(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh[portAStateKey] = "MERGED"

	f.tmux[panesKey] = "@1\t%1\t.claude-wrapped\t" + f.wt
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (empty capture of %1)", "crew reap: nothing reclaimed")
	if !f.portAExists(f.wt) {
		t.Fatal("worktree removed")
	}
	f.portAWantNoCall("tmux kill-window")

	f.tmux[panesKey] = "@1\t%1\t.claude-wrapped\t" + f.wt + "-sibling"
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	anchor, _ := wtgit.AnchorPath(f.wt)
	f.wantOut("crew reap: would reap feat/r (MERGED) @ "+f.wt, "crew reap: would prune record "+anchor)
}

// A terminal session past --idle loses its window and keeps its worktree.
func TestPortAIdleReleaseFailedKeepsWorktree(t *testing.T) {
	f := newFx(t)
	f.bus(status("failed", "", 0))
	f.stubOccupied("fish")
	f.wantOccupied()
	if rc := f.reap(f.main, Request{Idle: "0", Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: released @23 at " + f.wt + " (feat/r failed)")
	if !slices.Contains(f.calls, "tmux kill-window -t @23") || !f.portAExists(f.wt) {
		t.Fatalf("calls %q", f.calls)
	}
	rows := f.rows("release")
	if len(rows) != 1 {
		t.Fatalf("appended %q", f.appended)
	}
	wantJSON(t, rows[0], fmt.Sprintf(`{"ts":%d,"kind":"release","branch":"feat/r","session":"s1","state":"failed","windows":["@23"]}`, nowUnix*1000))
}

// Sessions that must not release a window, however long the idle.
func TestPortAIdleReleaseKeeps(t *testing.T) {
	idle := idleFrame(t)
	oldDone := statusFor(rb, "s1", "done", "", 100)
	newWorking := statusFor(rb, "s2", "working", "", 50)
	cases := []struct {
		name string
		bus  []string
		cmd  string
		idle string
	}{
		{"inside --idle", []string{status("failed", "", 0)}, "fish", "3600"},
		{"non-terminal session", []string{status("working", "", 0)}, "claude", "0"},
		{"newer working session masks an older done", []string{oldDone, newWorking}, "claude", "0"},
		{"watchdog-posted failed", []string{`{"ts":` + strconv.FormatInt((nowUnix-400)*1000, 10) +
			`,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"failed","detail":"dead: quiet: no output","source":"watchdog"}}`}, "claude", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			f.bus(c.bus...)
			f.stubOccupied(c.cmd)
			f.tmux["capture-pane -p -t %33"] = idle
			f.tmux["capture-pane -e -p -t %33"] = idle
			f.wantOccupied()
			if rc := f.reap(f.main, Request{Idle: c.idle}); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			if f.killed() || strings.Contains(f.stdout.String(), "released") || strings.Contains(f.stdout.String(), "pane busy") {
				t.Fatalf("released or sampled: calls %q stdout %q", f.calls, f.stdout.String())
			}
		})
	}
}

// At the default grace a done session older than it is released and a younger
// one is not.
func TestPortADefaultGrace(t *testing.T) {
	f := newFx(t)
	newWt := f.addWorktree("feat/new-done")
	f.bus(status("done", "", 400), statusFor("feat/new-done", "s2", "done", "", 100))
	f.tmux[portAWinsKey] = "@23\tsage\t" + f.wt + "\n@24\tbird\t" + newWt
	f.tmux[portAPanes3Key] = "@23\t%33\tfish\n@24\t%34\tfish"
	if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if !slices.Contains(f.calls, "tmux kill-window -t @23") || slices.Contains(f.calls, "tmux kill-window -t @24") {
		t.Fatalf("calls %q", f.calls)
	}
}

// A live claude turn keeps a done window at the default flags.
func TestPortALiveTurnKeepsAtDefaultFlags(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", "", 400))
	f.stubOccupied("claude")
	live := "  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)\n✳ Perusing… (1m2s · ↓ 3.1k tokens · thinking more with high effort)"
	f.tmux["capture-pane -p -t %33"] = live
	f.tmux["capture-pane -e -p -t %33"] = live
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.portAWantOutLine("crew reap: keeping feat/r — pane busy: %33 is not idle (live turn, unsent input, or no idle input box)")
	if f.killed() {
		t.Fatalf("killed: %q", f.calls)
	}
}

func TestPortAIdleReleaseDryRun(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", "", 0))
	f.stubOccupied("fish")
	if rc := f.reap(f.main, Request{Idle: "0", DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.portAWantOutLine("crew reap: would release @23 at " + f.wt + " (feat/r done)")
	if f.killed() || len(f.rows("release")) != 0 {
		t.Fatalf("dry run acted: calls %q appended %q", f.calls, f.appended)
	}
}

// A pi pane with a selection prompt on screen is not idle.
func TestPortAReleasePiPromptKeeps(t *testing.T) {
	r := newRel(t)
	r.panes = "@5\t%5\tpi"
	r.plain["%5"] = []string{"  2. Gate everything on 3.8\n Enter to select · ↑/↓ to navigate · Esc to cancel"}
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — pane busy: %5 is not idle (pi frame: prompt on screen)")
}

// One busy engine pane keeps a window even when its sibling pane is idle.
func TestPortAReleaseSecondBusyPaneKeeps(t *testing.T) {
	r := newRel(t)
	r.panes = "@5\t%5\tclaude\n@5\t%6\tclaude"
	r.plain["%6"] = []string{"✳ Perusing… (1m2s · ↓ 3.1k tokens · thinking more with high effort)"}
	rc, err := r.release("done", 300, false)
	r.wantKept(rc, err, "keeping feat/x — pane busy: %6 is not idle (live turn, unsent input, or no idle input box)")
}

// A merged PR with no closing issue, or whose closing issues cannot be
// resolved, reaps without a label removal; the failure is reported, not
// mistaken for no closing issue.
func TestPortAClosingIssues(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		f := newFx(t)
		f.bus(status("done", prURL, 0))
		f.gh[portAStateKey] = "MERGED"
		f.gh[portAClosingKey] = ""
		f.portATip()
		if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
			t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
		}
		f.wantOut("crew reap: reaped feat/r (MERGED)")
		f.portAWantNoCall("gh issue edit")
	})
	t.Run("resolution fails", func(t *testing.T) {
		f := newFx(t)
		f.bus(status("done", prURL, 0))
		f.gh[portAStateKey] = "MERGED"
		f.gh[portAClosingKey] = "gh: rate limited"
		f.ghFail[portAClosingKey] = true
		f.portATip()
		if rc := f.reap(f.main, Request{}); rc != 0 {
			t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
		}
		f.wantOut("crew reap: reaped feat/r (MERGED)",
			"crew reap: could not resolve closing issues for PR "+prURL+" (feat/r): gh: rate limited")
		f.portAWantNoCall("gh issue edit")
	})
}

// A squash-merged PR's branch is ahead of main, so it is reaped by outcome:
// reap row, label release, deliberate branch delete.
func TestPortASquashMergedReap(t *testing.T) {
	f := newFx(t)
	if err := os.WriteFile(filepath.Join(f.wt, "work.txt"), []byte("unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.wt, "add", "work.txt")
	runGit(t, f.wt, "commit", "-q", "-m", "work")
	f.bus(status("done", prURL, 0))
	f.gh[portAStateKey] = "MERGED"
	f.gh[portAClosingKey] = "99"
	f.portATip()
	if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (MERGED)")
	for _, c := range []string{"gh " + portAClosingKey, "gh issue edit 99 --remove-label dispatched"} {
		if !slices.Contains(f.calls, c) {
			t.Fatalf("calls %q lack %q", f.calls, c)
		}
	}
	rows := f.rows("reap")
	if len(rows) != 1 {
		t.Fatalf("appended %q", f.appended)
	}
	wantJSON(t, rows[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/r","pr":%q,"pr_state":"MERGED","worktree":%q}`, nowUnix*1000, prURL, f.wt))
	if f.portAExists(f.wt) || f.portABranchExists(rb) {
		t.Fatal("worktree or branch survived")
	}
}

// A merged pr_open worker gets exactly one reaped status row, none under
// --dry-run, and the roster folds it to reaped.
func TestPortAReapedStatusRow(t *testing.T) {
	f := newFx(t)
	f.bus(status("pr_open", prURL, 0))
	f.gh[portAStateKey] = "MERGED"
	f.gh[portAClosingKey] = ""
	f.portATip()
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if len(f.appended) != 0 {
		t.Fatalf("dry run appended %q", f.appended)
	}
	if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	st := f.rows("status")
	if len(st) != 1 {
		t.Fatalf("appended %q", f.appended)
	}
	wantJSON(t, st[0], fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"reaped","detail":"PR #7 MERGED","source":"reap"}}`, nowUnix*1000))

	var events []any
	raw, err := os.ReadFile(f.paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range append(strings.Split(strings.TrimSpace(string(raw)), "\n"), f.appended...) {
		var ev any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, ev)
	}
	probes := roster.Probes{
		Tmux:      &tmuxtest.Fake{Func: func([]string) (string, error) { return "", nil }},
		Worktrees: func() (string, error) { return "", nil },
	}
	rows, err := roster.Fold(context.Background(), events, "c1", float64(nowUnix), probes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rows, func(r map[string]any) bool { return r["branch"] == rb && r["state"] == "reaped" }) {
		t.Fatalf("roster rows %v", rows)
	}
}

// The reaped branch's per-branch records go; its artifacts, a branch whose
// name merely prefixes it, and one it prefixes stay.
func TestPortAReapForgetsRecords(t *testing.T) {
	f := newFx(t)
	dir := f.paths.Dir
	const other, longer = "feat/re", "feat/r-too"
	f.bus(status("pr_open", prURL, 0), statusFor(other, "s2", "working", "", 0))
	f.gh[portAStateKey] = "MERGED"
	f.gh[portAClosingKey] = ""
	f.portATip()
	touch := func(p, data string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	seed := func(branch, id string) []string {
		cks := strconv.FormatUint(uint64(identity.CKsum([]byte(id))), 10)
		return []string{
			touch(dir+"/grants/"+branch, ""),
			touch(dir+"/protocol-dirs/"+branch, ""),
			touch(dir+"/leads/"+branch, ""),
			touch(marks.Path(dir, "c1", id), ""),
			touch(marks.LockPath(dir, "c1", id), ""),
			touch(dir+"/phase-status/"+sanitize(id)+"."+cks, ""),
			touch(dir+"/directive-delivery/"+sanitize(id), "0\n"+id+"\n"),
		}
	}
	gone := seed(rb, "worker:feat/r#s1")
	kept := append(seed(other, "worker:feat/re#s2"), seed(longer, "worker:feat/r-too#s3")...)
	kept = append(kept, touch(dir+"/artifacts/"+rb+"/x.patch", ""))

	if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if f.portAExists(f.wt) {
		t.Fatal("worktree not reaped")
	}
	for _, p := range gone {
		if f.portAExists(p) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range kept {
		if !f.portAExists(p) {
			t.Errorf("%s removed", p)
		}
	}
}
