package reap

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// discardBranch is the `--discard <branch>` block: a human's explicit call to
// drop a finished worker's uncommitted state. Every refusal touches nothing;
// the state is saved as a patch before the shared removal tail runs. It
// returns the process exit code.
func (r *run) discardBranch(ctx context.Context) int {
	if !r.p.HaveGh() {
		return r.refuse("needs gh")
	}
	// --branch also expands @{-N}, so the name must come back unchanged.
	canon, err := exec.CommandContext(ctx, "git", "check-ref-format", "--branch", r.discard).Output()
	if err != nil || strings.TrimRight(string(canon), "\n") != r.discard {
		return r.refuse("not a valid branch name")
	}
	c := &candidate{mode: "pr"}
	c.branch = r.discard
	wt, code := r.worktreePath(ctx, c.branch)
	if code != 0 {
		return code
	}
	if wt == "" || !filekind.IsDir(wt) {
		return r.refuse("no worktree for it")
	}
	c.wtpath = wt
	rows, err := r.latest(c.branch)
	if err != nil {
		return r.refuse("could not read the bus")
	}
	c.state, c.own, c.earlier, c.reaprow = "none", "-", "-", "-"
	if len(rows) > 0 {
		c.latestRow = rows[0]
	}
	switch c.state {
	case "done", "failed", "exited":
	default:
		return r.refuse("latest status is " + c.state)
	}
	r.live = r.p.tmuxOut(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}")
	for line := range strings.SplitSeq(r.live, "\n") {
		f := fields(line, 4)
		if f[2] != "" && paneIsEngineAt(f[2]+" "+f[3], wt) {
			return r.refuse("an engine is running there")
		}
	}
	if strings.HasPrefix(r.reapCwd+"/", wt+"/") {
		return r.refuse("it is the current worktree")
	}
	r.pickPR(c)
	if !r.findPR(ctx, c) {
		return r.refuse(c.why)
	}
	if c.pr == "-" {
		c.mode = "issue"
		if !r.issueClosed(ctx, c) {
			return r.refuse(c.why)
		}
	} else {
		c.prState, _ = r.p.Gh(ctx, "pr", "view", c.pr, "--json", "state", "--jq", ".state")
		switch c.prState {
		case "MERGED", "CLOSED":
		case "":
			return r.refuse("could not read PR state (" + c.pr + ")")
		default:
			return r.refuse("PR " + c.prState)
		}
		c.label = c.prState
	}
	if why := r.discardGates(ctx, c); why != "" {
		return r.refuse(why)
	}

	preSt, err := wtgit.Status(ctx, c.admin, wt, wtgit.Opts{Stderr: r.stderr}, "--untracked-files=all")
	if err != nil {
		return r.refuse("git status failed")
	}
	patch := r.paths.Dir + "/artifacts/" + c.branch + "/discarded-" + r.p.Now().UTC().Format("20060102T150405Z") + ".patch"
	if r.dry {
		if preSt != "" {
			r.say("would save uncommitted state to " + patch)
		} else {
			r.say("no uncommitted changes to save")
		}
		r.say("would reap " + c.branch + " (" + c.label + ") @ " + wt)
		return 0
	}
	// $dir is ours, so the temp dir and the patch built in it are too.
	tmp, err := os.MkdirTemp(r.paths.Dir, "discard.")
	if err != nil {
		return r.refuse("could not save uncommitted state")
	}
	r.clean.setTmp(tmp)
	saved, tree, why := r.savePatch(ctx, c, tmp, patch)
	_ = os.RemoveAll(tmp)
	r.clean.setTmp("")
	if why != "" {
		return r.refuse(why)
	}
	if saved {
		r.say("saved uncommitted state to " + patch)
	} else {
		patch = ""
		r.say("no uncommitted changes to save")
	}
	if postSt, err := wtgit.Status(ctx, c.admin, wt, wtgit.Opts{Stderr: r.stderr}, "--untracked-files=all"); err != nil || postSt != preSt {
		r.say("keeping " + c.branch + " — changed while saving")
		return 1
	}
	r.discardPatch, r.discardSt, r.discardTree = patch, preSt, tree
	removed, code := r.remove(ctx, c)
	if code != 0 {
		return code
	}
	if !removed {
		return 1
	}
	return 0
}

