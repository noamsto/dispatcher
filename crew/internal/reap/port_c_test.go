package reap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// portCReap is f.reap with the probes adjusted by tweak first.
func portCReap(f *fx, cwd string, req Request, tweak func(*Probes)) int {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	o := f.opts()
	if tweak != nil {
		tweak(&o.Probes)
	}
	return Run(context.Background(), req, f.paths, cwd, &f.stdout, &f.stderr, o)
}

// portCAnchor plants the 4-line record `dispatch` writes for wt.
func portCAnchor(f *fx, wt, branch string) {
	f.t.Helper()
	path, err := wtgit.AnchorPath(wt)
	if err != nil {
		f.t.Fatal(err)
	}
	admin, err := wtgit.AdminDir(f.paths.Common, wt)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	rec := wt + "\n" + f.paths.Dir + "\n" + branch + "\n" + admin + "\n"
	if err := os.WriteFile(path, []byte(rec), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// portCFake is a standalone clone of the main repo, the repo a worker's
// swapped `.git` would name. mutate configures the trap.
func portCFake(f *fx, name string, mutate func(fake string)) string {
	f.t.Helper()
	fake := filepath.Join(f.root, name)
	if out, err := exec.Command("git", "clone", "-q", f.main, fake).CombinedOutput(); err != nil {
		f.t.Fatalf("clone: %v\n%s", err, out)
	}
	mutate(fake)
	return fake
}

// portCHookFake traps any ref update in the fake: its reference-transaction
// hook touches sentinel.
func portCHookFake(f *fx, name, branch, sentinel string) string {
	return portCFake(f, name, func(fake string) {
		runGit(f.t, fake, "branch", branch, "origin/"+branch)
		hook := fake + "/.git/hooks/reference-transaction"
		if err := os.WriteFile(hook, []byte("#!/bin/sh\n: >"+sentinel+"\n"), 0o755); err != nil {
			f.t.Fatal(err)
		}
	})
}

// portCFilterFake traps any git that discovers the fake: its own attributes
// select a clean filter that touches sentinel.
func portCFilterFake(f *fx, name, sentinel string) string {
	return portCFake(f, name, func(fake string) {
		if err := os.WriteFile(fake+"/.git/info/attributes", []byte("* filter=x\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
		runGit(f.t, fake, "config", "filter.x.clean", "touch "+sentinel+"; cat")
	})
}

func portCSwapGit(t *testing.T, wt, fake string) {
	t.Helper()
	if err := os.Remove(wt + "/.git"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fake+"/.git", wt+"/.git"); err != nil {
		t.Fatal(err)
	}
}

func portCExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// portCMerged is a done worker on branch whose PR merged, tip at the PR head.
func portCMerged(f *fx, branch string) {
	f.t.Helper()
	f.bus(statusFor(branch, "s1", "done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	f.gh["pr view "+prURL+" --json headRefOid --jq .headRefOid"] = gitOut(f.t, f.main, "rev-parse", "refs/heads/"+branch)
}

// portCCandAndHere is the #633 layout: a candidate worktree with unique work
// and the caller's own worktree, both linked to main.
func portCCandAndHere(f *fx) (cand, here string) {
	f.t.Helper()
	cand = f.addWorktree("feat/633-cand")
	if err := os.WriteFile(cand+"/work.txt", []byte("unique"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, cand, "add", "work.txt")
	runGit(f.t, cand, "commit", "-q", "-m", "cand work")
	here = f.addWorktree("feat/633-here")
	return cand, here
}

// portCSwapOnTip swaps here's .git for fake on the first headRefOid lookup, as
// a worker could at any point during the sweep.
func portCSwapOnTip(f *fx, here, fake string) (swapped *bool, tweak func(*Probes)) {
	swapped = new(bool)
	return swapped, func(p *Probes) {
		p.Gh = &ghtest.Fake{Func: func(args []string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "headRefOid") && !*swapped {
				*swapped = true
				portCSwapGit(f.t, here, fake)
			}
			return f.ghCall(args)
		}}
	}
}

// #633: the branch delete lands in the real repo even when the caller's .git is
// swapped mid-sweep, recorded or not.
func TestPortCSwappedCallerGitBranchDelete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool
	}{
		{"recorded caller", true},
		{"unrecorded caller", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			cand, here := portCCandAndHere(f)
			if tc.recorded {
				portCAnchor(f, here, "feat/633-here")
			}
			sentinel := filepath.Join(f.root, "SENTINEL")
			fake := portCHookFake(f, "633-fake", "feat/633-cand", sentinel)
			f.gh["pr view "+prURL+" --json closingIssuesReferences --jq .closingIssuesReferences[].number"] = "99"
			portCMerged(f, "feat/633-cand")
			swapped, tweak := portCSwapOnTip(f, here, fake)

			rc := portCReap(f, here, Request{Quiet: true}, tweak)

			if rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			if !*swapped {
				t.Fatal("the swap never fired")
			}
			if portCExists(sentinel) {
				t.Fatal("the fake repo ran its hook")
			}
			if !strings.Contains(f.stdout.String(), "crew reap: reaped feat/633-cand (MERGED)") {
				t.Fatalf("stdout %q", f.stdout.String())
			}
			if portCExists(cand) {
				t.Fatal("candidate worktree still there")
			}
			if exec.Command("git", "-C", f.main, "show-ref", "--verify", "--quiet", "refs/heads/feat/633-cand").Run() == nil {
				t.Fatal("branch not deleted from the real repo")
			}
			// The fake keeps its own copy of the branch: the delete never reached it.
			gitOut(t, here, "--git-dir="+here+"/.git", "show-ref", "--verify", "refs/heads/feat/633-cand")
		})
	}
}

// #633: a recorded worktree the caller stands in is kept as the current worktree.
func TestPortCKeepsRecordedCurrentWorktree(t *testing.T) {
	f := newFx(t)
	here := f.addWorktree("feat/633-here")
	portCAnchor(f, here, "feat/633-here")
	f.bus(statusFor("feat/633-here", "s1", "done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"

	if rc := portCReap(f, here, Request{}, nil); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: keeping feat/633-here — it is the current worktree", "crew reap: nothing reclaimed")
	if !portCExists(here) {
		t.Fatal("worktree removed")
	}
}

// #633: from inside a worker worktree whose .git was swapped, reap refuses
// before running any git there.
func TestPortCRefusesFromSwappedWorkerWorktree(t *testing.T) {
	f := newFx(t)
	cand, here := portCCandAndHere(f)
	portCAnchor(f, here, "feat/633-here")
	sentinel := filepath.Join(f.root, "SENTINEL")
	portCSwapGit(t, here, portCHookFake(f, "633-fake", "feat/633-cand", sentinel))
	f.bus(statusFor("feat/633-cand", "s1", "done", prURL, 0))

	if rc := portCReap(f, here, Request{Quiet: true}, nil); rc != 1 {
		t.Fatalf("rc=%d, want 1; stderr=%q", rc, f.stderr.String())
	}
	want := "refusing git: " + f.paths.Common + " is not the git dir " + here + " resolves to (" + here +
		"/.git) — the common dir changed since it was read\n"
	f.wantErr(want)
	if portCExists(sentinel) || !portCExists(cand) || len(f.calls) != 0 {
		t.Fatalf("sentinel=%v cand=%v calls=%q", portCExists(sentinel), portCExists(cand), f.calls)
	}
}

// portCStatDirty is a worktree on a new branch holding a tracked, stat-dirty
// file f, so any git discovering a swapped-in repo would run its clean filter
// on it.
func portCStatDirty(f *fx, branch string) string {
	f.t.Helper()
	if err := os.WriteFile(f.main+"/f", []byte("a"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, f.main, "add", "f")
	runGit(f.t, f.main, "commit", "-q", "-m", "f")
	wt := f.addWorktree(branch)
	future := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(wt+"/f", future, future); err != nil {
		f.t.Fatal(err)
	}
	return wt
}

// #677: a .git swapped while the scaffold is trashed, after the gitlink check,
// is never discovered: the removal is kept.
func TestPortCSwapAfterGitlinkCheckNeverDiscovered(t *testing.T) {
	f := newFx(t)
	wt := portCStatDirty(f, rb+"677a")
	if err := os.WriteFile(wt+"/WORKER_TASK.md", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(f.root, "SENTINEL")
	fake := portCFilterFake(f, "677-fake", sentinel)
	f.bus(statusFor(rb+"677a", "s1", "done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	swapped := false

	rc := portCReap(f, f.main, Request{}, func(p *Probes) {
		inner := p.Gtrash
		p.Gtrash = func(ctx context.Context, path string) {
			if !swapped {
				swapped = true
				if err := os.Rename(wt+"/.git", wt+".gitfile"); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(fake+"/.git", wt+"/.git"); err != nil {
					t.Fatal(err)
				}
			}
			inner(ctx, path)
		}
	})

	if rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if !swapped {
		t.Fatal("the swap never fired")
	}
	if portCExists(sentinel) {
		t.Fatal("a git discovered the swapped repo and ran its filter")
	}
	if !portCExists(wt) || len(f.rows("reap")) != 0 {
		t.Fatalf("wt exists=%v rows=%q", portCExists(wt), f.appended)
	}
	f.wantOut("crew reap: keeping feat/r677a — its .git changed during the reap; possible tampering: tell the human",
		"crew reap: nothing reclaimed")
	// The trap is armed: a discovering git here does fire the filter.
	_ = exec.Command("git", "-C", wt, "status", "--porcelain").Run()
	if !portCExists(sentinel) {
		t.Fatal("the filter trap was never armed")
	}
}

// portCGitWrapper installs a git on PATH that swaps wt's .git for fake when it
// sees `worktree remove … wt`, and logs every later git that discovers its
// repo from inside wt.
func portCGitWrapper(t *testing.T, wt, fake, saved, marker, discovered string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
last=
for arg in "$@"; do last=$arg; done
if [ ! -e %[2]q ] && [ "$last" = %[1]q ]; then
  prev=
  for arg in "$@"; do
    if [ "$prev" = worktree ] && [ "$arg" = remove ]; then
      : >%[2]q
      mv %[1]q/.git %[3]q
      mv %[4]q/.git %[1]q/.git
      break
    fi
    prev=$arg
  done
fi
if [ -e %[2]q ] && [ -z "${GIT_DIR:-}" ]; then
  anchored= in_wt=
  case "$PWD/" in %[1]q/*) in_wt=1 ;; esac
  prev=
  for arg in "$@"; do
    case "$arg" in --git-dir=*) anchored=1 ;; esac
    if [ "$prev" = -C ]; then
      case "$arg/" in %[1]q/*) in_wt=1 ;; esac
    fi
    prev=$arg
  done
  if [ -z "$anchored" ] && [ -n "$in_wt" ]; then
    printf '%%s\n' "$*" >>%[6]q
  fi
fi
exec %[5]q "$@"
`, wt, marker, saved, fake, real, discovered)
	bin := t.TempDir()
	if err := os.WriteFile(bin+"/git", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// #677: a .git swapped inside the removal call itself, after every re-check, is
// refused by the anchored removal; no later git discovers the swapped tree.
func TestPortCSwapAfterLastRecheckRefusedByAnchoredRemoval(t *testing.T) {
	f := newFx(t)
	wt := portCStatDirty(f, rb+"677b")
	sentinel := filepath.Join(f.root, "SENTINEL")
	fake := portCFilterFake(f, "677b-fake", sentinel)
	anchor, err := wtgit.AnchorPath(wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte("record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker, discovered := filepath.Join(f.root, "swapped"), filepath.Join(f.root, "discovered")
	portCGitWrapper(t, wt, fake, filepath.Join(f.root, "real-gitfile"), marker, discovered)
	f.bus(statusFor(rb+"677b", "s1", "done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"

	if rc := portCReap(f, f.main, Request{}, nil); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if !portCExists(marker) {
		t.Fatal("the swap never fired")
	}
	if portCExists(sentinel) || portCExists(discovered) {
		t.Fatalf("a git discovered the swapped tree (sentinel=%v)", portCExists(sentinel))
	}
	if !portCExists(wt) || !portCExists(anchor) || len(f.rows("reap")) != 0 {
		t.Fatalf("wt=%v anchor=%v rows=%q", portCExists(wt), portCExists(anchor), f.appended)
	}
	f.wantOut("crew reap: keeping feat/r677b — its .git changed during the reap; possible tampering: tell the human",
		"crew reap: nothing reclaimed")
}

// #677: --force skips git's submodule refusal, so a gitlink staged after the
// first gate must be caught by the pre-removal re-check.
func TestPortCGitlinkStagedAfterFirstGateKeeps(t *testing.T) {
	f := newFx(t)
	wt := f.addWorktree(rb + "677c")
	if err := os.WriteFile(wt+"/WORKER_TASK.md", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	admin := gitOut(t, wt, "rev-parse", "--absolute-git-dir")
	head := gitOut(t, f.main, "rev-parse", "HEAD")
	f.bus(statusFor(rb+"677c", "s1", "done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	staged := false

	rc := portCReap(f, f.main, Request{}, func(p *Probes) {
		inner := p.Gtrash
		p.Gtrash = func(ctx context.Context, path string) {
			if !staged {
				staged = true
				runGit(t, wt, "init", "-q", wt+"/sub")
				out, err := exec.Command("git", "--git-dir="+admin, "--work-tree="+wt, "update-index", "--add",
					"--cacheinfo", "160000,"+head+",sub").CombinedOutput()
				if err != nil {
					t.Errorf("update-index: %v\n%s", err, out)
				}
			}
			inner(ctx, path)
		}
	})

	if rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if !staged {
		t.Fatal("the gitlink was never staged")
	}
	f.wantOut("crew reap: keeping feat/r677c — it has submodules", "crew reap: nothing reclaimed")
	if !portCExists(wt) || !portCExists(wt+"/sub/.git") || len(f.rows("reap")) != 0 {
		t.Fatalf("wt=%v rows=%q", portCExists(wt), f.appended)
	}
}

// #677: git deletes the admin dir even when part of the tree cannot go, so the
// resulting keep is reported as a partial removal, not as tampering.
func TestPortCPartialRemovalNotTampering(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFx(t)
	exclude, err := os.OpenFile(f.main+"/.git/info/exclude", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exclude.WriteString("ro/\n"); err != nil {
		t.Fatal(err)
	}
	_ = exclude.Close()
	ro := f.wt + "/ro"
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ro+"/f", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	anchor, err := wtgit.AnchorPath(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte("record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"

	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: keeping feat/r — removal failed partway; "+f.wt+" is no longer a worktree: tell the human (fix its permissions, then remove it)",
		"crew reap: nothing reclaimed")
	if portCExists(anchor) || len(f.rows("reap")) != 0 {
		t.Fatalf("anchor=%v rows=%q", portCExists(anchor), f.appended)
	}
}

// #557: reap runs once workers exist, so it must never record a baseline: with
// none, the worktree is kept and none appears.
func TestPortCNoBaselineKeepsAndRecordsNone(t *testing.T) {
	f := newFx(t)
	baseline := gitguard.BaselinePath(f.paths.Common)
	if err := os.Remove(baseline); err != nil {
		t.Fatal(err)
	}
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"

	rc := f.reap(f.main, Request{})

	if rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if !portCExists(f.wt) {
		t.Fatal("worktree removed")
	}
	if !slices.ContainsFunc(strings.Split(f.stdout.String(), "\n"), func(l string) bool {
		return strings.HasPrefix(l, "crew reap: keeping feat/r")
	}) {
		t.Fatalf("stdout %q stderr %q", f.stdout.String(), f.stderr.String())
	}
	if portCExists(baseline) {
		t.Fatal("reap recorded a baseline")
	}
}

// A terminal worker whose PR merged is not reclaimed while an engine pane still
// sits in its tree, exited or failed as much as done.
func TestPortCLiveEnginePaneKeepsTerminalMergedWorktree(t *testing.T) {
	f := newFx(t)
	f.bus(statusFor(rb, "s1", "exited", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	f.tmux[panesKey] = "@1\t%1\tclaude\t" + f.wt

	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if !strings.Contains(f.stdout.String(), "crew reap: keeping feat/r — an engine is still running there") {
		t.Fatalf("stdout %q", f.stdout.String())
	}
	if !portCExists(f.wt) {
		t.Fatal("worktree removed")
	}
	for _, c := range f.calls {
		if strings.Contains(c, "kill-window") || strings.HasPrefix(c, "gtrash") {
			t.Fatalf("calls %q", f.calls)
		}
	}
}

// An exited worker with no PR on the bus or GitHub is kept.
func TestPortCExitedWithoutPRKept(t *testing.T) {
	f := newFx(t)
	f.bus(`{"ts":1800000000000,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"exited"}}`)

	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: keeping feat/r — exited but no PR on the bus or GitHub; only done/failed reclaim on a closed issue",
		"crew reap: nothing reclaimed")
	if !portCExists(f.wt) {
		t.Fatal("worktree removed")
	}
}
