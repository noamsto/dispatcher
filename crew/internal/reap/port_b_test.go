package reap

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const portBIssue = "gh issue edit 99 --remove-label dispatched"

func (f *fx) portBGh(state string) {
	f.gh["pr view "+prURL+" --json state --jq .state"] = state
}

func (f *fx) portBMerged(closing, head string) {
	f.portBGh("MERGED")
	f.gh["pr view "+prURL+" --json closingIssuesReferences --jq .closingIssuesReferences[].number"] = closing
	f.gh["pr view "+prURL+" --json headRefOid --jq .headRefOid"] = head
}

func (f *fx) portBTip() string { return gitOut(f.t, f.main, "rev-parse", "refs/heads/"+rb) }

// portBCommit commits a file in the worker's worktree.
func (f *fx) portBCommit(name, body string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.wt, name)), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.wt, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, f.wt, "add", name)
	runGit(f.t, f.wt, "commit", "-q", "-m", "work "+name)
}

func (f *fx) portBAnchor() string {
	f.t.Helper()
	a, err := wtgit.AnchorPath(f.wt)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func portBExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func (f *fx) portBBranchExists() bool {
	return exec.Command("git", "-C", f.main, "show-ref", "--verify", "--quiet", "refs/heads/"+rb).Run() == nil
}

// portBHit writes a program that touches a marker, returning both paths.
func (f *fx) portBHit() (script, marker string) {
	f.t.Helper()
	script, marker = filepath.Join(f.root, "hit.sh"), filepath.Join(f.root, "SENTINEL")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return script, marker
}

// portBReseed re-records the git-config baseline from the current config, the
// way a dispatch's entry guard does before any worker exists.
func (f *fx) portBReseed() {
	f.t.Helper()
	if err := os.Remove(gitguard.BaselinePath(f.paths.Common)); err != nil {
		f.t.Fatal(err)
	}
	armed, _ := gitguard.ArmedPath(f.paths.Common)
	if err := os.RemoveAll(armed); err != nil {
		f.t.Fatal(err)
	}
	if err := gitguard.Init(f.paths.Common, &f.stderr); err != nil {
		f.t.Fatalf("gitguard.Init: %v %s", err, f.stderr.String())
	}
	f.stderr.Reset()
}

func (f *fx) portBOK(req Request) {
	f.t.Helper()
	if rc := f.reap(f.main, req); rc != 0 {
		f.t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
}

func (f *fx) portBWantAbsent(path string) {
	f.t.Helper()
	if portBExists(path) {
		f.t.Fatalf("%s should be gone", path)
	}
}

func (f *fx) portBWantPresent(path string) {
	f.t.Helper()
	if !portBExists(path) {
		f.t.Fatalf("%s should still exist", path)
	}
}

func (f *fx) portBWantNoCall(sub string) {
	f.t.Helper()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			f.t.Fatalf("unexpected call %q in %q", sub, f.calls)
		}
	}
}

func (f *fx) portBWantCall(call string) {
	f.t.Helper()
	if !slices.Contains(f.calls, call) {
		f.t.Fatalf("calls %q missing %q", f.calls, call)
	}
}

func TestPortBAnchorPrunedOnMerge(t *testing.T) {
	f := newFx(t)
	f.portBCommit("work.txt", "unique")
	anchor := f.portBAnchor()
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte("record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBMerged("99", f.portBTip())
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{Quiet: true})
	f.wantOut("crew reap: reaped feat/r (MERGED)")
	f.portBWantAbsent(anchor)
	f.portBWantAbsent(f.wt)
}

func TestPortBKeptWorkerAnchorSurvives(t *testing.T) {
	f := newFx(t)
	f.portBCommit("work.txt", "unique")
	anchor := f.portBAnchor()
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte("record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBGh("OPEN")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — PR OPEN", "crew reap: nothing reclaimed")
	f.portBWantPresent(anchor)
	f.portBWantPresent(f.wt)
}

func TestPortBDryRunNamesAnchor(t *testing.T) {
	f := newFx(t)
	f.portBCommit("work.txt", "unique")
	anchor := f.portBAnchor()
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte("record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBMerged("99", f.portBTip())
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{Quiet: true, DryRun: true})
	f.wantOut("crew reap: would reap feat/r (MERGED) @ "+f.wt, "crew reap: would prune record "+anchor)
	f.portBWantPresent(anchor)
	f.portBWantPresent(f.wt)
	if len(f.appended) != 0 {
		t.Fatalf("dry run appended %q", f.appended)
	}
}

