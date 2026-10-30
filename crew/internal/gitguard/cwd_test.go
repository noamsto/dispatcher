package gitguard

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hooksRepo baselines the absolute hooks spelling, then switches the config
// to git-hooks.nix's relative main-checkout spelling.
func hooksRepo(t *testing.T) repo {
	t.Helper()
	r := newRepo(t)
	r.config(t, "core.hooksPath", r.common+"/hooks")
	r.init(t)
	r.config(t, "core.hooksPath", ".git/hooks")
	return r
}

func TestCheckCwdHooksMainCheckoutAccepts(t *testing.T) {
	r := hooksRepo(t)
	wantPass(t, func(w io.Writer) error { return CheckCwd(r.common, r.main, w) })
}

func TestCheckCwdHooksLinkedWorktreeRefuses(t *testing.T) {
	r := hooksRepo(t)
	wt, _ := r.addWorktree(t, "wt")
	want := Quote(r.common + "/hooks")
	// From a linked worktree `.git` is a file: git has no hooks dir there.
	wantRefusal(t, "refusing git: core.hooksPath .git/hooks in "+Quote(wt)+" names no hooks dir git can use, not the baselined "+want+"\n"+
		"  set the absolute spelling: git config core.hooksPath "+want+"\n",
		func(w io.Writer) error { return CheckCwd(r.common, wt, w) })
}

func TestCheckCwdHooksCoreWorktreeRefuses(t *testing.T) {
	r := hooksRepo(t)
	r.config(t, "core.worktree", r.main)
	want := Quote(r.common + "/hooks")
	wantRefusal(t, "refusing git: core.hooksPath .git/hooks in "+Quote(r.main)+" resolves under core.worktree, not against the baselined "+want+"\n"+
		"  set the absolute spelling: git config core.hooksPath "+want+"\n",
		func(w io.Writer) error { return CheckCwd(r.common, r.main, w) })
}

func TestCheckCwdHooksRelativeBaselinePasses(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.hooksPath", ".git/hooks")
	r.init(t)
	wt, _ := r.addWorktree(t, "wt")
	wantPass(t, func(w io.Writer) error { return CheckCwd(r.common, wt, w) })
}

func TestCheckCwdHooksReadFailure(t *testing.T) {
	r := hooksRepo(t)
	shimGit(t, "core.hooksPath")
	wantRefusal(t, "refusing git: cannot read core.hooksPath in "+Quote(r.main)+"\n",
		func(w io.Writer) error { return CheckCwd(r.common, r.main, w) })
}

func TestCheckCwdOwnConfigWorktreeDrift(t *testing.T) {
	r := newRepo(t)
	r.config(t, "extensions.worktreeConfig", "true")
	wt, admin := r.addWorktree(t, "wt")
	r.init(t)
	run(t, wt, "config", "--worktree", "core.pager", "less")
	cfg := Quote(admin + "/config.worktree")
	wantRefusal(t, "refusing git: core.pager (from "+cfg+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  remove it: git config --file "+cfg+" --unset-all core.pager\n"+clearHint(),
		func(w io.Writer) error { return CheckCwd(r.common, wt, w) })
}

func TestCheckCwdUnresolvableOwn(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	outside := filepath.Join(r.root, "outside")
	mustDo(t, os.Mkdir(outside, 0o755))
	t.Setenv("GIT_CEILING_DIRECTORIES", r.root)
	got, err := capture(func(w io.Writer) error { return CheckCwd(r.common, outside, w) })
	if err == nil {
		t.Fatal("passed outside a repo")
	}
	if !strings.HasPrefix(got, "fatal: ") || !strings.HasSuffix(got, "\nrefusing git: cannot resolve the git dir of "+outside+"\n") {
		t.Fatalf("stderr %q", got)
	}
	// The common check runs first and, refusing, hides git's error as bash did.
	mustDo(t, os.Remove(r.baseline()))
	wantRefusal(t, "refusing git: no git-config baseline at "+r.baseline()+", but this repo had one — a dispatch never re-records it: run `crew git-baseline --accept` from your own terminal\n",
		func(w io.Writer) error { return CheckCwd(r.common, outside, w) })
}

func TestRealpathMissing(t *testing.T) {
	bin, err := exec.LookPath("realpath")
	if err != nil {
		t.Skip("no realpath")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, os.MkdirAll(root+"/a/b", 0o755))
	mustDo(t, os.Symlink(root+"/a/b", root+"/l"))
	mustDo(t, os.Symlink("../gone/x", root+"/a/dangling"))
	mustDo(t, os.Symlink("loop", root+"/loop"))
	writeFile(t, root+"/f", "")
	for _, p := range []string{
		root + "/a/b", root + "/l", root + "/l/..", root + "/l/../c", root + "/missing/../x",
		root + "/a/dangling", root + "/a/dangling/y", root + "/f/x", root + "//a/./b/",
	} {
		out, err := exec.Command(bin, "-m", "--", p).Output()
		if err != nil {
			t.Fatalf("realpath -m %s: %v", p, err)
		}
		if got, _ := realpathMissing(p); got != strings.TrimSuffix(string(out), "\n") {
			t.Errorf("realpathMissing(%q) = %q, want %q", p, got, out)
		}
	}
	if _, err := realpathMissing(root + "/loop"); err == nil {
		t.Error("a symlink loop resolved")
	}
}
