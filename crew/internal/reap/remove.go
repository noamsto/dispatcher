package reap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/claimindex"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const tampered = "its .git changed during the reap; possible tampering: tell the human"

// remove is `_reap_remove`, the anchored removal tail shared by the candidate
// loop and --discard. Under --discard it re-checks the tree against the saved
// status and scratch-index tree. A non-zero code is a cwd-discovered git that
// failed (the arm's `set -e`).
func (r *run) remove(ctx context.Context, c *candidate) (removed bool, code int) {
	// Leftover processes reparented out of the pane but still rooted in this
	// worktree (the #187 class). Before the --dry-run return so dry runs
	// report them too.
	r.killLeftovers(ctx, c.wtpath)

	// The resume record is keyed by the worktree's own realpath, so it must be
	// computed while the directory still exists (#556).
	anchor, _ := wtgit.AnchorPath(c.wtpath)

	if r.dry {
		r.say("would reap " + c.branch + " (" + c.label + ") @ " + c.wtpath)
		r.say("would prune record " + anchor)
		return false, 0
	}

	// The re-check below treats any status line as dirt, so the scaffold must
	// also move out physically. --discard's patch already holds it, and its
	// re-check compares against the status saved with it, so it stays put.
	if r.discard == "" {
		r.trashScaffold(ctx, c)
	}
	r.killWindows(ctx, c.wtpath)
	// Remove anchored (#677): --git-dir=<common> discovers nothing through the
	// tree's worker-writable .git, and --force skips git's clean-check child.
	// The re-checks just before it are the keep gates.
	if !r.guardOK(gitguard.Check(r.paths.Common, c.admin, r.stderr)) ||
		!r.guardOK(gitguard.CheckCwd(r.paths.Common, r.trusted, r.stderr)) {
		r.keep(c, "git config drift")
		return false, 0
	}
	if !wtgit.GitlinkOK(c.admin, c.wtpath) {
		r.say("keeping " + c.branch + " — " + tampered)
		return false, 0
	}
	// --force skips git's own submodule refusal, and the status below hides
	// gitlinks: one staged since the first gate would go with its embedded repo.
	staged, ok := r.wtOutput(ctx, c, "ls-files", "--stage")
	if !ok {
		r.say("keeping " + c.branch + " — could not read its index")
		return false, 0
	}
	if hasGitlink(staged) {
		r.say("keeping " + c.branch + " — it has submodules")
		return false, 0
	}
	st, err := wtgit.Status(ctx, c.admin, c.wtpath, wtgit.Opts{Stderr: r.stderr}, "--untracked-files=all")
	if err != nil {
		r.say("keeping " + c.branch + " — git status failed")
		return false, 0
	}
	if r.discard != "" {
		if why := r.discardRecheck(ctx, c, st); why != "" {
			r.say("keeping " + c.branch + " — " + why)
			return false, 0
		}
	} else if st != "" {
		r.say("keeping " + c.branch + " — uncommitted changes")
		return false, 0
	}
	if cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: io.Discard}, "worktree", "remove", "--force", c.wtpath); err == nil {
		_ = cmd.Run()
	}
	// Judge success by the observable outcome, not the exit status (#194).
	left, code := r.worktreePath(ctx, c.branch)
	if code != 0 {
		return false, code
	}
	if _, err := os.Stat(c.wtpath); left != "" || err == nil {
		switch {
		case !filekind.IsDir(c.admin):
			// git validates the gitfile before deleting anything, so a gone
			// admin dir means a real tree whose contents only partly went.
			r.say("keeping " + c.branch + " — removal failed partway; " + c.wtpath + " is no longer a worktree: tell the human (fix its permissions, then remove it)")
			removeAnchor(anchor)
		case !wtgit.GitlinkOK(c.admin, c.wtpath):
			r.say("keeping " + c.branch + " — " + tampered)
		default:
			r.say("keeping " + c.branch + " — worktree removal failed")
		}
		return false, 0
	}

	suffix := ""
	if r.discard != "" {
		suffix = ", discarded"
	}
	r.say("reaped " + c.branch + " (" + c.label + suffix + ")")
	removeAnchor(anchor)
	if err := r.p.Append(r.paths.Log, r.reapRow(c)); err != nil {
		// Best-effort: the tree is gone, so the reaped row, forget, branch
		// delete and label release must still run, and the sweep go on.
		_, _ = fmt.Fprintf(r.stderr, "crew reap: could not record the reap row for %s: %v\n", c.branch, err)
	}
	r.postReaped(c)
	r.forget(c.branch)
	r.deleteBranch(ctx, c)
	r.releaseLabels(ctx, c)
	return true, 0
}

