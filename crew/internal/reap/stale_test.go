package reap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	issueView  = "issue view 5 --json state --jq .state"
	threeDays  = int64(3*24*3600 + 60)
	winListKey = "list-windows -a -F #{window_id}\t#{@crew_branch}\t#{@crew_dir}"
)

func claim5() string {
	return `{"ts":1,"crew_id":"c1","kind":"claim-issue","branch":"` + rb + `","issue":5}`
}

// dirty is a merged-PR worker whose tree holds uncommitted work.
func (f *fx) dirty(state string, ageSec int64, extra ...string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.wt, "notes.txt"), []byte("x"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.portBGh("MERGED")
	f.bus(append([]string{status(state, prURL, ageSec)}, extra...)...)
}

func (f *fx) wantDirtyKept() {
	f.t.Helper()
	if _, err := os.Stat(filepath.Join(f.wt, "notes.txt")); err != nil {
		f.t.Fatalf("dirty worktree was touched: %v", err)
	}
}

func (f *fx) staleRows() []map[string]any {
	f.t.Helper()
	var rows []map[string]any
	for _, line := range f.appended {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			f.t.Fatal(err)
		}
		if m["kind"] == "stale" {
			rows = append(rows, m)
		}
	}
	return rows
}

func (f *fx) inspectCmd() string {
	return "git -C '" + f.wt + "' status --short; git -C '" + f.wt + "' log --oneline '" + rb + "' --not --remotes"
}

func (f *fx) removeCmd() string {
	common := f.paths.Common
	return "git -C '" + common + "' worktree remove --force '" + f.wt + "' && git -C '" + common + "' branch -D '" + rb + "'"
}

func TestStaleDirtyClosedIssue(t *testing.T) {
	f := newFx(t)
	f.dirty("done", 0, claim5())
	f.gh[issueView] = "CLOSED"
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	f.wantOut(
		"crew reap: keeping feat/r — uncommitted changes",
		"crew reap:   stale: needs human cleanup — uncommitted changes; issue closed",
		"crew reap:   inspect: "+f.inspectCmd(),
		"crew reap:   remove:  "+f.removeCmd(),
		"crew reap: nothing reclaimed",
	)
	f.wantDirtyKept()
	rows := f.staleRows()
	if len(rows) != 1 {
		t.Fatalf("stale rows %v", f.appended)
	}
	row := rows[0]
	if row["stale"] != true || row["branch"] != rb || row["crew_id"] != "c1" || row["session"] != "worker:feat/r#s1" ||
		row["reason"] != "uncommitted changes; issue closed" || row["worktree"] != f.wt ||
		row["inspect"] != f.inspectCmd() || row["remove"] != f.removeCmd() {
		t.Fatalf("row %v", row)
	}
}

func TestStaleDirtyOld(t *testing.T) {
	f := newFx(t)
	f.dirty("failed", threeDays, claim5())
	f.gh[issueView] = "OPEN"
	f.reap(f.main, Request{})
	if !strings.Contains(f.stdout.String(), "stale: needs human cleanup — uncommitted changes; older than 72h") {
		t.Fatalf("stdout %s", f.stdout.String())
	}
	f.wantDirtyKept()

	f.dirty("failed", threeDays-120, claim5())
	f.appended = nil
	f.reap(f.main, Request{})
	if len(f.staleRows()) != 0 {
		t.Fatalf("under the threshold went stale: %v", f.appended)
	}
	f.reap(f.main, Request{StaleAfter: "60"})
	if !strings.Contains(f.stdout.String(), "uncommitted changes; older than 60s") {
		t.Fatalf("stdout %s", f.stdout.String())
	}
}

func TestStaleExitedDirtyInPRMode(t *testing.T) {
	f := newFx(t)
	f.dirty("exited", 0, claim5())
	f.gh[issueView] = "CLOSED"
	f.reap(f.main, Request{})
	if len(f.staleRows()) != 1 {
		t.Fatalf("stdout %s", f.stdout.String())
	}
}

func TestStaleNotStale(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fx)
	}{
		{"fresh failed with open issue", func(f *fx) {
			f.dirty("failed", 0, claim5())
			f.gh[issueView] = "OPEN"
		}},
		{"unreadable issue fails closed", func(f *fx) {
			f.dirty("done", 0, claim5())
		}},
		{"no claim row", func(f *fx) { f.dirty("done", 0) }},
		{"non-numeric claim", func(f *fx) {
			f.dirty("done", 0, `{"ts":1,"kind":"claim-issue","branch":"`+rb+`","issue":"x1"}`)
		}},
		{"open PR", func(f *fx) {
			f.dirty("done", threeDays, claim5())
			f.portBGh("OPEN")
		}},
		{"engine pane", func(f *fx) {
			f.dirty("done", threeDays, claim5())
			f.tmux["list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"] = "@1\t%1\tclaude\t" + f.wt
		}},
		{"working worker", func(f *fx) { f.dirty("working", threeDays, claim5()) }},
		{"blocked worker", func(f *fx) { f.dirty("blocked", threeDays, claim5()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			tc.setup(f)
			f.reap(f.main, Request{})
			if strings.Contains(f.stdout.String(), "stale:") || len(f.staleRows()) != 0 {
				t.Fatalf("stdout %s rows %v", f.stdout.String(), f.appended)
			}
			f.wantDirtyKept()
		})
	}
}

