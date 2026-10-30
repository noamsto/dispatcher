package reap

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const portFIssueRow = `{"ts":1,"crew_id":"c1","kind":"claim-issue","branch":"feat/r","issue":5}`

func portFWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// portFDirty leaves a tracked edit and an untracked file in the worker tree.
func portFDirty(f *fx) {
	f.t.Helper()
	portFWrite(f.t, f.wt+"/a.txt", "dirty\n")
	portFWrite(f.t, f.wt+"/new.txt", "new\n")
}

func portFDiscardOK(f *fx) {
	f.t.Helper()
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 0 {
		f.t.Fatalf("rc=%d stdout=%q stderr=%q", rc, f.stdout.String(), f.stderr.String())
	}
}

func portFNoPatch(f *fx) {
	f.t.Helper()
	if m, _ := filepath.Glob(f.paths.Dir + "/artifacts/*/discarded-*"); len(m) != 0 {
		f.t.Fatalf("patch written: %q", m)
	}
}

func TestPortFDiscardPlantedCleanFilterNeverRuns(t *testing.T) {
	f := discardFx(t)
	script, marker := f.portBHit()
	f.portBTrackFiltered()
	runGit(t, f.main, "config", "filter.x.clean", script)
	portFWrite(t, f.wt+"/f", "x\n")
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d stdout=%q stderr=%q", rc, f.stdout.String(), f.stderr.String())
	}
	f.portBWantAbsent(marker)
	f.portBWantPresent(f.wt)
	if !strings.Contains(f.stderr.String(), "crew reap: refusing --discard feat/r") {
		t.Fatalf("stderr %q", f.stderr.String())
	}
}

func TestPortFDiscardBaselinedRelativeFilterRewrittenNeverRuns(t *testing.T) {
	f := discardFx(t)
	_, marker := f.portBHit()
	f.portBCommit("tools/conv.sh", "#!/bin/sh\ncat\n")
	if err := os.Chmod(f.wt+"/tools/conv.sh", 0o755); err != nil {
		t.Fatal(err)
	}
	f.portBCommit("f", "a")
	f.portBCommit(".gitattributes", "f filter=x\n")
	runGit(t, f.main, "config", "filter.x.clean", "tools/conv.sh")
	f.portBReseed()
	portFWrite(t, f.wt+"/tools/conv.sh", "#!/bin/sh\ntouch "+marker+"\ncat\n")
	runGit(t, f.wt, "commit", "-qam", "rewrite")
	if err := os.RemoveAll(marker); err != nil {
		t.Fatal(err)
	}
	portFWrite(t, f.wt+"/f", "b")
	portFWrite(t, f.wt+"/untracked.txt", "u\n")
	soon := time.Unix(nowUnix, 0).Add(5 * time.Second)
	if err := os.Chtimes(f.wt+"/f", soon, soon); err != nil {
		t.Fatal(err)
	}
	portFDiscardOK(f)
	if !strings.Contains(f.stdout.String(), "crew reap: saved uncommitted state to ") {
		t.Fatalf("stdout %q", f.stdout.String())
	}
	f.portBWantAbsent(marker)
	f.portBWantAbsent(f.wt)
}

func TestPortFDiscardRefusesSymlinkedArtifactsLeaf(t *testing.T) {
	f := discardFx(t)
	portFDirty(f)
	victim := filepath.Join(f.root, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.paths.Dir+"/artifacts/feat", 0o755); err != nil {
		t.Fatal(err)
	}
	leaf := f.paths.Dir + "/artifacts/" + rb
	if err := os.Symlink(victim, leaf); err != nil {
		t.Fatal(err)
	}
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d stdout=%q", rc, f.stdout.String())
	}
	f.wantErr("crew reap: refusing --discard feat/r — " + leaf + " is a symlink or not a directory\n")
	if m, _ := os.ReadDir(victim); len(m) != 0 {
		t.Fatalf("wrote through the symlink: %v", m)
	}
	f.portBWantPresent(f.wt)
	f.noScratch()
}

// The patch is staged in a private scratch dir and linked into place, so a
// symlink at any timestamp-derived temp name is never opened.
func TestPortFDiscardIgnoresSymlinkAtPredictableTempName(t *testing.T) {
	f := discardFx(t)
	portFDirty(f)
	victim := filepath.Join(f.root, "victim.txt")
	portFWrite(t, victim, "keep\n")
	art := f.paths.Dir + "/artifacts/" + rb
	if err := os.MkdirAll(art, 0o755); err != nil {
		t.Fatal(err)
	}
	for off := int64(-1); off <= 10; off++ {
		name := "discarded-" + time.Unix(nowUnix+off, 0).UTC().Format("20060102T150405Z") + ".patch.tmp"
		if err := os.Symlink(victim, filepath.Join(art, name)); err != nil {
			t.Fatal(err)
		}
	}
	portFDiscardOK(f)
	if got, _ := os.ReadFile(victim); string(got) != "keep\n" {
		t.Fatalf("victim = %q", got)
	}
	f.portBWantAbsent(f.wt)
}

