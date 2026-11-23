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
	if inside(it.callerCwd, wt) {
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

	before, err := o.treeState(ctx, admin, it, stderr)
	if err != nil {
		return "", err
	}
	ts := o.now().UTC().Format("20060102T150405Z")
	rel := filepath.Join(it.repoName(), strings.ReplaceAll(it.Branch, "/", "-")+"-"+ts)
	dir, err := o.archive(ctx, it, admin, rel, stderr)
	if err != nil {
		return "", fmt.Errorf("archive failed, nothing removed: %w", err)
	}

	if after, err := o.treeState(ctx, admin, it, stderr); err != nil || after != before {
		return dir, errors.New("changed while saving; archived, but nothing removed — review it again")
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
func (o Options) archive(ctx context.Context, it *Item, admin, rel string, stderr io.Writer) (dir string, err error) {
	root := o.archiveRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	// Scratch beside the destination: Snapshot hard-links its patch into place.
	tmp, err := os.MkdirTemp(root, ".tmp-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	res, err := reap.Snapshot(ctx, reap.SnapshotRequest{
		Admin: admin, WtPath: it.Worktree, Branch: it.Branch, Tmp: tmp,
		Root: root, Dir: rel, Name: patchName, FreshDir: true, IgnoredScaffold: true, Stderr: stderr,
	})
	if err != nil {
		// Snapshot may have made the dir before failing; rmdir only takes it
		// while empty, and never a dir that already existed with content.
		_ = os.Remove(filepath.Join(root, rel))
		return "", err
	}
	// A named result is zeroed by `return "", err`, so the cleanup keeps its own path.
	out := filepath.Join(root, rel)
	dir = out
	// An archive that did not finish is no archive: never leave one that looks real.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(out)
		}
	}()
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
	// The branch is deleted, so bring its tip back before applying the patch.
	if hasCommits {
		meta.Restore = append(meta.Restore, "git fetch "+shq(filepath.Join(dir, bundleName))+" "+shq("refs/heads/"+it.Branch+":refs/heads/"+it.Branch))
	} else {
		meta.Restore = append(meta.Restore, "git branch "+shq(it.Branch)+" "+shq(res.Tip))
	}
	meta.Restore = append(meta.Restore, "git switch "+shq(it.Branch))
	if meta.Uncommit {
		meta.Restore = append(meta.Restore, "git apply "+shq(filepath.Join(dir, patchName)))
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

// shq is a single-quoted shell word.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// treeState is the worktree's content (a scratch-index tree, as the archive
// saves it) and branch tip, compared before and after the archive so work
// written meanwhile is never deleted unsaved.
func (o Options) treeState(ctx context.Context, admin string, it *Item, stderr io.Writer) (string, error) {
	cmd, err := wtgit.CommonCommand(ctx, it.paths.Common, wtgit.Opts{Stderr: stderr}, "rev-parse", "--verify", "refs/heads/"+it.Branch)
	if err != nil {
		return "", errDrift
	}
	tip, err := cmd.Output()
	if err != nil {
		return "", errors.New("could not read the branch tip")
	}
	root := o.archiveRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, ".tmp-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	tree, err := reap.ScratchTree(ctx, admin, it.Worktree, strings.TrimSpace(string(tip)), filepath.Join(tmp, "index"), true, stderr)
	if err != nil {
		return "", errors.New("could not read its uncommitted state")
	}
	return tree + "\n" + string(tip), nil
}

// inside reports whether dir is wt or below it, through symlinks.
func inside(dir, wt string) bool {
	d, errD := filepath.EvalSymlinks(dir)
	w, errW := filepath.EvalSymlinks(wt)
	if errD != nil || errW != nil {
		d, w = dir, wt
	}
	return strings.HasPrefix(d+"/", w+"/")
}