func TestStaleIssueModeKeeps(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", "", threeDays), claim5())
	f.gh["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] = "[]"
	f.gh[issueView] = "OPEN"
	f.reap(f.main, Request{})
	if !strings.Contains(f.stdout.String(), "done but no PR on the bus or GitHub; issue #5 is OPEN; older than 72h") {
		t.Fatalf("stdout %s", f.stdout.String())
	}

	f.gh[issueView] = "CLOSED"
	f.appended = nil
	f.reap(f.main, Request{})
	rows := f.staleRows()
	if len(rows) != 1 || !strings.HasSuffix(rows[0]["reason"].(string), "no origin default branch ref; issue closed") {
		t.Fatalf("rows %v\nstdout %s", f.appended, f.stdout.String())
	}

	// With a default ref, an unpushed commit is the keep and inspect is unchanged.
	runGit(t, f.main, "update-ref", "refs/remotes/origin/main", "main")
	f.portBCommit("a.txt", "a")
	f.appended = nil
	f.reap(f.main, Request{})
	rows = f.staleRows()
	if len(rows) != 1 || rows[0]["inspect"] != f.inspectCmd() {
		t.Fatalf("rows %v\nstdout %s", f.appended, f.stdout.String())
	}
	if _, err := os.Stat(f.wt); err != nil {
		t.Fatal(err)
	}
}

func TestStaleDryRunAndJSON(t *testing.T) {
	f := newFx(t)
	f.dirty("done", 0, claim5())
	f.gh[issueView] = "CLOSED"
	f.reap(f.main, Request{DryRun: true})
	for _, want := range []string{"stale: needs human cleanup", "inspect: " + f.inspectCmd(), "remove:  " + f.removeCmd()} {
		if !strings.Contains(f.stdout.String(), want) {
			t.Fatalf("missing %q in %s", want, f.stdout.String())
		}
	}
	if len(f.appended) != 0 {
		t.Fatalf("dry-run wrote %v", f.appended)
	}

	f.reap(f.main, Request{DryRun: true, JSON: true})
	var doc struct{ Keeps []Keep }
	if err := json.Unmarshal(f.stdout.Bytes(), &doc); err != nil || len(doc.Keeps) != 1 {
		t.Fatalf("json %q: %v", f.stdout.String(), err)
	}
	k := doc.Keeps[0]
	if !k.Stale || k.StaleReason != "uncommitted changes; issue closed" || k.Worktree != f.wt || k.Inspect != f.inspectCmd() || k.Remove != f.removeCmd() {
		t.Fatalf("keep %+v", k)
	}

	f.gh[issueView] = "OPEN"
	f.reap(f.main, Request{DryRun: true, JSON: true})
	if strings.Contains(f.stdout.String(), "stale") {
		t.Fatalf("non-stale keep carries stale fields: %s", f.stdout.String())
	}
	f.reap(f.main, Request{JSON: false, Quiet: true})
	if f.stdout.Len() != 0 {
		t.Fatalf("--quiet printed %s", f.stdout.String())
	}
}

func TestStaleRowDedupeAndClear(t *testing.T) {
	f := newFx(t)
	f.dirty("done", 1, claim5())
	f.gh[issueView] = "CLOSED"
	f.reap(f.main, Request{})
	rows := f.staleRows()
	if len(rows) != 1 {
		t.Fatalf("first run rows %v", f.appended)
	}
	posted := f.appended[0]

	// The same reason, row newer than the status: nothing more is posted.
	f.bus(status("done", prURL, 1), claim5(), posted)
	f.appended = nil
	f.reap(f.main, Request{})
	if len(f.appended) != 0 {
		t.Fatalf("rerun posted %v", f.appended)
	}

	// Kept for another reason now: the standing row is cleared.
	f.gh[issueView] = "OPEN"
	f.reap(f.main, Request{})
	rows = f.staleRows()
	if len(rows) != 1 || rows[0]["stale"] != false || rows[0]["branch"] != rb || rows[0]["reason"] != nil {
		t.Fatalf("clear rows %v", f.appended)
	}

	// A stale row older than the status is not standing, so nothing is cleared.
	f.bus(status("done", prURL, 1), claim5(), strings.Replace(posted, `"ts":`, `"ts":1,"x":`, 1))
	f.appended = nil
	f.reap(f.main, Request{})
	if len(f.appended) != 0 {
		t.Fatalf("cleared a superseded row: %v", f.appended)
	}
}

// ghostFx is a finished worker whose worktree was removed by hand.
func ghostFx(t *testing.T, extra ...string) *fx {
	t.Helper()
	f := newFx(t)
	f.bus(append([]string{status("done", prURL, 0)}, extra...)...)
	runGit(t, f.main, "worktree", "remove", "--force", f.wt)
	return f
}

func (f *fx) reapedRows() []map[string]any {
	f.t.Helper()
	var rows []map[string]any
	for _, line := range f.appended {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			f.t.Fatal(err)
		}
		if body, _ := m["body"].(map[string]any); body["state"] == "reaped" {
			rows = append(rows, m)
		}
	}
	return rows
}