func TestPortBLeftoverAnchorNoWorktree(t *testing.T) {
	f := newFx(t)
	ghost := filepath.Join(f.root, "ghost-wt")
	if err := os.Mkdir(ghost, 0o755); err != nil {
		t.Fatal(err)
	}
	anchor, err := wtgit.AnchorPath(ghost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ghost); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte("record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bus(statusFor("feat/ghost", "s1", "done", prURL, 0))
	f.portBOK(Request{Quiet: true})
	f.wantOut()
	f.portBWantPresent(anchor)
	if len(f.rows("reap")) != 0 {
		t.Fatalf("invented a reclaim: %q", f.appended)
	}
}

func TestPortBUndeletableAnchorKeepsBookkeeping(t *testing.T) {
	f := newFx(t)
	f.portBCommit("work.txt", "unique")
	anchor := f.portBAnchor()
	if err := os.MkdirAll(anchor, 0o755); err != nil {
		t.Fatal(err)
	}
	f.portBMerged("99", f.portBTip())
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{Quiet: true})
	f.wantOut("crew reap: reaped feat/r (MERGED)")
	f.portBWantAbsent(f.wt)
	if f.portBBranchExists() {
		t.Fatal("merged branch was not deleted")
	}
	if len(f.rows("reap")) != 1 {
		t.Fatalf("appended %q", f.appended)
	}
	f.portBWantCall(portBIssue)
	if fi, err := os.Lstat(anchor); err != nil || !fi.IsDir() {
		t.Fatalf("anchor directory gone: %v", err)
	}
}

func TestPortBDivergedTipKept(t *testing.T) {
	f := newFx(t)
	f.portBCommit("work.txt", "unique")
	f.portBMerged("99", "0000000000000000000000000000000000000000")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{})
	f.wantOut(
		"crew reap: reaped feat/r (MERGED)",
		"crew reap: kept local branch feat/r — tip diverges from the merged PR head",
	)
	f.portBWantCall(portBIssue)
	f.portBWantAbsent(f.wt)
	if !f.portBBranchExists() {
		t.Fatal("diverged branch was deleted")
	}
}

func TestPortBFailedRemovalKept(t *testing.T) {
	f := newFx(t)
	f.portBMerged("99", "")
	f.bus(status("done", prURL, 0))
	runGit(t, f.main, "worktree", "lock", f.wt)
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — worktree removal failed", "crew reap: nothing reclaimed")
	f.portBWantNoCall("remove-label")
	f.portBWantPresent(f.wt)
	if !f.portBBranchExists() {
		t.Fatal("branch deleted after a failed removal")
	}
	if len(f.rows("reap")) != 0 {
		t.Fatalf("reap row written: %q", f.appended)
	}
}

