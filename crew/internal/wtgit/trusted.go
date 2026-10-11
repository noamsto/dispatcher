package wtgit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/gitguard"
)

// AnchorPath is `_worktree_anchor_path`: the dispatcher-owned record of wt's
// genuine crew dir, branch and gitdir, keyed by wt's own realpath. The key
// must match the bash helper's byte for byte: dispatch writes the records and
// dispatch-resume reads them.
func AnchorPath(wt string) (string, error) {
	real, err := filepath.EvalSymlinks(wt)
	if err != nil {
		return "", err
	}
	if real, err = filepath.Abs(real); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(real))
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = os.Getenv("HOME") + "/.local/share"
	}
	return data + "/crew/worktrees/" + hex.EncodeToString(sum[:]), nil
}

// TrustedCwd is `_wt_trusted_cwd`: the directory a dispatcher command whose cwd
// may sit inside a dispatched worker's worktree must run from. The caller
// changes into it; this function moves nothing. A cwd outside any worker
// worktree comes back unchanged.
//
// The lookup is path-keyed on the dispatcher record of cwd's own ancestor, so
// there is no check-then-use gap: swapping `.git` cannot hide the worktree
// (#633). Moving or renaming the worktree can — a documented residual.
func TrustedCwd(common, cwd string, stderr io.Writer) (string, error) {
	phys, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	d, rec := phys, ""
	for {
		if rec, err = AnchorPath(d); err != nil {
			return "", err
		}
		if _, err := os.Lstat(rec); err == nil {
			break
		}
		if d == "/" {
			return cwd, guarded(gitguard.PinCwd(common, cwd, stderr), stderr)
		}
		d = filepath.Dir(d)
	}
	if fi, err := os.Lstat(rec); err != nil || !fi.Mode().IsRegular() {
		return "", refuse(stderr, "refusing git: the dispatcher record for %s is not a regular file: %s\n", gitguard.Quote(d), gitguard.Quote(rec))
	}
	data, err := os.ReadFile(rec)
	if err != nil {
		return "", err
	}
	recorded := ""
	if lines := strings.Split(string(data), "\n"); len(lines) > 1 {
		recorded = lines[1]
	}
	if want, err := gitguard.RealpathMissing(common + "/crew"); err != nil || recorded != want {
		return "", refuse(stderr, "refusing git: %s is inside the worker worktree %s, whose git resolves to %s, not the recorded %s — its .git no longer matches its dispatcher record (possible tampering): stop and tell the human\n",
			gitguard.Quote(phys), gitguard.Quote(d), gitguard.Quote(common), gitguard.Quote(strings.TrimSuffix(recorded, "/crew")))
	}
	// The record checks read no git; the worktree list would follow a planted commondir (#993).
	if err := guarded(gitguard.PinCwd(common, cwd, stderr), stderr); err != nil {
		return "", err
	}
	primary := mainWorktree(common, cwd, stderr)
	if primary != "" {
		fi, statErr := os.Stat(primary)
		realPrimary, errP := gitguard.RealpathMissing(primary)
		realCommon, errC := gitguard.RealpathMissing(common)
		// A separate-git-dir layout reports the git dir itself as the main worktree.
		if statErr != nil || !fi.IsDir() || errP != nil || errC != nil || realPrimary == realCommon {
			primary = ""
		}
	}
	if primary == "" {
		return "", refuse(stderr, "refusing git: %s is a worker worktree and %s has no main checkout to run from — run from a directory outside any worker worktree\n", gitguard.Quote(d), gitguard.Quote(common))
	}
	// The primary checkout's own discovery is pinned too, not just the worker's.
	if err := guarded(gitguard.PinCwd(common, primary, stderr), stderr); err != nil {
		return "", err
	}
	return primary, nil
}

// mainWorktree is the first `git worktree list --porcelain` entry of common,
// "" for a bare repository or a listing that failed.
func mainWorktree(common, cwd string, stderr io.Writer) string {
	cmd := exec.Command("git", "--git-dir="+common, "worktree", "list", "--porcelain")
	cmd.Dir = cwd
	cmd.Env = Env(os.Environ(), "")
	cmd.Stderr = stderr
	out, _ := cmd.Output()
	primary := ""
	for _, line := range strings.Split(string(bytes.TrimRight(out, "\n")), "\n") {
		if line == "" {
			break
		}
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			primary = p
		} else if line == "bare" {
			return ""
		}
	}
	return primary
}
