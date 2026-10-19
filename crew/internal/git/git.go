// Package git is the one seam to the git binary, beside wtgit's anchored porcelain layer.
package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Client runs `git args…` in dir ("" keeps the cwd): stdout without trailing
// newlines, kept on failure as $(…) keeps it, and a *ExitError for a non-zero
// exit. A git that cannot start returns the exec error.
type Client interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// ExitError is a git that ran and exited non-zero.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d: %s", e.Code, e.Stderr) }
func (e *ExitError) ExitCode() int { return e.Code }

// Exec runs the git on PATH; WaitDelay bounds Wait if a stub's child holds stdout.
type Exec struct{}

func (Exec) Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	stdout := strings.TrimRight(string(out), "\n")
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return stdout, &ExitError{Code: ee.ExitCode(), Stderr: string(ee.Stderr)}
	}
	return stdout, err
}

// Output is c.Run with failure blanked, as `$(git … 2>/dev/null || true)`.
func Output(ctx context.Context, c Client, dir string, args ...string) string {
	out, err := c.Run(ctx, dir, args...)
	if err != nil {
		return ""
	}
	return out
}

// Worktrees is `git [--git-dir=common] worktree list --porcelain`; a git that
// never ran maps to the shell's 127, as the roster arm dies with git's status.
func Worktrees(ctx context.Context, c Client, dir, common string) (string, error) {
	args := []string{"worktree", "list", "--porcelain"}
	if common != "" {
		args = append([]string{"--git-dir=" + common}, args...)
	}
	out, err := c.Run(ctx, dir, args...)
	if ee := (*ExitError)(nil); err != nil && !errors.As(err, &ee) {
		err = &ExitError{Code: 127, Stderr: "git: command not found\n"}
	}
	return out, err
}

// Toplevel is the work tree root containing dir, "" outside one.
func Toplevel(ctx context.Context, c Client, dir string) string {
	return Output(ctx, c, dir, "rev-parse", "--show-toplevel")
}

// Origin is remote.origin.url of the repo at common; exit 1 (unset) is no error.
func Origin(ctx context.Context, c Client, common string) (string, error) {
	out, err := c.Run(ctx, "", "--git-dir="+common, "config", "--get", "remote.origin.url")
	if ee := (*ExitError)(nil); errors.As(err, &ee) && ee.Code == 1 {
		return "", nil
	}
	return out, err
}

var slugPrefix = regexp.MustCompile(`(git@|https://)[^/:]+[/:]`)

// RepoSlug is the arm's `_origin_repo` sed (`s#(git@|https://)([^/:]+)[/:]##;
// s#\.git$##`). sed works per line, so each line of url maps on its own and an
// `ssh://git@host/…` URL still loses its `git@host/` piece.
func RepoSlug(url string) string {
	lines := strings.Split(url, "\n")
	for i, line := range lines {
		if loc := slugPrefix.FindStringIndex(line); loc != nil {
			line = line[:loc[0]] + line[loc[1]:]
		}
		lines[i] = strings.TrimSuffix(line, ".git")
	}
	return strings.Join(lines, "\n")
}