// removeAnchor prunes the resume record; a failed unlink must not stop the
// rows, branch delete and label release after it.
func removeAnchor(anchor string) {
	if anchor != "" {
		rmF(anchor)
	}
}

// rmF is `rm -f`, which never removes a directory (os.Remove would an empty one).
func rmF(path string) {
	_ = syscall.Unlink(path)
}

// killLeftovers signals every process whose cwd is in wt, this process and its
// ancestors excepted. A dangling cwd still matches by string.
func (r *run) killLeftovers(ctx context.Context, wt string) {
	for _, c := range r.p.Proc.Cwds(ctx) {
		if !strings.HasPrefix(c.Dir+"/", wt+"/") || c.PID == os.Getpid() || proc.IsAncestor(r.p.Proc, c.PID) {
			continue
		}
		pid := strconv.Itoa(c.PID)
		if r.dry {
			r.say("would kill pid " + pid + " (cwd " + c.Dir + ")")
			continue
		}
		r.p.Proc.Kill(c.PID)
		r.say("killed pid " + pid + " (cwd " + c.Dir + ")")
	}
}

// trashScaffold moves every untracked scaffold file to the trash, so a
// post-mortem can still recover it.
func (r *run) trashScaffold(ctx context.Context, c *candidate) {
	st, _ := wtgit.Status(ctx, c.admin, c.wtpath, wtgit.Opts{Stderr: r.stderr}, "--untracked-files=all")
	for line := range strings.SplitSeq(st, "\n") {
		if !scaffoldRe.MatchString(line) {
			continue
		}
		r.p.Gtrash(ctx, c.wtpath+"/"+strings.TrimPrefix(line, "?? "))
	}
}

// killWindows kills every window whose pane path or @worktree is wt.
func (r *run) killWindows(ctx context.Context, wt string) {
	rows := r.p.tmuxOut(ctx, "list-windows", "-a", "-F", "#{window_id} #{pane_current_path} #{@worktree}")
	for line := range strings.SplitSeq(rows, "\n") {
		f := strings.Fields(line)
		if (len(f) > 1 && f[1] == wt) || (len(f) > 2 && f[2] == wt) {
			_, _ = r.p.Tmux.Run(ctx, "kill-window", "-t", f[0])
		}
	}
}

// discardRecheck is --discard's keep gate before the removal: a new path or
// state change after the save is not in the patch. Status lines carry no
// content, so the tree comparison catches a later write to an already-dirty
// path. It returns why the tree is kept, "" to go on.
func (r *run) discardRecheck(ctx context.Context, c *candidate, st string) string {
	if st != r.discardSt {
		return "changed while saving"
	}
	tree := ""
	if tmp, err := os.MkdirTemp(r.paths.Dir, "discard."); err == nil {
		r.clean.setTmp(tmp)
		idx := tmp + "/index"
		if r.scratchTree(ctx, c, idx) {
			tree, _ = r.wtIndexOutput(ctx, c, idx, io.Discard, "write-tree")
		}
		_ = os.RemoveAll(tmp)
		r.clean.setTmp("")
	}
	if tree == "" {
		return "could not re-check its uncommitted state"
	}
	if tree != r.discardTree {
		return "changed while saving"
	}
	return ""
}

// scratchTree is `_discard_tree`: a throwaway index seeded from the branch
// tip, then `add -A`, which stages tracked, staged and untracked non-ignored
// files without touching the worker's own index.
func (r *run) scratchTree(ctx context.Context, c *candidate, idx string) bool {
	for _, args := range [][]string{{"read-tree", "refs/heads/" + c.branch}, {"add", "-A"}} {
		cmd, err := wtgit.Command(ctx, c.admin, c.wtpath, wtgit.Opts{Stderr: io.Discard, IndexFile: idx}, args...)
		if err != nil || cmd.Run() != nil {
			return false
		}
	}
	return true
}

// wtIndexOutput is `$(GIT_INDEX_FILE=idx _wt_git admin wt args…)` with git's
// stderr to stderr.
func (r *run) wtIndexOutput(ctx context.Context, c *candidate, idx string, stderr io.Writer, args ...string) (string, bool) {
	cmd, err := wtgit.Command(ctx, c.admin, c.wtpath, wtgit.Opts{Stderr: stderr, IndexFile: idx}, args...)
	if err != nil {
		return "", false
	}
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err == nil
}

// reapRow is the `kind:"reap"` row: the PR source `_reap_latest` falls back to
// and what the ratings records read.
func (r *run) reapRow(c *candidate) string {
	row := reapRowJSON{
		TS:       r.p.Now().UnixMilli(),
		Kind:     "reap",
		Branch:   c.branch,
		PR:       c.pr,
		PRState:  c.prState,
		Worktree: c.wtpath,
	}
	if c.mode == "issue" {
		row.Issues = append([]string{}, c.issues...)
	}
	if r.discard != "" {
		patch := r.discardPatch
		row.Discarded = &patch
	}
	return rowLine(row)
}

