package wtgit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// fetchSpec is `_wt_fetch_origin_branch`'s refspec: an explicit src:dst, so an
// untrusted (gh- or stamp-derived) branch name can never parse as a refspec
// that force-updates another ref (`+refs/heads/x:refs/remotes/origin/main`) or
// as a fetch option (`--upload-pack=…`).
const fetchSpec = "+refs/heads/%s:refs/remotes/origin/%s"

// fetchArgs is the helper's argv: the fetch flags and the remote, before the refspec.
var fetchArgs = []string{"fetch", "--porcelain", "-v", "--no-recurse-submodules", "origin"}

// fetchFlags are the `git fetch --porcelain` flags whose line carries the ref's
// new value: ' ' unchanged, '+' forced, '*' new, '=' up to date, 't' tip update.
var fetchFlags = map[byte]bool{' ': true, '+': true, '*': true, '=': true, 't': true}

// ErrFetch is a fetch that did not produce a usable oid: the branch is not on
// origin, its porcelain line named no flag we read, or the oid is no commit.
// The caller prints its own refusal; bash had only the helper's exit status.
var ErrFetch = errors.New("could not fetch the branch")

// PlainBranchName is `_wt_plain_branch_name`: whether name is safe to put into
// a refspec and a directive. The two guards come before git because
// `git check-ref-format --branch` calls `+refs/heads/x` a valid branch name,
// and a colon is a refspec. `--branch` is git's own refname check, `@{-1}`
// included, so it runs wherever the caller runs (bash ran it by discovery).
func PlainBranchName(ctx context.Context, name string, o Opts) bool {
	if strings.Contains(name, ":") || strings.HasPrefix(name, "+") {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", name)
	cmd.Env = Env(os.Environ(), "")
	cmd.Stdout = io.Discard
	cmd.Stderr = o.stderr()
	return cmd.Run() == nil
}

// FetchOriginBranch is `_wt_fetch_origin_branch`: fetch an untrusted branch name
// into refs/remotes/origin/<name> only, anchored on the common dir so it runs
// the same from any cwd and no worker's `.git` is consulted (#539, #633).
//
// The oid is the fetch's own porcelain output, never a re-read: refs are shared
// with workers, which can rewrite refs/remotes/origin/<name> between the fetch
// and a later rev-parse (#695).
func FetchOriginBranch(ctx context.Context, common, name string, o Opts) (string, error) {
	if !PlainBranchName(ctx, name, o) {
		return "", fmt.Errorf("%w: %q is not a plain branch name", ErrFetch, name)
	}
	argv := append(slices.Clone(fetchArgs), fmt.Sprintf(fetchSpec, name, name))
	cmd, err := CommonCommand(ctx, common, o, argv...)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: git fetch origin %s: %w", ErrFetch, name, err)
	}
	ref := "refs/remotes/origin/" + name
	oid := ""
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		fields := strings.Fields(line[2:])
		if len(fields) < 3 || fields[2] != ref {
			continue
		}
		if fetchFlags[line[0]] {
			oid = fields[1]
		}
	}
	if oid == "" {
		return "", fmt.Errorf("%w: origin %s named no fetched oid", ErrFetch, name)
	}
	// The bash helper ran this with 2>/dev/null: the refusal is the caller's line.
	quiet := o
	quiet.Stderr = io.Discard
	check, err := CommonCommand(ctx, common, quiet, "cat-file", "-e", oid+"^{commit}")
	if err != nil {
		return "", err
	}
	if err := check.Run(); err != nil {
		return "", fmt.Errorf("%w: %s is not a commit: %w", ErrFetch, oid, err)
	}
	return oid, nil
}
