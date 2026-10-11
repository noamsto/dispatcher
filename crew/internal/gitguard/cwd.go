package gitguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// git-hooks.nix spells the shared core.hooksPath `.git/hooks` from the main
// checkout and `<common>/hooks` from a linked worktree. Only that exact
// relative string maps to `<R>/hooks` (Canon), R the realpath of the caller's
// anchored common dir: any other spelling may name a worker-editable dir. git
// resolves a relative value against a context the worker controls (a `.git`
// dir or symlink, core.bare, core.worktree), so CheckCwd — run only from a
// trusted cwd — asks git's own `--git-path hooks` whether the spellings
// resolve to one dir. Any core.worktree turns the equivalence off: work-tree
// commands resolve the value there, which rev-parse does not show.

// CheckCwd is Check for common and for the git dir cwd resolves to, plus the
// check that git run from cwd resolves a baselined relative hooks path to the
// baselined dir.
func CheckCwd(common, cwd string, stderr io.Writer) error {
	if err := pinMain(common, stderr); err != nil {
		return err
	}
	// A linked worktree's cwd adds its config.worktree to what plain git reads.
	// git's stderr waits until the common check passes, where bash printed it.
	var revParseErr bytes.Buffer
	own, ownErr := ownGitDir(cwd, &revParseErr)
	if ownErr != nil {
		if err := check(common, common, cwd, stderr); err != nil {
			return err
		}
		_, _ = stderr.Write(revParseErr.Bytes())
		say(stderr, "refusing git: cannot resolve the git dir of %s\n", cwd)
		return ErrRefused
	}
	if err := pinGitDir(common, own, cwd, stderr); err != nil {
		return err
	}
	if err := check(common, common, cwd, stderr); err != nil {
		return err
	}
	_, _ = stderr.Write(revParseErr.Bytes())
	if own != common {
		if err := check(common, own, cwd, stderr); err != nil {
			return err
		}
	}
	return checkHooks(common, own, cwd, stderr)
}

// exitCode is a command's exit status, or -1 when it could not run.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// checkHooks holds git from cwd to the baselined hooks dir: the checks above
// took a relative .git/hooks as <R>/hooks.
func checkHooks(common, own, cwd string, stderr io.Writer) error {
	out, err := git(cwd, stderr, "config", "--get", "core.hooksPath")
	switch exitCode(err) {
	case 0:
	case 1:
		return nil
	default:
		say(stderr, "refusing git: cannot read core.hooksPath in %s\n", Quote(cwd))
		return ErrRefused
	}
	if strings.TrimRight(string(out), "\n") != ".git/hooks" {
		return nil
	}
	recs, err := ReadBaseline(BaselinePath(common))
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec == "core.hookspath\n.git/hooks" {
			return nil
		}
	}
	r, err := realpath(cwd, common)
	if err != nil {
		r = ""
	}
	want := Quote(r + "/hooks")
	_, err = git(cwd, stderr, "config", "--get", "core.worktree")
	switch exitCode(err) {
	case 0:
		say(stderr, "refusing git: core.hooksPath .git/hooks in %s resolves under core.worktree, not against the baselined %s\n", Quote(cwd), want)
		say(stderr, "  set the absolute spelling: git config core.hooksPath %s\n", want)
		return ErrRefused
	case 1:
	default:
		say(stderr, "refusing git: cannot read core.worktree in %s\n", Quote(cwd))
		return ErrRefused
	}
	if out, err := git(cwd, io.Discard, "rev-parse", "--path-format=absolute", "--git-path", "hooks"); err == nil {
		h := strings.TrimRight(string(out), "\n")
		if m, err := realpathMissing(r + "/hooks"); err == nil && h == m {
			return nil
		}
		say(stderr, "refusing git: core.hooksPath .git/hooks in %s names %s, not the baselined %s\n", Quote(cwd), Quote(h), want)
	} else {
		say(stderr, "refusing git: core.hooksPath .git/hooks in %s names no hooks dir git can use, not the baselined %s\n", Quote(cwd), want)
	}
	if ro, err := realpath(cwd, own); r != "" && err == nil && ro == r {
		say(stderr, "  run from the main checkout, or set the absolute spelling: git config core.hooksPath %s\n", want)
	} else {
		say(stderr, "  set the absolute spelling: git config core.hooksPath %s\n", want)
	}
	return ErrRefused
}

// realpathMissing is `realpath -m`: no component need exist; symlinks among
// the existing ones are followed, `..` applies to the resolved path.
func realpathMissing(p string) (string, error) {
	// Not filepath.Abs: cleaning first would apply `..` before a symlink.
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + "/" + p
	}
	resolved := "/"
	rest := strings.Split(p, "/")
	for links := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, c)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if links++; links > 255 {
			return "", fmt.Errorf("%s: too many levels of symbolic links", p)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return resolved, nil
}