type reapRowJSON struct {
	TS        int64    `json:"ts"`
	Kind      string   `json:"kind"`
	Branch    string   `json:"branch"`
	PR        string   `json:"pr"`
	PRState   string   `json:"pr_state"`
	Worktree  string   `json:"worktree"`
	Issues    []string `json:"issues,omitzero"`
	Discarded *string  `json:"discarded,omitempty"`
}

// postReaped is `_reap_post_reaped`: the terminal `reaped` status row for the
// branch's newest session, only with a session and a crew. Never aborts the
// sweep.
func (r *run) postReaped(c *candidate) {
	if c.session == "" || c.scrow == "" {
		return
	}
	detail := c.label
	if c.mode == "pr" {
		detail = "PR #" + c.pr[strings.LastIndex(c.pr, "/")+1:] + " " + c.prState
	}
	_ = r.p.Append(r.paths.Log, rowLine(reapedRow{
		TS:     r.p.Now().UnixMilli(),
		CrewID: c.scrow,
		From:   c.session,
		To:     "dispatcher:" + c.scrow,
		Kind:   "status",
		Body:   reapedBody{State: "reaped", Detail: detail, Source: "reap"},
	}))
}

type reapedRow struct {
	TS     int64      `json:"ts"`
	CrewID string     `json:"crew_id"`
	From   string     `json:"from"`
	To     string     `json:"to"`
	Kind   string     `json:"kind"`
	Body   reapedBody `json:"body"`
}

type reapedBody struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
	Source string `json:"source"`
}

// deleteBranch deletes a MERGED PR's local branch: a squash-merged branch is
// never an ancestor of main, so git still reads it as unmerged. Only when the
// local tip is exactly the merged PR head — commits past the merge would be
// orphaned. A CLOSED PR's branch may hold work worth reviving, and --discard
// keeps it: its patch applies to that branch's tip.
func (r *run) deleteBranch(ctx context.Context, c *candidate) {
	ref := "refs/heads/" + c.branch
	if r.discard != "" || c.mode != "pr" || c.prState != "MERGED" ||
		exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", ref).Run() != nil {
		return
	}
	head, _ := r.p.Gh(ctx, "pr", "view", c.pr, "--json", "headRefOid", "--jq", ".headRefOid")
	switch {
	case head == "":
		r.note("kept local branch " + c.branch + " — could not verify the merged PR head")
	case r.revParse(ctx, ref) != head:
		r.note("kept local branch " + c.branch + " — tip diverges from the merged PR head")
	case !r.guardOK(gitguard.Check(r.paths.Common, r.paths.Common, r.stderr)):
		r.note("kept local branch " + c.branch + " — git config drift")
	default:
		if cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: io.Discard}, "branch", "-D", "--", c.branch); err == nil {
			_ = cmd.Run()
		}
	}
}

// revParse is `$(git rev-parse <ref>)` from the trusted cwd, git's stderr to
// the reap's.
func (r *run) revParse(ctx context.Context, ref string) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", ref)
	cmd.Stderr = r.stderr
	out, _ := cmd.Output()
	return strings.TrimRight(string(out), "\n")
}

// releaseLabels drops `dispatched` from the issues this branch closes.
// Best-effort: a Linear PR closes no GitHub issue, and a gh failure must not
// block the sweep — but a failed lookup is noted, so it cannot pass for "no
// closing issues" and leave the label stuck without a trace.
func (r *run) releaseLabels(ctx context.Context, c *candidate) {
	closing := strings.Join(c.issues, " ")
	if c.mode != "issue" {
		out, err := r.p.GhAll(ctx, "pr", "view", c.pr, "--json", "closingIssuesReferences", "--jq", ".closingIssuesReferences[].number")
		if err != nil {
			r.note("could not resolve closing issues for PR " + c.pr + " (" + c.branch + "): " + out)
			out = ""
		}
		closing = out
	}
	for _, issue := range strings.Fields(closing) {
		if r.p.GhQuiet(ctx, "issue", "edit", issue, "--remove-label", "dispatched") != nil {
			r.note("could not remove the dispatched label from #" + issue + " (" + c.branch + ")")
			continue
		}
		if err := claimindex.Release(r.paths.Common, r.o.Getenv, issue, "", r.p.Now()); err != nil {
			r.note("could not record the release of #" + issue + " in the claim index: " + err.Error())
		}
	}
}