func TestPortFDiscardPatchIgnoresOperatorDiffConfig(t *testing.T) {
	f := discardFx(t)
	portFDirty(f)
	gcfg := filepath.Join(f.root, "global.gitconfig")
	portFWrite(t, gcfg, "[diff]\n\tnoprefix = true\n\tmnemonicPrefix = true\n[color]\n\tdiff = always\n")
	t.Setenv("GIT_CONFIG_GLOBAL", gcfg)
	patch := f.patchPath()
	portFDiscardOK(f)
	fresh := filepath.Join(f.root, "fresh")
	runGit(t, f.main, "worktree", "add", "-q", fresh, rb)
	runGit(t, fresh, "apply", "--check", patch)
}

// A write into an already-dirty path between the save and the removal leaves
// the status text unchanged only if it is checked by content; the tree is kept.
func TestPortFDiscardKeepsTreeWrittenAfterSave(t *testing.T) {
	f := discardFx(t)
	portFDirty(f)
	late := false
	rc := portCReap(f, f.main, Request{Discard: rb}, func(p *Probes) {
		p.Tmux = &tmuxtest.Fake{Func: func(args []string) (string, error) {
			if args[0] == "list-windows" && !late {
				late = true
				portFWrite(f.t, f.wt+"/a.txt", "dirty\nlate\n")
			}
			return f.tmux[strings.Join(args, " ")], nil
		}}
	})
	if rc != 1 || !late {
		t.Fatalf("rc=%d late=%v stdout=%q stderr=%q", rc, late, f.stdout.String(), f.stderr.String())
	}
	if !strings.Contains(f.stdout.String(), "crew reap: keeping feat/r — changed while saving") {
		t.Fatalf("stdout %q", f.stdout.String())
	}
	f.portBWantPresent(f.wt)
	f.noScratch()
}

func portFIssueFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	f.portBCommit("work.txt", "x\n")
	f.bus(status("done", "", 0), portFIssueRow)
	f.gh["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] = "[]"
	f.gh["issue view 5 --json state --jq .state"] = "CLOSED"
	return f
}