func TestGhostRetired(t *testing.T) {
	f := ghostFx(t, `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"`+rb+`","host":"mini"}`)
	f.reap(f.main, Request{DryRun: true})
	f.wantOut("crew reap: would retire feat/r (worktree already gone)")
	if len(f.appended) != 0 {
		t.Fatalf("dry-run wrote %v", f.appended)
	}

	f.reap(f.main, Request{})
	f.wantOut("crew reap: retired feat/r (worktree already gone)")
	rows := f.reapedRows()
	if len(rows) != 1 {
		t.Fatalf("rows %v", f.appended)
	}
	body := rows[0]["body"].(map[string]any)
	if body["source"] != "reap" || body["detail"] != "worktree already gone" || rows[0]["from"] != "worker:feat/r#s1" || rows[0]["crew_id"] != "c1" {
		t.Fatalf("row %v", rows[0])
	}

	// Once the reaped row is on the bus the branch is no candidate.
	f.bus(status("done", prURL, 0), f.appended[0])
	f.appended = nil
	f.reap(f.main, Request{})
	f.wantOut("crew reap: nothing done to reap")
	if len(f.appended) != 0 {
		t.Fatalf("rerun wrote %v", f.appended)
	}
}

func TestGhostSkipped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		setup func(f *fx)
	}{
		{"remote dispatch", []string{`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"` + rb + `","host":"mini","remote":true}`}, nil},
		{"stamped window", nil, func(f *fx) { f.tmux[winListKey] = "@2\t" + rb + "\t" + f.paths.Dir }},
		{"detached worktree", nil, func(f *fx) {
			runGit(t, f.main, "worktree", "add", "-q", "--detach", filepath.Join(f.root, "det"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ghostFx(t, tc.extra...)
			if tc.setup != nil {
				tc.setup(f)
			}
			f.reap(f.main, Request{})
			if len(f.reapedRows()) != 0 || strings.Contains(f.stdout.String(), "retire") {
				t.Fatalf("retired: %v\n%s", f.appended, f.stdout.String())
			}
		})
	}
}

// noPRFx is a worker with no PR anywhere: portEIssue's commit is replaced by
// the bus rows the case names, and the tree is clean with no default ref, so
// the keep is the unpushed-work one only when an age or closed issue allows it.
func noPRFx(t *testing.T, state string, ageSec int64, extra ...string) *fx {
	t.Helper()
	f := newFx(t)
	f.bus(append([]string{statusFor(rb, "s1", state, "", ageSec)}, extra...)...)
	f.gh[portEListKey] = "[]"
	return f
}

func TestStaleNoPRDefiniteStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		age   int64
		extra []string
		want  string
	}{
		{"done no claim row old", "done", threeDays, nil, "no claim-issue row; older than 72h"},
		{"done no claim row fresh", "done", 0, nil, ""},
		{"exited no PR old", "exited", threeDays, nil, "only done/failed reclaim on a closed issue; older than 72h"},
		{"exited no PR fresh", "exited", 0, nil, ""},
		{"non-numeric claim old", "done", threeDays, []string{`{"ts":1,"kind":"claim-issue","branch":"` + rb + `","issue":"x1"}`}, "non-numeric issue; older than 72h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := noPRFx(t, tc.state, tc.age, tc.extra...)
			f.reap(f.main, Request{})
			rows := f.staleRows()
			if tc.want == "" {
				if len(rows) != 0 {
					t.Fatalf("went stale: %v", f.appended)
				}
				return
			}
			if len(rows) != 1 || !strings.HasSuffix(rows[0]["reason"].(string), tc.want) {
				t.Fatalf("rows %v\nstdout %s", f.appended, f.stdout.String())
			}
		})
	}
}

func TestStaleNoPRUnreadableFailsClosed(t *testing.T) {
	f := noPRFx(t, "done", threeDays, claim5())
	f.reap(f.main, Request{})
	if len(f.staleRows()) != 0 {
		t.Fatalf("unreadable issue went stale: %v", f.appended)
	}
}

func TestGhostLiveWorktreeNotRetired(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(t *testing.T, f *fx)
	}{
		{"space in path", func(t *testing.T, f *fx) {
			runGit(t, f.main, "worktree", "move", f.wt, filepath.Join(f.root, "my wt"))
		}},
		{"tree switched to another branch", func(t *testing.T, f *fx) {
			slug := filepath.Join(f.root, "feat-r")
			runGit(t, f.main, "worktree", "move", f.wt, slug)
			runGit(t, slug, "switch", "-q", "-c", "other")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			f.bus(status("done", prURL, 0))
			tc.move(t, f)
			f.reap(f.main, Request{})
			if len(f.reapedRows()) != 0 || strings.Contains(f.stdout.String(), "retire") {
				t.Fatalf("retired: %v\n%s", f.appended, f.stdout.String())
			}
		})
	}
}