// discardGates are the tree gates --discard refuses on: a real admin dir the
// gitlink points at, no config.worktree, no submodules, no config drift. They
// set admin and return why the branch is refused, "" to go on.
func (r *run) discardGates(ctx context.Context, c *candidate) string {
	admin, err := wtgit.AdminDir(r.paths.Common, c.wtpath)
	if err != nil {
		return "no git admin dir for " + c.wtpath
	}
	c.admin = admin
	if !wtgit.GitlinkOK(admin, c.wtpath) {
		return "its .git does not point at its git admin dir"
	}
	if _, err := os.Lstat(admin + "/config.worktree"); err == nil {
		return admin + "/config.worktree exists"
	}
	staged, ok := r.wtOutput(ctx, c, "ls-files", "--stage")
	if !ok {
		return "could not read its index"
	}
	if hasGitlink(staged) {
		return "it has submodules"
	}
	if !r.guardOK(gitguard.Check(r.paths.Common, admin, r.stderr)) ||
		!r.guardOK(gitguard.CheckCwd(r.paths.Common, r.trusted, r.stderr)) {
		return "git config drift"
	}
	return ""
}

// savePatch builds the scratch-index tree in tmp and, when it differs from
// the branch tip, moves its patch to the artifacts path without clobbering.
// It returns whether a patch was saved, the tree, and why the save was
// refused ("" when it was not).
func (r *run) savePatch(ctx context.Context, c *candidate, tmp, patch string) (saved bool, tree, why string) {
	const failed = "could not save uncommitted state"
	idx := tmp + "/index"
	if !r.scratchTree(ctx, c, idx) {
		return false, "", failed
	}
	// add -A stages an untracked nested repo as a bare gitlink: the patch would
	// hold only its commit id while the removal deletes its contents.
	stage, ok := r.wtIndexOutput(ctx, c, idx, r.stderr, "ls-files", "--stage")
	if !ok {
		return false, "", failed
	}
	if hasGitlink(stage) {
		return false, "", "it holds an embedded git repository"
	}
	tree, ok = r.wtIndexOutput(ctx, c, idx, r.stderr, "write-tree")
	if !ok {
		return false, "", failed
	}
	tmpPatch := tmp + "/patch"
	if !r.writeDiff(ctx, c, idx, tmpPatch) {
		return false, "", failed
	}
	if fi, err := os.Stat(tmpPatch); err != nil || fi.Size() == 0 {
		return false, tree, ""
	}
	// artifacts/<branch> is worker-writable: a symlink on it redirects the patch.
	artifacts := r.paths.Dir + "/artifacts"
	if bad := chainBad(artifacts, c.branch); bad != "" {
		return false, "", bad + " is a symlink or not a directory"
	}
	if os.MkdirAll(artifacts+"/"+c.branch, 0o755) != nil {
		return false, "", failed
	}
	if bad := chainBad(artifacts, c.branch); bad != "" {
		return false, "", bad + " is a symlink or not a directory"
	}
	if _, err := os.Lstat(patch); err == nil {
		return false, "", patch + " already exists"
	}
	if os.Link(tmpPatch, patch) == nil {
		_ = os.Remove(tmpPatch)
	}
	_, leftErr := os.Lstat(tmpPatch)
	if fi, err := os.Lstat(patch); leftErr == nil || err != nil || !fi.Mode().IsRegular() {
		return false, "", failed
	}
	return true, tree, ""
}

// writeDiff writes the scratch index's diff against the branch tip to path.
// Pinned diff options: the operator's diff.* and color.* config would
// otherwise reshape the patch so `git apply` rejects it.
func (r *run) writeDiff(ctx context.Context, c *candidate, idx, path string) bool {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return false
	}
	defer func() { _ = out.Close() }()
	cmd, err := wtgit.Command(ctx, c.admin, c.wtpath, wtgit.Opts{Stderr: io.Discard, IndexFile: idx},
		"diff", "--cached", "--binary", "--full-index", "--no-color", "--src-prefix=a/", "--dst-prefix=b/",
		"--no-relative", "--no-ext-diff", "--no-textconv", "refs/heads/"+c.branch, "--")
	if err != nil {
		return false
	}
	cmd.Stdout = out
	return cmd.Run() == nil
}
