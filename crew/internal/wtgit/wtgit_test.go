package wtgit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/gitguard"
)

// repo is a symlink-resolved main checkout with one commit, a recorded
// git-config baseline and a linked worktree.
type repo struct {
	root, main, common, wt, admin string
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)
	var stderr bytes.Buffer
	cmd := exec.Command("git", full...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimRight(string(out), "\n")
}

func newRepo(t *testing.T) repo {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	r := repo{root: root, main: filepath.Join(root, "main"), common: filepath.Join(root, "main", ".git")}
	run(t, root, "init", "-q", r.main)
	run(t, r.main, "commit", "-q", "--allow-empty", "-m", "init")
	r.wt = filepath.Join(root, "wt")
	run(t, r.main, "worktree", "add", "-q", "-b", "wt", r.wt)
	r.admin = filepath.Join(r.common, "worktrees", "wt")
	var stderr bytes.Buffer
	if err := gitguard.Init(r.common, &stderr); err != nil {
		t.Fatalf("Init: %v\n%s", err, stderr.String())
	}
	return r
}

// sha1Empty is the empty tree git computes for a sha1 repository.
const sha1Empty = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func TestEmptyTree(t *testing.T) {
	r := newRepo(t)
	got, err := EmptyTree(r.admin)
	if err != nil || got != sha1Empty {
		t.Fatalf("EmptyTree = %q, %v; want %s", got, err, sha1Empty)
	}
}

func TestCommandEmptyTreeFailure(t *testing.T) {
	r := newRepo(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in *\" hash-object \"*) exit 3 ;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+":"+os.Getenv("PATH"))
	var stderr bytes.Buffer
	if _, err := Command(context.Background(), r.admin, r.wt, Opts{Stderr: &stderr}, "status"); !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("err = %v; want a refusal", err)
	}
	if want := "refusing git: cannot compute the empty tree of " + r.admin + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q; want %q", stderr.String(), want)
	}
}

func neutralArgs() []string {
	var a []string
	for _, kv := range NeutralCfg {
		a = append(a, "-c", kv)
	}
	return a
}

func TestCommandArgv(t *testing.T) {
	r := newRepo(t)
	var stderr bytes.Buffer
	cmd, err := Command(context.Background(), r.admin, r.wt, Opts{Stderr: &stderr}, "ls-files", "-z")
	if err != nil {
		t.Fatalf("Command: %v\n%s", err, stderr.String())
	}
	want := append([]string{"git", "-C", r.wt, "--no-optional-locks"}, neutralArgs()...)
	want = append(want, "--attr-source="+sha1Empty, "--git-dir="+r.admin, "--work-tree="+r.wt, "ls-files", "-z")
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("argv:\n%q\nwant:\n%q", cmd.Args, want)
	}
	if cmd.Dir != "" {
		t.Errorf("Dir = %q; -C carries the worktree, not Dir", cmd.Dir)
	}
	if cmd.Stderr != &stderr {
		t.Error("Stderr not the Opts writer")
	}
	if stderr.Len() != 0 {
		t.Errorf("a passing Command wrote %q", stderr.String())
	}
}

func TestCommandIndexFile(t *testing.T) {
	r := newRepo(t)
	t.Setenv("GIT_INDEX_FILE", "/elsewhere/index")
	cmd, err := Command(context.Background(), r.admin, r.wt, Opts{Stderr: &bytes.Buffer{}, IndexFile: "/tmp/ix"}, "status")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			got = append(got, kv)
		}
	}
	if !slices.Equal(got, []string{"GIT_INDEX_FILE=/tmp/ix"}) {
		t.Errorf("GIT_INDEX_FILE entries = %q", got)
	}
}

