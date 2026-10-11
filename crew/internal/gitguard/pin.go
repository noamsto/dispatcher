package gitguard

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A `commondir` file in a git dir redirects git's common dir. Only a linked
// worktree's admin dir `<common>/worktrees/<id>` legitimately has one; planted in
// the main git dir it makes git read — and the guard check — a worker-chosen
// config and baseline. Each guard entry pins the common dir before reading
// either.

func refuseCommondir(stderr io.Writer, dir string) error {
	say(stderr, "refusing git: %s/commondir redirects the git common dir — a main repository's git dir never has one; inspect and remove it before re-running\n", Quote(dir))
	return ErrRefused
}

// hasCommondir reports whether dir has a commondir entry of any type; a
// failed lstat other than absence counts as one.
func hasCommondir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, "commondir"))
	return !errors.Is(err, fs.ErrNotExist)
}

// pinMain refuses a commondir in common itself.
func pinMain(common string, stderr io.Writer) error {
	if hasCommondir(common) {
		return refuseCommondir(stderr, common)
	}
	return nil
}

// Pin pins common for a non-cwd entry checking gitdir ("" = common).
func Pin(common, gitdir string, stderr io.Writer) error {
	if err := pinMain(common, stderr); err != nil {
		return err
	}
	if gitdir == "" {
		return nil
	}
	return PinAdmin(common, gitdir, stderr)
}

// PinCwd pins common for an entry whose common dir came from git discovery in
// cwd, where common may itself be the redirect target.
func PinCwd(common, cwd string, stderr io.Writer) error {
	if err := pinMain(common, stderr); err != nil {
		return err
	}
	own, err := ownGitDir(cwd, stderr)
	if err != nil {
		say(stderr, "refusing git: cannot resolve the git dir of %s\n", cwd)
		return ErrRefused
	}
	return pinGitDir(common, own, cwd, stderr)
}

// PinAdmin requires a gitdir with a commondir to be a linked-worktree admin
// dir of common that git resolves back to common.
func PinAdmin(common, gitdir string, stderr io.Writer) error {
	return pinGitDir(common, gitdir, "", stderr)
}

func ownGitDir(cwd string, stderr io.Writer) (string, error) {
	out, err := git(cwd, stderr, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// pinGitDir is PinAdmin for g. cwd is set only for the cwd entries, which
// also refuse a main checkout's git dir with a commondir (pinMainCheckout) and
// a commondir-less g that is not common.
func pinGitDir(common, g, cwd string, stderr io.Writer) error {
	realG, errG := realpath(cwd, g)
	realC, errC := realpath(cwd, common)
	same := errG == nil && errC == nil && realG == realC
	if !hasCommondir(g) {
		// The caller read common by discovery from cwd earlier; a commondir
		// removed since leaves common naming the old redirect target.
		if cwd == "" || same {
			return nil
		}
		say(stderr, "refusing git: %s is not the git dir %s resolves to (%s) — the common dir changed since it was read\n", Quote(common), Quote(cwd), Quote(g))
		return ErrRefused
	}
	if same {
		return nil
	}
	if cwd != "" {
		if err := pinMainCheckout(g, realG, errG, cwd, stderr); err != nil {
			return err
		}
	}
	if errG != nil || errC != nil || filepath.Join(realC, "worktrees", filepath.Base(realG)) != realG {
		say(stderr, "refusing git: %s has a commondir but is not a linked-worktree admin dir of %s\n", Quote(g), Quote(common))
		return ErrRefused
	}
	out, err := git(cwd, stderr, "--git-dir="+g, "rev-parse", "--path-format=absolute", "--git-common-dir")
	resolved := strings.TrimSuffix(string(out), "\n")
	if err == nil {
		if r, err := realpath(cwd, resolved); err == nil {
			if r == realC {
				return nil
			}
			resolved = r
		}
	}
	say(stderr, "refusing git: %s/commondir resolves to %s, not %s\n", Quote(g), Quote(resolved), Quote(common))
	return ErrRefused
}

// pinMainCheckout refuses when g is a main repository's git dir. Path shape
// cannot tell one from an admin dir (a main checkout named `worktrees` with
// commondir `../..` has the admin shape and resolves consistently), and git's
// own --show-toplevel and core.bare read the redirected config, so it asks the
// filesystem only: is the nearest `.git` at or above the physical cwd a real
// directory that is g? Anything it cannot inspect refuses.
func pinMainCheckout(g, realG string, errG error, cwd string, stderr io.Writer) error {
	if errG != nil {
		return refuseCommondir(stderr, g)
	}
	start, err := realpath("", cwd)
	if err != nil {
		return refuseCommondir(stderr, g)
	}
	for a := start; ; a = filepath.Dir(a) {
		dotgit := filepath.Join(a, ".git")
		fi, err := os.Lstat(dotgit)
		if err == nil {
			if !fi.IsDir() {
				return nil
			}
			if r, err := filepath.EvalSymlinks(dotgit); err != nil || r == realG {
				return refuseCommondir(stderr, g)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return refuseCommondir(stderr, g)
		}
		if a == "/" {
			return nil
		}
	}
}
