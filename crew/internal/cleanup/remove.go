package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const (
	patchName  = "uncommitted.patch"
	bundleName = "commits.bundle"
	infoName   = "info.json"
)

// errDrift is reap's own refusal text for a git config that left its baseline.
var errDrift = errors.New("git config drift (run `crew git-baseline`)")

// archiveRoot is ${XDG_DATA_HOME:-$HOME/.local/share}/crew/cleanup-archive.
func (o Options) archiveRoot() string {
	base := o.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = o.Getenv("HOME") + "/.local/share"
	}
	return filepath.Join(base, "crew", "cleanup-archive")
}

// info is info.json: what a human needs to find the work again.
type info struct {
	Repo       string   `json:"repo"`
	Branch     string   `json:"branch"`
	Codename   string   `json:"codename,omitempty"`
	Worktree   string   `json:"worktree"`
	Tip        string   `json:"tip"`
	Reason     string   `json:"reason"`
	Issues     []Ref    `json:"issues,omitempty"`
	PR         *Ref     `json:"pr,omitempty"`
	Uncommit   bool     `json:"has_uncommitted"`
	Commits    bool     `json:"has_commits"`
	ArchivedAt string   `json:"archived_at"`
	Restore    []string `json:"restore"`
}

// remove archives the item's work, then removes its worktree and local branch
// the way reap's `remove:` line does, behind the same baseline and worktree-
// record checks. It returns the archive dir. Nothing is deleted unless the
// archive is complete.
func (o Options) remove(ctx context.Context, it *Item, stderr io.Writer) (string, error) {
	common, wt := it.paths.Common, it.Worktree
	if !filekind.IsDir(wt) {
		return "", fmt.Errorf("%s is already gone", wt)
	}
	if cwd, err := os.Getwd(); err == nil && strings.HasPrefix(cwd+"/", wt+"/") {
		return "", errors.New("it is the current worktree")
	}
	admin, err := wtgit.AdminDir(common, wt)
	if err != nil {
		return "", fmt.Errorf("no git admin dir for %s", wt)
	}
	if !wtgit.GitlinkOK(admin, wt) {
		return "", errors.New("its .git does not point at its git admin dir")
	}
	if err := checkBaseline(common, admin, it.cwd, stderr); err != nil {
		return "", err
	}

	ts := o.now().UTC().Format("20060102T150405Z")
	rel := filepath.Join(it.repoName(), strings.ReplaceAll(it.Branch, "/", "-")+"-"+ts)
	dir, err := o.archive(ctx, it, admin, rel, stderr)
	if err != nil {
		return "", fmt.Errorf("archive failed, nothing removed: %w", err)
	}

	anchor, _ := wtgit.AnchorPath(wt)
	// The checks run again right before the removal, as reap's do.
	if err := checkBaseline(common, admin, it.cwd, stderr); err != nil {
		return dir, err
	}
	if !wtgit.GitlinkOK(admin, wt) {
		return dir, errors.New("its .git changed during cleanup; possible tampering: tell the human")
	}
	cmd, err := wtgit.CommonCommand(ctx, common, wtgit.Opts{Stderr: io.Discard}, "worktree", "remove", "--force", wt)
	if err != nil {
		return dir, errDrift
	}
	_ = cmd.Run()
	if filekind.IsDir(wt) {
		return dir, errors.New("worktree removal failed")
	}
	if anchor != "" {
		_ = syscall.Unlink(anchor)
	}
	cmd, err = wtgit.CommonCommand(ctx, common, wtgit.Opts{Stderr: stderr}, "branch", "-D", "--", it.Branch)
	if err != nil {
		return dir, errDrift
	}
	if err := cmd.Run(); err != nil {
		return dir, fmt.Errorf("worktree removed, but branch delete failed: %w", err)
	}
	return dir, nil
}

// checkBaseline is reap's pair of git-config checks: the admin dir and the
// trusted cwd both still match the baseline.
func checkBaseline(common, admin, cwd string, stderr io.Writer) error {
	trusted, err := wtgit.TrustedCwd(common, cwd, stderr)
	if err != nil {
		return err
	}
	if gitguard.Check(common, admin, stderr) != nil || gitguard.CheckCwd(common, trusted, stderr) != nil {
		return errDrift
	}
	return nil
}

// archive writes the recovery bundle under archiveRoot()/rel.
func (o Options) archive(ctx context.Context, it *Item, admin, rel string, stderr io.Writer) (string, error) {
	tmp, err := os.MkdirTemp("", "crew-cleanup-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	root := o.archiveRoot()
	res, err := reap.Snapshot(ctx, reap.SnapshotRequest{
		Admin: admin, WtPath: it.Worktree, Branch: it.Branch, Tmp: tmp,
		Root: root, Dir: rel, Name: patchName, FreshDir: true, IgnoredScaffold: true, Stderr: stderr,
	})
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, rel)
	opts := wtgit.Opts{Stderr: stderr}
	revs := []string{"refs/heads/" + it.Branch, "--not", "--remotes"}
	cmd, err := wtgit.CommonCommand(ctx, it.paths.Common, opts, append([]string{"rev-list"}, revs...)...)
	if err != nil {
		return "", errDrift
	}
	unpushed, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("could not list its unpushed commits: %w", err)
	}
	hasCommits := len(strings.TrimSpace(string(unpushed))) > 0
	if hasCommits {
		cmd, err := wtgit.CommonCommand(ctx, it.paths.Common, opts, append([]string{"bundle", "create", filepath.Join(dir, bundleName)}, revs...)...)
		if err != nil {
			return "", errDrift
		}
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git bundle failed: %w", err)
		}
	}
	meta := info{
		Repo: it.Repo, Branch: it.Branch, Codename: it.Codename, Worktree: it.Worktree, Tip: res.Tip,
		Reason: it.Reason, Issues: it.Issues, PR: it.PR, Uncommit: res.Patch != "", Commits: hasCommits,
		ArchivedAt: o.now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if meta.Uncommit {
		meta.Restore = append(meta.Restore, "git apply "+filepath.Join(dir, patchName)+"  # on the branch tip")
	}
	if hasCommits {
		meta.Restore = append(meta.Restore, "git fetch "+filepath.Join(dir, bundleName)+" refs/heads/"+it.Branch+":refs/heads/"+it.Branch)
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, infoName), append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// patchText is the item's full uncommitted diff, saved the way the archive
// saves it and read back.
func (o Options) patchText(ctx context.Context, it *Item, tmp string, stderr io.Writer) (string, error) {
	admin, err := wtgit.AdminDir(it.paths.Common, it.Worktree)
	if err != nil {
		return "", fmt.Errorf("no git admin dir for %s", it.Worktree)
	}
	work := filepath.Join(tmp, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		return "", err
	}
	res, err := reap.Snapshot(ctx, reap.SnapshotRequest{
		Admin: admin, WtPath: it.Worktree, Branch: it.Branch, Tmp: work,
		Root: tmp, Dir: "out", Name: patchName, IgnoredScaffold: true, Stderr: stderr,
	})
	if err != nil || res.Patch == "" {
		return "", err
	}
	b, err := os.ReadFile(res.Patch)
	return string(b), err
}