func TestPortBReviewNotesScaffoldTrashed(t *testing.T) {
	f := newFx(t)
	names := []string{"WORKER_TASK.md", "REVIEW_NOTES.md", "PLAN_ROUND4.md"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(f.wt, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.portBMerged("", "")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{Quiet: true})
	f.wantOut("crew reap: reaped feat/r (MERGED)")
	for _, n := range names {
		f.portBWantCall("gtrash put " + filepath.Join(f.wt, n))
	}
	f.portBWantAbsent(f.wt)
}

func TestPortBFinishedGridWindowReleasedThenReclaimed(t *testing.T) {
	f := newFx(t)
	t.Setenv("CREW_RELEASE_GAP", "0")
	for _, n := range []string{"WORKER_TASK.md", "REVIEW_NOTES.md"} {
		if err := os.WriteFile(filepath.Join(f.wt, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const (
		winsKey  = "list-windows -a -F #{window_id}\t#{@crew_name}\t#{pane_current_path}"
		paneKey  = "list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}"
		panePath = panesKey
	)
	f.tmux[winsKey] = "@23\tsage\t" + f.wt
	f.tmux[paneKey] = "@23\t%33\tcodex\n@23\t%34\tcodex"
	f.tmux[panePath] = "@23\t%33\tcodex\t" + f.wt + "\n@23\t%34\tcodex\t" + f.wt
	f.tmux["capture-pane -p -t %33"] = "pi: finished"
	f.tmux["capture-pane -p -t %34"] = "pi: finished"
	f.portBMerged("", f.portBTip())
	f.bus(statusFor(rb, "s1-1", "done", prURL, 5))

	o := f.opts()
	o.Probes.Tmux = &tmuxtest.Fake{Func: func(args []string) (string, error) {
		key := strings.Join(args, " ")
		f.record("tmux " + key)
		if args[0] == "kill-window" {
			clear(f.tmux)
			return "", nil
		}
		return f.tmux[key], nil
	}}
	run := func() int {
		f.stdout.Reset()
		f.stderr.Reset()
		return Run(t.Context(), Request{Idle: "0", Quiet: true}, f.paths, f.main, &f.stdout, &f.stderr, o)
	}
	if rc := run(); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: released @23 at "+f.wt+" (feat/r done)", "crew reap: reaped feat/r (MERGED)")
	f.portBWantCall("tmux kill-window -t @23")
	f.portBWantCall("gtrash put " + filepath.Join(f.wt, "REVIEW_NOTES.md"))
	f.portBWantAbsent(f.wt)
	if f.portBBranchExists() {
		t.Fatal("merged branch was not deleted")
	}
	// A later pass is a clean no-op: the worktree is already gone.
	if rc := run(); rc != 0 {
		t.Fatalf("second pass rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut()
}

func TestPortBExitedWorkerMergedReclaimed(t *testing.T) {
	f := newFx(t)
	f.portBGh("MERGED")
	f.bus(status("exited", prURL, 0))
	f.portBOK(Request{DryRun: true})
	f.wantOut(
		"crew reap: would reap feat/r (MERGED) @ "+f.wt,
		"crew reap: would prune record "+f.portBAnchor(),
	)
}

func TestPortBFailedWorkerOpenPRKept(t *testing.T) {
	f := newFx(t)
	f.portBGh("OPEN")
	f.bus(status("failed", prURL, 0))
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — PR OPEN", "crew reap: nothing reclaimed")
	f.portBWantPresent(f.wt)
	f.portBWantNoCall("remove")
}

func TestPortBScaffoldOnlyDirtIsReclaimed(t *testing.T) {
	f := newFx(t)
	for _, n := range []string{"WORKER_TASK.md", "SPEC.md", "PLAN.md", "DECOMPOSITION.md", "docs/superpowers/plans/2026-08-29-scaffold.md"} {
		p := filepath.Join(f.wt, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.portBGh("MERGED")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{DryRun: true})
	f.wantOut(
		"crew reap: would reap feat/r (MERGED) @ "+f.wt,
		"crew reap: would prune record "+f.portBAnchor(),
	)
}

func TestPortBTrackedModificationKeeps(t *testing.T) {
	f := newFx(t)
	f.portBCommit("README.md", "one\n")
	if err := os.WriteFile(filepath.Join(f.wt, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBGh("MERGED")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — uncommitted changes", "crew reap: nothing reclaimed")
	f.portBWantPresent(f.wt)
	portFNoPatch(f)
}

func TestPortBUntrackedNonScaffoldKeeps(t *testing.T) {
	f := newFx(t)
	if err := os.WriteFile(filepath.Join(f.wt, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBGh("MERGED")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{})
	f.wantOut("crew reap: keeping feat/r — uncommitted changes", "crew reap: nothing reclaimed")
	f.portBWantPresent(f.wt)
}

// portBKept reaps a MERGED worker whose tree must be kept and checks the
// planted program never ran, the worktree survives and the keep line is shown.
func (f *fx) portBKept(marker string, want ...string) {
	f.t.Helper()
	f.portBGh("MERGED")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{})
	f.portBWantAbsent(marker)
	f.portBWantPresent(f.wt)
	all := f.stdout.String() + f.stderr.String()
	for _, w := range append([]string{"crew reap: keeping feat/r"}, want...) {
		if !strings.Contains(all, w) {
			f.t.Fatalf("output lacks %q:\nstdout:\n%s\nstderr:\n%s", w, f.stdout.String(), f.stderr.String())
		}
	}
	if len(f.rows("reap")) != 0 {
		f.t.Fatalf("reap row written: %q", f.appended)
	}
}

func TestPortBForgedGitlinkNeverRunsConfig(t *testing.T) {
	f := newFx(t)
	script, marker := f.portBHit()
	fake := filepath.Join(f.root, "fakegit")
	if out, err := exec.Command("cp", "-r", f.paths.Common, fake).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	if err := os.RemoveAll(filepath.Join(fake, "worktrees")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "core.fsmonitor", script},
		{"symbolic-ref", "HEAD", "refs/heads/" + rb},
	} {
		if out, err := exec.Command("git", append([]string{"--git-dir=" + fake}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(f.wt, ".git"), []byte("gitdir: "+fake+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBKept(marker, "does not point at its git admin dir")
}

func TestPortBSubmoduleNeverRecursedInto(t *testing.T) {
	f := newFx(t)
	script, marker := f.portBHit()
	sub := filepath.Join(f.wt, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, sub, "init", "-q")
	runGit(t, sub, "commit", "-q", "--allow-empty", "-m", "sub-init")
	runGit(t, sub, "config", "core.fsmonitor", script)
	oid := gitOut(t, sub, "rev-parse", "HEAD")
	runGit(t, f.wt, "update-index", "--add", "--cacheinfo", "160000,"+oid+",sub")
	f.portBKept(marker, "it has submodules")
}

func TestPortBConfigWorktreeRefused(t *testing.T) {
	f := newFx(t)
	script, marker := f.portBHit()
	runGit(t, f.main, "config", "extensions.worktreeConfig", "true")
	runGit(t, f.wt, "config", "--worktree", "core.fsmonitor", script)
	f.portBReseed()
	f.portBKept(marker, "config.worktree")
}

// portBTrackFiltered commits f under attribute filter x in the worker's tree.
func (f *fx) portBTrackFiltered() {
	f.portBCommit("f", "")
	f.portBCommit(".gitattributes", "f filter=x\n")
}

func TestPortBPlantedCleanFilterNeverRuns(t *testing.T) {
	f := newFx(t)
	script, marker := f.portBHit()
	f.portBTrackFiltered()
	runGit(t, f.main, "config", "filter.x.clean", script)
	if err := os.WriteFile(filepath.Join(f.wt, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBKept(marker, "filter.x.clean")
}

func TestPortBBaselinedRelativeFilterRewrittenNeverRuns(t *testing.T) {
	f := newFx(t)
	_, marker := f.portBHit()
	f.portBCommit("tools/conv.sh", "#!/bin/sh\ncat\n")
	if err := os.Chmod(filepath.Join(f.wt, "tools/conv.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.portBCommit("f", "a")
	f.portBCommit(".gitattributes", "f filter=x\n")
	runGit(t, f.main, "config", "filter.x.clean", "tools/conv.sh")
	f.portBReseed()
	if err := os.WriteFile(filepath.Join(f.wt, "tools/conv.sh"), []byte("#!/bin/sh\ntouch "+marker+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.wt, "commit", "-qam", "rewrite")
	if err := os.RemoveAll(marker); err != nil {
		t.Fatal(err)
	}
	soon := time.Unix(nowUnix, 0).Add(5 * time.Second)
	if err := os.Chtimes(filepath.Join(f.wt, "f"), soon, soon); err != nil {
		t.Fatal(err)
	}
	f.portBGh("MERGED")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{})
	f.portBWantAbsent(marker)
	f.portBWantAbsent(f.wt)
}

func TestPortBPlantedIncludeNeverRuns(t *testing.T) {
	f := newFx(t)
	script, marker := f.portBHit()
	f.portBTrackFiltered()
	inc := filepath.Join(f.root, "include.gitconfig")
	if err := os.WriteFile(inc, []byte("[filter \"x\"]\n\tclean = "+script+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.main, "config", "include.path", inc)
	if err := os.WriteFile(filepath.Join(f.wt, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.portBKept(marker, "include.path")
}

func TestPortBPlantedFsmonitorNeverRuns(t *testing.T) {
	f := newFx(t)
	script, marker := f.portBHit()
	runGit(t, f.main, "config", "core.fsmonitor", script)
	f.portBKept(marker, "core.fsmonitor")
}

func TestPortBBaselineHooksPathCredentialHelperDoNotRefuse(t *testing.T) {
	f := newFx(t)
	runGit(t, f.main, "config", "core.hooksPath", ".husky")
	runGit(t, f.main, "config", "credential.helper", "store")
	f.portBReseed()
	base, err := os.ReadFile(gitguard.BaselinePath(f.paths.Common))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(base), "core.hookspath") {
		t.Fatalf("baseline lacks core.hookspath: %q", base)
	}
	f.portBMerged("", "")
	f.bus(status("done", prURL, 0))
	f.portBOK(Request{Quiet: true})
	f.wantOut("crew reap: reaped feat/r (MERGED)")
	f.portBWantAbsent(f.wt)
}
