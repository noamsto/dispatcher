package reap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/migrate/release"
)

// migratedFx is rb moved to host mini by crew migrate: a local dispatch row,
// the mark at rb's tip with tree, and the confirmed remote resume on mini.
// origin is a bare repo holding rb at the tip, its tracking ref fetched.
type migratedFx struct {
	*fx
	tip, bare string
}

func newMigratedFx(t *testing.T) *migratedFx {
	t.Helper()
	f := &migratedFx{fx: newFx(t)}
	f.bare = f.bareOrigin()
	f.tip = gitOut(t, f.main, "rev-parse", "refs/heads/"+rb)
	runGit(t, f.main, "update-ref", "refs/remotes/origin/"+rb, f.tip)
	return f
}

func localDispatch() string {
	return `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"` + rb + `","session":"s1","worker_id":"worker:` + rb + `#s1"}`
}

func remoteDispatch(host string) string {
	return fmt.Sprintf(`{"ts":4000,"crew_id":"c1","kind":"dispatch","branch":%q,"session":"s9-1","worker_id":"worker:%s#s9-1","host":%q,"remote":true}`, rb, rb, host)
}

// markRow is crew migrate mark's row; source "" is a worker's own migrated.
func markRow(tip, tree, source string) string {
	src := ""
	if source != "" {
		src = `,"source":"` + source + `"`
	}
	return fmt.Sprintf(`{"ts":3000,"crew_id":"c1","from":"worker:%s#s1","to":"dispatcher:c1","kind":"status","body":{"state":"migrated","to_host":"mini"%s,"mode":"now","tip":%q,"tree":%q,"patch":"","detail":"moved to mini"}}`,
		rb, src, tip, tree)
}

func workingRow() string {
	return `{"ts":2000,"crew_id":"c1","from":"worker:` + rb + `#s1","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}`
}

// moved writes the bus: the local run, the mark at tip with tree, then rest.
func (f *migratedFx) moved(tip, tree string, rest ...string) {
	f.t.Helper()
	f.bus(append([]string{localDispatch(), workingRow(), markRow(tip, tree, "migrate")}, rest...)...)
}

func (f *migratedFx) tipTree() string {
	return gitOut(f.t, f.main, "rev-parse", f.tip+"^{tree}")
}

func (f *migratedFx) wantReaped() {
	f.t.Helper()
	if rc := f.reap(f.main, Request{}); rc != 0 {
		f.t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (migrated to mini)")
	if filekind.IsDir(f.wt) {
		f.t.Fatal("worktree still there")
	}
	if exec.Command("git", "-C", f.main, "show-ref", "--verify", "--quiet", "refs/heads/"+rb).Run() == nil {
		f.t.Fatal("local branch kept")
	}
	if !originHas(f.bare) {
		f.t.Fatal("origin branch deleted")
	}
	wantJSON(f.t, f.rows("status")[0], fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"reaped","detail":"migrated to mini","source":"reap"}}`, nowUnix*1000))
	for _, c := range f.calls {
		if strings.HasPrefix(c, "gh ") {
			f.t.Fatalf("a migrated reap ran %q", c)
		}
	}
}

func TestMigratedReaped(t *testing.T) {
	f := newMigratedFx(t)
	f.moved(f.tip, f.tipTree(), remoteDispatch("mini"))
	f.wantReaped()
	wantJSON(t, f.rows("reap")[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","crew_id":"c1","branch":"feat/r","repo":%q,"pr":"","pr_state":"","worktree":%q,"migrated_to":"mini"}`, nowUnix*1000, f.paths.Common, f.wt))
}

