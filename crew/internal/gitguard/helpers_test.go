package gitguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv isolates git from the host's config; the nix check sandbox has none.
func gitEnv(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return root
}

// run runs git in dir with a test identity and returns its trimmed stdout.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimRight(string(out), "\n")
}

type repo struct {
	root, main, common string
}

// newRepo is a symlink-resolved main checkout with one commit.
func newRepo(t *testing.T) repo {
	t.Helper()
	root := gitEnv(t)
	r := repo{root: root, main: filepath.Join(root, "main"), common: filepath.Join(root, "main", ".git")}
	run(t, root, "init", "-q", r.main)
	run(t, r.main, "commit", "-q", "--allow-empty", "-m", "init")
	return r
}

func (r repo) config(t *testing.T, args ...string) {
	t.Helper()
	run(t, r.main, append([]string{"config"}, args...)...)
}

// addWorktree adds a linked worktree; it returns its checkout and admin dir.
func (r repo) addWorktree(t *testing.T, name string) (string, string) {
	t.Helper()
	wt := filepath.Join(r.root, name)
	run(t, r.main, "worktree", "add", "-q", "-b", name, wt)
	return wt, filepath.Join(r.common, "worktrees", name)
}

func (r repo) baseline() string { return BaselinePath(r.common) }

func (r repo) init(t *testing.T) {
	t.Helper()
	if out, err := capture(func(w io.Writer) error { return Init(r.common, w) }); err != nil {
		t.Fatalf("Init: %v\n%s", err, out)
	}
}

func capture(fn func(io.Writer) error) (string, error) {
	var b bytes.Buffer
	err := fn(&b)
	return b.String(), err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wantRefusal asserts fn refused with exactly want on stderr.
func wantRefusal(t *testing.T, want string, fn func(io.Writer) error) {
	t.Helper()
	got, err := capture(fn)
	if err == nil {
		t.Fatalf("passed, want refusal\n%s", want)
	}
	if got != want {
		t.Fatalf("stderr:\n%q\nwant:\n%q", got, want)
	}
}

func wantPass(t *testing.T, fn func(io.Writer) error) {
	t.Helper()
	if got, err := capture(fn); err != nil || got != "" {
		t.Fatalf("err %v, stderr %q; want silent pass", err, got)
	}
}

// shimGit puts a `git` on PATH that fails when its args contain fail, else
// runs the real git.
func shimGit(t *testing.T, fail string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in *\" " + fail + " \"*) exit 3 ;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func clearHint(file string) string {
	return "  to clear it: list drift with `crew git-baseline`, remove any key you did not set (git config --unset-all), then accept the rest from your own terminal: `crew git-baseline --accept` (deleting " + Quote(file) + " instead re-records EVERYTHING present at the next dispatch)\n"
}

// sha8 is the 8-hex fingerprint the baseline note prints after a masked value.
func sha8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func noteHint(file string) string {
	return "  human: check each value above is yours — a planted one stays trusted: restore it in its config file, delete " + Quote(file) + ", then run `crew git-baseline --accept` in your own terminal before any dispatch (a dispatch first re-records EVERYTHING present)\n"
}