func TestPortFDiscardCleanTreeClosedIssueKeepsBranch(t *testing.T) {
	f := portFIssueFx(t)
	portFDiscardOK(f)
	f.wantOut("crew reap: no uncommitted changes to save", "crew reap: reaped feat/r (issue #5 CLOSED, discarded)")
	f.portBWantAbsent(f.wt)
	if !f.portBBranchExists() {
		t.Fatal("branch deleted")
	}
	portFNoPatch(f)
	wantJSON(t, f.rows("reap")[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/r","pr":"","pr_state":"ISSUE_CLOSED","worktree":%q,"issues":["5"],"discarded":""}`, nowUnix*1000, f.wt))
}

func TestPortFClosedIssueNeedsCommitsOnARemoteRef(t *testing.T) {
	f := portFIssueFx(t)
	runGit(t, f.main, "update-ref", "refs/remotes/origin/main", "main")
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — done but no PR on the bus or GitHub; 1 commit(s) not on any remote and not patch-equivalent to origin/main", "crew reap: nothing reclaimed")
	f.portBWantPresent(f.wt)

	runGit(t, f.main, "update-ref", "refs/remotes/origin/"+rb, "refs/heads/"+rb)
	f.portBOK(Request{Quiet: true})
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)")
	f.portBWantAbsent(f.wt)
	if !f.portBBranchExists() {
		t.Fatal("branch deleted")
	}
}

func TestPortFEarlierSessionMergedPRDoesNotVouchForLaterCommits(t *testing.T) {
	f := newFx(t)
	f.portBCommit("later.txt", "x\n")
	f.bus(statusFor(rb, "s1-1", "done", prURL, 10), statusFor(rb, "s2-2", "failed", "", 0))
	f.portBMerged("", gitOut(t, f.main, "rev-parse", "refs/heads/"+rb+"~1"))
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — its tip has commits past "+prURL, "crew reap: nothing reclaimed")
	f.portBWantPresent(f.wt)
}

// Bus rows with an empty branch or empty state parse to their own fields and
// shift no other branch's columns.
func TestPortFEmptyBranchOrStateRowsShiftNothing(t *testing.T) {
	f := newFx(t)
	f.portBCommit("later.txt", "x\n")
	f.portBMerged("", gitOut(t, f.main, "rev-parse", "refs/heads/"+rb+"~1"))
	f.gh["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] = "[]"
	row := func(ts int64, from, state, pr string) string {
		return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":"dispatcher:c1","kind":"status","body":{"state":%q,"pr_url":%q}}`, ts, from, state, pr)
	}
	f.bus(
		statusFor(rb, "s1-1", "done", prURL, 10),
		statusFor(rb, "s2-2", "failed", "", 5),
		row(8999999999998, "worker:", "done", "https://github.com/o/r/pull/96"),
		row(8999999999999, "worker:#a", "done", "https://github.com/o/r/pull/97"),
		row(9000000000000, "worker:#b", "done", "https://github.com/o/r/pull/99"),
		row(9000000000001, "worker:feat/other#s1-1", "", "https://github.com/o/r/pull/98"),
	)
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — its tip has commits past "+prURL, "crew reap: nothing reclaimed")
	f.portBWantPresent(f.wt)

	f.bus(statusFor(rb, "s3-3", "", "https://github.com/o/r/pull/97", 0))
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantErr("crew reap: refusing --discard feat/r — latest status is none\n")
	f.portBWantPresent(f.wt)
}

func TestPortFIdleReleaseIgnoresEmptyBranchRow(t *testing.T) {
	f := newFx(t)
	f.bus(`{"ts":1000,"crew_id":"c1","from":"worker:#` + rb + `","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`)
	f.stubOccupied("fish")
	f.wantOccupied()
	if rc := f.reap(f.main, Request{Idle: "0", Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if slices.Contains(f.calls, "tmux kill-window -t @23") || len(f.rows("release")) != 0 {
		t.Fatalf("released: calls %q appended %q", f.calls, f.appended)
	}
	f.portBWantPresent(f.wt)
}

func TestPortFIdleReleaseKeepsStateWhenSessionEmpty(t *testing.T) {
	f := newFx(t)
	f.bus(`{"ts":1000,"crew_id":"c1","from":"worker:` + rb + `#","to":"dispatcher:c1","kind":"status","body":{"state":"exited"}}`)
	f.stubOccupied(".claude-wrapped")
	f.wantOccupied()
	if rc := f.reap(f.main, Request{Idle: "0"}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.portAWantOutLine("crew reap: keeping feat/r — exited but an engine is still running there")
	if slices.Contains(f.calls, "tmux kill-window -t @23") {
		t.Fatalf("killed: %q", f.calls)
	}
}

// reap knows nothing of stacking: a child is just another worktree.
func TestPortFStackedChildNeverTouched(t *testing.T) {
	setup := func(t *testing.T) (*fx, string, string) {
		f := newFx(t)
		child := f.addWorktree("feat/child")
		task := child + "/WORKER_TASK.md"
		portFWrite(t, task, "base: "+rb+"\n")
		f.bus(status("done", prURL, 0))
		return f, child, task
	}
	t.Run("open parent PR keeps both", func(t *testing.T) {
		f, child, _ := setup(t)
		f.portBGh("OPEN")
		f.portBOK(Request{})
		f.wantOut("crew reap: keeping feat/r — PR OPEN", "crew reap: nothing reclaimed")
		f.portBWantPresent(f.wt)
		f.portBWantPresent(child)
		f.portBWantNoCall("gtrash")
	})
	t.Run("merged parent is reaped alone", func(t *testing.T) {
		f, child, task := setup(t)
		f.portBCommit("work.txt", "unique\n")
		f.portBMerged("", f.portBTip())
		f.portBOK(Request{Quiet: true})
		f.wantOut("crew reap: reaped feat/r (MERGED)")
		f.portBWantAbsent(f.wt)
		if f.portBBranchExists() {
			t.Fatal("parent branch survived")
		}
		f.portBWantPresent(child)
		if !filekind.IsDir(child) {
			t.Fatal("child gone")
		}
		if got, _ := os.ReadFile(task); string(got) != "base: "+rb+"\n" {
			t.Fatalf("child task = %q", got)
		}
		runGit(t, f.main, "show-ref", "--verify", "--quiet", "refs/heads/feat/child")
		if len(f.rows("reap")) != 1 {
			t.Fatalf("appended %q", f.appended)
		}
	})
}

// A commondir in the main git dir redirects every git call, so the guard
// refuses before gh or any git config runs.
func TestPortFPlantedCommondirRefusesBeforeGh(t *testing.T) {
	f := newFx(t)
	marker := filepath.Join(f.root, "SENTINEL")
	hit := filepath.Join(f.root, "evil-hit.sh")
	portFWrite(t, hit, "#!/bin/sh\ntouch "+marker+"\ncat\n")
	if err := os.Chmod(hit, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(f.root, "evil-common")
	runGit(t, f.root, "init", "-q", "--bare", fake)
	runGit(t, f.root, "--git-dir", fake, "config", "core.fsmonitor", hit)
	if err := os.MkdirAll(fake+"/crew", 0o755); err != nil {
		t.Fatal(err)
	}
	portFWrite(t, fake+"/crew/git-config-baseline", "#covers\nredirect\x00#covers\nlegacy\x00core.fsmonitor\n"+hit+"\x00")
	planted := f.paths.Common + "/commondir"
	portFWrite(t, planted, fake+"\n")
	f.bus(status("done", prURL, 0))
	f.portBGh("MERGED")

	if rc := f.reap(f.main, Request{}); rc == 0 {
		t.Fatalf("rc=0 stdout=%q", f.stdout.String())
	}
	out := f.stdout.String() + f.stderr.String()
	for _, want := range []string{"refusing git: ", planted, "redirects the git common dir"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	f.portBWantPresent(f.wt)
	f.portBWantAbsent(marker)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "gh ") {
			t.Fatalf("gh called: %q", f.calls)
		}
	}
}