// The remote worker pushed past the mark and a local fetch moved origin/<b>.
func TestMigratedReapedWhenOriginAhead(t *testing.T) {
	f := newMigratedFx(t)
	ahead := gitOut(t, f.main, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", f.tip+"^{tree}", "-p", f.tip, "-m", "remote work")
	runGit(t, f.main, "update-ref", "refs/remotes/origin/"+rb, ahead)
	f.moved(f.tip, f.tipTree(), remoteDispatch("mini"))
	f.wantReaped()
}

// The SessionEnd backstop and an orphaned stall-watch post after the mark;
// neither is the worker's word.
func TestMigratedReapedPastExitedAndWatchdogRows(t *testing.T) {
	for _, row := range []string{
		`{"ts":3500,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"exited","detail":"SessionEnd"}}`,
		`{"ts":3500,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"blocked","detail":"dead: pane gone","source":"watchdog"}}`,
	} {
		f := newMigratedFx(t)
		f.moved(f.tip, f.tipTree(), row, remoteDispatch("mini"))
		f.wantReaped()
	}
}

// The patch's content is still in the tree: its scratch tree, gitignored root
// scaffold included as crew migrate snapshot took it, equals the mark's tree.
func TestMigratedReapedWithTheSnapshotStillInTheTree(t *testing.T) {
	f := newMigratedFx(t)
	exclude, err := os.OpenFile(f.main+"/.git/info/exclude", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exclude.WriteString("SPEC.md\nWORKER_TASK.md\n"); err != nil {
		t.Fatal(err)
	}
	_ = exclude.Close()
	portFWrite(t, f.wt+"/SPEC.md", "spec\n")
	portFWrite(t, f.wt+"/WORKER_TASK.md", "task\n")
	portFWrite(t, f.wt+"/a.txt", "wip\n")
	portFWrite(t, f.wt+"/PLAN.md", "plan\n")
	c := f.candidate()
	snap, err := Snapshot(t.Context(), SnapshotRequest{
		Admin: c.admin, WtPath: c.wtpath, Branch: rb, Tmp: t.TempDir(),
		Root: f.paths.Dir + "/artifacts", Dir: rb + "/migrate-x", Name: "patch", IgnoredScaffold: true, Stderr: &f.stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.moved(snap.Tip, snap.Tree, remoteDispatch("mini"))
	f.wantReaped()
	if _, err := os.Stat(snap.Patch); err != nil {
		t.Fatalf("the mark's patch went: %v", err)
	}
}

// Each C7 gate that fails keeps the worktree, naming the gate.
func TestMigratedKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		why  string
		set  func(f *migratedFx)
	}{
		{"no remote row", "migrated to mini, but no remote dispatch row on mini yet", func(f *migratedFx) {
			f.moved(f.tip, f.tipTree())
		}},
		{"remote row on another host", "migrated to mini, but its newest dispatch row is on studio", func(f *migratedFx) {
			f.moved(f.tip, f.tipTree(), remoteDispatch("studio"))
		}},
		{"local tip moved", "migrated to mini, but its tip is not the mark's", func(f *migratedFx) {
			runGit(f.t, f.wt, "commit", "-q", "--allow-empty", "-m", "late")
			f.moved(f.tip, f.tipTree(), remoteDispatch("mini"))
		}},
		{"tip not on origin", "migrated to mini, but its tip is not on origin/feat/r", func(f *migratedFx) {
			runGit(f.t, f.wt, "commit", "-q", "--allow-empty", "-m", "unpushed")
			tip := gitOut(f.t, f.main, "rev-parse", "refs/heads/"+rb)
			f.moved(tip, gitOut(f.t, f.main, "rev-parse", tip+"^{tree}"), remoteDispatch("mini"))
		}},
		{"origin not fetched", "migrated to mini, but could not compare its tip with origin/feat/r (not fetched?)", func(f *migratedFx) {
			runGit(f.t, f.main, "update-ref", "-d", "refs/remotes/origin/"+rb)
			f.moved(f.tip, f.tipTree(), remoteDispatch("mini"))
		}},
		{"dirty beyond the mark's tree", "migrated to mini, but uncommitted changes beyond its migrate snapshot", func(f *migratedFx) {
			portFWrite(f.t, f.wt+"/late.txt", "x\n")
			f.moved(f.tip, f.tipTree(), remoteDispatch("mini"))
		}},
		{"live engine", "an engine is still running there (migrated session)", func(f *migratedFx) {
			f.tmux[panesKey] = "@1\t%1\tclaude\t" + f.wt
			f.moved(f.tip, f.tipTree(), remoteDispatch("mini"))
		}},
		{"worker-posted migrated", "migrated, but no crew migrate mark vouches for it", func(f *migratedFx) {
			f.bus(localDispatch(), workingRow(), markRow(f.tip, f.tipTree(), ""), remoteDispatch("mini"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMigratedFx(t)
			tc.set(f)
			if rc := f.reap(f.main, Request{}); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			want := "crew reap: keeping feat/r — " + tc.why
			if !strings.Contains(f.stdout.String(), want) {
				t.Fatalf("stdout %q, want a line %q", f.stdout.String(), want)
			}
			if !filekind.IsDir(f.wt) || len(f.rows("status"))+len(f.rows("reap")) != 0 {
				t.Fatalf("reaped: appended %q", f.appended)
			}
			if !originHas(f.bare) {
				t.Fatal("origin branch deleted")
			}
		})
	}
}

// The removal tail re-checks a snapshot-holding tree against the mark's tree
// right before the removal.
func TestRemoveMigratedRechecksTheSnapshot(t *testing.T) {
	f := newMigratedFx(t)
	portFWrite(t, f.wt+"/a.txt", "wip\n")
	c := f.candidate()
	c.mode, c.pr, c.prState, c.label, c.markTree = "migrated", "", "", "migrated to mini", true
	c.rel = release.Release{ToHost: "mini", Tip: f.tip, Tree: f.tipTree()}
	if removed, code := f.run().remove(context.Background(), c); removed || code != 0 {
		t.Fatalf("removed=%v code=%d", removed, code)
	}
	f.wantOut("crew reap: keeping feat/r — changed since its migrate snapshot")
	if !filekind.IsDir(f.wt) || len(f.appended) != 0 {
		t.Fatalf("reaped: appended %q", f.appended)
	}
}