func TestCommonCommandArgv(t *testing.T) {
	r := newRepo(t)
	var stderr bytes.Buffer
	cmd, err := CommonCommand(context.Background(), r.common, Opts{Stderr: &stderr}, "fetch", "origin")
	if err != nil {
		t.Fatalf("CommonCommand: %v\n%s", err, stderr.String())
	}
	want := append([]string{"git", "--git-dir=" + r.common}, neutralArgs()...)
	want = append(want, "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "origin")
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("argv:\n%q\nwant:\n%q", cmd.Args, want)
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("GIT_DIR", "/x")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.pager=evil'")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.pager")
	t.Setenv("GIT_CONFIG_VALUE_0", "evil")
	t.Setenv("GIT_INDEX_FILE", "/x/index")
	t.Setenv("GIT_ATTR_SOURCE", "HEAD")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("HOME", "/home/x")
	for _, index := range []string{"", "/tmp/ix"} {
		env := Env(os.Environ(), index)
		has := func(name string) (string, bool) {
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, name+"="); ok {
					return v, true
				}
			}
			return "", false
		}
		for _, name := range []string{"GIT_DIR", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_ATTR_SOURCE"} {
			if v, ok := has(name); ok {
				t.Errorf("index %q: %s=%s survived", index, name, v)
			}
		}
		if v, ok := has("GIT_CONFIG_GLOBAL"); !ok || v != "/dev/null" {
			t.Errorf("index %q: GIT_CONFIG_GLOBAL = %q, %v", index, v, ok)
		}
		if v, ok := has("HOME"); !ok || v != "/home/x" {
			t.Errorf("index %q: HOME = %q, %v", index, v, ok)
		}
		v, ok := has("GIT_INDEX_FILE")
		if (index == "") == ok || ok && v != index {
			t.Errorf("index %q: GIT_INDEX_FILE = %q, %v", index, v, ok)
		}
	}
}

func TestCommandRefusals(t *testing.T) {
	r := newRepo(t)
	nonLinked := "refusing git in " + r.wt + ": " + r.common + " is not a linked-worktree admin dir\n"
	cases := []struct {
		name, admin, want string
		setup             func()
	}{
		{"config.worktree", r.admin, "refusing git in " + r.wt + ": " + r.admin + "/config.worktree exists — per-worktree config is worker-writable\n",
			func() { _ = os.WriteFile(r.admin+"/config.worktree", nil, 0o644) }},
		{"main git dir", r.common, nonLinked, func() {}},
		{"trailing worktrees", r.common + "/worktrees/", "refusing git in " + r.wt + ": " + r.common + "/worktrees/ is not a linked-worktree admin dir\n", func() {}},
		{"nested", r.admin + "/x", "refusing git in " + r.wt + ": " + r.admin + "/x is not a linked-worktree admin dir\n", func() {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.setup()
			var stderr bytes.Buffer
			cmd, err := Command(context.Background(), c.admin, r.wt, Opts{Stderr: &stderr}, "status")
			if cmd != nil || !errors.Is(err, gitguard.ErrRefused) {
				t.Fatalf("got %v, %v; want a refusal", cmd, err)
			}
			if stderr.String() != c.want {
				t.Errorf("stderr:\n%q\nwant:\n%q", stderr.String(), c.want)
			}
		})
	}
}

func TestCommandGuardRefusal(t *testing.T) {
	r := newRepo(t)
	run(t, r.main, "config", "core.fsmonitor", "/bin/evil")
	var stderr bytes.Buffer
	if _, err := Command(context.Background(), r.admin, r.wt, Opts{Stderr: &stderr}, "status"); !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("err = %v; want a refusal", err)
	}
	if !strings.Contains(stderr.String(), "refusing git: core.fsmonitor") {
		t.Errorf("stderr lacks the guard's reason:\n%s", stderr.String())
	}
	stderr.Reset()
	if _, err := CommonCommand(context.Background(), r.common, Opts{Stderr: &stderr}, "status"); !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("CommonCommand err = %v; want a refusal", err)
	}
}

