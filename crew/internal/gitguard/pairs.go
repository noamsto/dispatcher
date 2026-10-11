package gitguard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// ErrRefused reports a refusal whose reason is already on stderr.
var ErrRefused = errors.New("gitguard: refused")

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// git runs git in dir ("" keeps this process's cwd) through PATH, returning
// its stdout; its stderr goes to stderr.
func git(dir string, stderr io.Writer, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = stderr
	return cmd.Output()
}

// splitNUL splits as bash's `mapfile -d ”`: a final terminator adds no empty
// record, an unterminated tail is still one.
func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	recs := strings.Split(string(b), "\x00")
	if recs[len(recs)-1] == "" {
		recs = recs[:len(recs)-1]
	}
	return recs
}

// keyValue splits a `key\nvalue` record; a valueless key (`[core] fsmonitor`)
// has no newline and an empty value.
func keyValue(rec string) (string, string) {
	k, v, _ := strings.Cut(rec, "\n")
	return k, v
}

func sortUnique(recs []string) []string {
	slices.Sort(recs)
	return slices.Compact(recs)
}

// realpath is `realpath -e`, a relative p taken from dir when dir is set.
func realpath(dir, p string) (string, error) {
	if dir != "" && !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Pairs lists the guarded local/worktree `key\nvalue` pairs of gitdir,
// byte-sorted and unique. Listing config never runs a configured program.
func Pairs(gitdir string, stderr io.Writer) ([]string, error) {
	return pairs(gitdir, "", stderr)
}

func pairs(gitdir, dir string, stderr io.Writer) ([]string, error) {
	out, err := git(dir, stderr, "--git-dir="+gitdir, "config", "--list", "--show-scope", "-z")
	if err != nil {
		return nil, err
	}
	fields := splitNUL(out)
	var recs []string
	for i := 0; i+1 < len(fields); i += 2 {
		if scope := fields[i]; scope != "local" && scope != "worktree" {
			continue
		}
		k, v := keyValue(fields[i+1])
		if Guarded(k, v) {
			recs = append(recs, k+"\n"+v)
		}
	}
	return sortUnique(recs), nil
}

// Union is Pairs over common and its linkedAdmins, each admin first passing
// PinAdmin.
func Union(common string, stderr io.Writer) ([]string, error) {
	dirs := append([]string{common}, linkedAdmins(common)...)
	var all []string
	for i, d := range dirs {
		if i > 0 {
			if err := PinAdmin(common, d, stderr); err != nil {
				return nil, err
			}
		}
		got, err := Pairs(d, stderr)
		if err != nil {
			return nil, err
		}
		all = append(all, got...)
	}
	return sortUnique(all), nil
}

// linkedAdmins lists common's linked admin dirs: the matches of the
// `common/worktrees/*/HEAD` glob that are regular files, so a dot name is
// skipped and an unreadable worktrees dir has none.
func linkedAdmins(common string) []string {
	var admins []string
	entries, _ := os.ReadDir(common + "/worktrees")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		admin := common + "/worktrees/" + e.Name()
		if fi, err := os.Stat(admin + "/HEAD"); err == nil && fi.Mode().IsRegular() {
			admins = append(admins, admin)
		}
	}
	return admins
}