func TestCommandUnreadableBaselinePrinted(t *testing.T) {
	r := newRepo(t)
	if err := os.Remove(gitguard.BaselinePath(r.common)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(gitguard.BaselinePath(r.common), 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	_, err := CommonCommand(context.Background(), r.common, Opts{Stderr: &stderr}, "status")
	if !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("err = %v; want a refusal", err)
	}
	if !strings.Contains(stderr.String(), "refusing git: no git-config baseline") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestStatus(t *testing.T) {
	r := newRepo(t)
	if err := os.WriteFile(filepath.Join(r.wt, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	got, err := Status(context.Background(), r.admin, r.wt, Opts{Stderr: &stderr})
	if err != nil || got != "?? new.txt" {
		t.Fatalf("Status = %q, %v\n%s", got, err, stderr.String())
	}
	got, err = Status(context.Background(), r.admin, r.wt, Opts{Stderr: &stderr}, "--", "other")
	if err != nil || got != "" {
		t.Fatalf("Status with a pathspec = %q, %v", got, err)
	}
}

func TestGitlinkOK(t *testing.T) {
	r := newRepo(t)
	if !GitlinkOK(r.admin, r.wt) {
		t.Fatal("a genuine linked worktree failed GitlinkOK")
	}
	if GitlinkOK(r.common, r.wt) {
		t.Error("gitlink matched an admin dir it does not name")
	}
	link := filepath.Join(r.root, "link")
	if err := os.Symlink(r.admin, link); err != nil {
		t.Fatal(err)
	}
	gitlink := filepath.Join(r.wt, ".git")
	rewrite := func(content string) {
		t.Helper()
		if err := os.Remove(gitlink); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gitlink, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("gitdir: " + link + "\n")
	if !GitlinkOK(r.admin, r.wt) {
		t.Error("a symlinked spelling of the admin dir should resolve to it")
	}
	rewrite("gitdir: ../main/.git/worktrees/wt\n")
	if !GitlinkOK(r.admin, r.wt) {
		t.Error("a relative gitdir resolves against the worktree")
	}
	rewrite("gitdir:" + r.admin + "\n")
	if GitlinkOK(r.admin, r.wt) {
		t.Error("a malformed gitdir line passed")
	}
	rewrite("")
	if GitlinkOK(r.admin, r.wt) {
		t.Error("an empty .git passed")
	}
	if err := os.Remove(gitlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.root, "elsewhere"), gitlink); err != nil {
		t.Fatal(err)
	}
	if GitlinkOK(r.admin, r.wt) {
		t.Error("a symlinked .git passed")
	}
}

func TestAdminDir(t *testing.T) {
	r := newRepo(t)
	got, err := AdminDir(r.common, r.wt)
	if err != nil || got != r.admin {
		t.Fatalf("AdminDir = %q, %v; want %q", got, err, r.admin)
	}
	if _, err := AdminDir(r.common, filepath.Join(r.root, "stranger")); err == nil {
		t.Error("AdminDir named an admin dir for a path no gitdir file points at")
	}
	if _, err := AdminDir(filepath.Join(r.root, "none"), r.wt); err == nil {
		t.Error("AdminDir succeeded with no worktrees dir")
	}
}

func TestAdminDirRefusesSymlinkedAdmin(t *testing.T) {
	r := newRepo(t)
	moved := filepath.Join(r.root, "moved-admin")
	if err := os.Rename(r.admin, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, r.admin); err != nil {
		t.Fatal(err)
	}
	if got, err := AdminDir(r.common, r.wt); err == nil {
		t.Fatalf("AdminDir followed a symlinked admin dir to %q", got)
	}
}

func TestAnchorPath(t *testing.T) {
	r := newRepo(t)
	out, err := exec.Command("sh", "-c", `printf %s "$1" | sha256sum | cut -c1-64`, "sh", r.wt).Output()
	if err != nil {
		t.Skipf("sha256sum: %v", err)
	}
	want := filepath.Join(r.root, "data", "crew", "worktrees", strings.TrimSpace(string(out)))
	if got, err := AnchorPath(r.wt); err != nil || got != want {
		t.Fatalf("AnchorPath = %q, %v; want %q", got, err, want)
	}
	link := filepath.Join(r.root, "wtlink")
	if err := os.Symlink(r.wt, link); err != nil {
		t.Fatal(err)
	}
	if got, err := AnchorPath(link); err != nil || got != want {
		t.Errorf("AnchorPath via a symlink = %q, %v; want the target's key", got, err)
	}
	t.Setenv("XDG_DATA_HOME", "")
	if got, err := AnchorPath(r.wt); err != nil || !strings.HasPrefix(got, r.root+"/.local/share/crew/worktrees/") {
		t.Errorf("AnchorPath without XDG_DATA_HOME = %q, %v", got, err)
	}
	if _, err := AnchorPath(filepath.Join(r.root, "missing")); err == nil {
		t.Error("AnchorPath of a missing path succeeded")
	}
}

func writeRecord(t *testing.T, wt string, lines ...string) {
	t.Helper()
	rec, err := AnchorPath(wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedCwdNoRecord(t *testing.T) {
	r := newRepo(t)
	var stderr bytes.Buffer
	got, err := TrustedCwd(r.common, r.main, &stderr)
	if err != nil || got != r.main || stderr.Len() != 0 {
		t.Fatalf("TrustedCwd = %q, %v, %q; want the cwd back", got, err, stderr.String())
	}
}

func TestTrustedCwdRecordOK(t *testing.T) {
	r := newRepo(t)
	writeRecord(t, r.wt, "branch", r.common+"/crew", r.admin)
	sub := filepath.Join(r.wt, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{r.wt, sub} {
		var stderr bytes.Buffer
		got, err := TrustedCwd(r.common, cwd, &stderr)
		if err != nil || got != r.main || stderr.Len() != 0 {
			t.Errorf("TrustedCwd(%s) = %q, %v, %q; want %q", cwd, got, err, stderr.String(), r.main)
		}
	}
}

func TestTrustedCwdFollowsPhysicalCwd(t *testing.T) {
	r := newRepo(t)
	writeRecord(t, r.wt, "branch", r.common+"/crew", r.admin)
	link := filepath.Join(r.root, "wtlink")
	if err := os.Symlink(r.wt, link); err != nil {
		t.Fatal(err)
	}
	got, err := TrustedCwd(r.common, link, &bytes.Buffer{})
	if err != nil || got != r.main {
		t.Fatalf("TrustedCwd via a symlink = %q, %v; want %q", got, err, r.main)
	}
}

func TestTrustedCwdMismatchedRecord(t *testing.T) {
	r := newRepo(t)
	writeRecord(t, r.wt, "branch", "/other/main/.git/crew", r.admin)
	var stderr bytes.Buffer
	got, err := TrustedCwd(r.common, r.wt, &stderr)
	if got != "" || !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("TrustedCwd = %q, %v; want a refusal", got, err)
	}
	want := "refusing git: " + gitguard.Quote(r.wt) + " is inside the worker worktree " + gitguard.Quote(r.wt) +
		", whose git resolves to " + gitguard.Quote(r.common) + ", not the recorded /other/main/.git" +
		" — its .git no longer matches its dispatcher record (possible tampering): stop and tell the human\n"
	if stderr.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", stderr.String(), want)
	}
}

func TestTrustedCwdRecordNotRegular(t *testing.T) {
	r := newRepo(t)
	rec, err := AnchorPath(r.wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rec, 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if _, err := TrustedCwd(r.common, r.wt, &stderr); !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("err = %v; want a refusal", err)
	}
	want := "refusing git: the dispatcher record for " + gitguard.Quote(r.wt) + " is not a regular file: " + gitguard.Quote(rec) + "\n"
	if stderr.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", stderr.String(), want)
	}
}

func TestTrustedCwdNoMainCheckout(t *testing.T) {
	r := newRepo(t)
	// A bare common dir lists itself as the first worktree, flagged bare.
	run(t, r.main, "worktree", "remove", "--force", r.wt)
	bare := filepath.Join(r.root, "bare.git")
	run(t, r.root, "clone", "-q", "--bare", r.main, bare)
	if err := gitguard.Init(bare, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(r.root, "bwt")
	run(t, bare, "worktree", "add", "-q", "-b", "b", wt)
	writeRecord(t, wt, "branch", bare+"/crew", bare+"/worktrees/bwt")
	var stderr bytes.Buffer
	if _, err := TrustedCwd(bare, wt, &stderr); !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("err = %v; want a refusal\n%s", err, stderr.String())
	}
	want := "refusing git: " + gitguard.Quote(wt) + " is a worker worktree and " + gitguard.Quote(bare) +
		" has no main checkout to run from — run from a directory outside any worker worktree\n"
	if stderr.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", stderr.String(), want)
	}
}

func TestTrustedCwdPinsCommondir(t *testing.T) {
	r := newRepo(t)
	if err := os.WriteFile(r.common+"/commondir", []byte("../elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if _, err := TrustedCwd(r.common, r.main, &stderr); !errors.Is(err, gitguard.ErrRefused) {
		t.Fatalf("err = %v; want a refusal", err)
	}
	if !strings.Contains(stderr.String(), "redirects the git common dir") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
