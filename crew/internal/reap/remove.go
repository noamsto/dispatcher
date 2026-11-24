package reap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/claimindex"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const tampered = "its .git changed during the reap; possible tampering: tell the human"

// remove is `_reap_remove`, the anchored removal tail shared by the candidate
// loop and --discard. Under --discard it re-checks the tree against the saved
// status and scratch-index tree. A non-zero code is a cwd-discovered git that
// failed (the arm's `set -e`).
func (r *run) remove(ctx context.Context, c *candidate) (removed bool, code int) {
	// The resume record is keyed by the worktree's own realpath, so it must be
	// computed while the directory still exists (#556).
	anchor, _ := wtgit.AnchorPath(c.wtpath)
	// The row's crew id comes from sources the kills and removal below destroy.
	c.crewID = rowCrewID(ctx, r.p.Tmux, worktreeWindows(ctx, r.p.Tmux, c.wtpath), c.wtpath, c.branch, r.busDocs, indexCrewDocs(r.paths))

	// Leftover processes reparented out of the pane but still rooted in this
	// worktree (the #187 class). Before the --dry-run return so dry runs
	// report them too.
	killLeftovers(ctx, r.p.Proc, c.wtpath, r.dry, r.say)

	if r.dry {
		r.say("would reap " + c.branch + " (" + c.label + ") @ " + c.wtpath)
		r.say("would prune record " + anchor)
		return false, 0
	}

	// The re-check below treats any status line as dirt, so the scaffold must
	// also move out physically. --discard's patch already holds it, and its
	// re-check compares against the status saved with it, so it stays put; so
	// does a migrated tree's, which its re-check compares to the mark's tree.
	if r.discard == "" && !c.markTree {
		r.trashScaffold(ctx, c)
	}
	killWindows(ctx, r.p.Tmux, c.wtpath)
	// Remove anchored (#677): --git-dir=<common> discovers nothing through the
	// tree's worker-writable .git, and --force skips git's clean-check child.
	// The re-checks just before it are the keep gates.
	if !r.guardOK(gitguard.Check(r.paths.Common, c.admin, r.stderr)) ||
		!r.guardOK(gitguard.CheckCwd(r.paths.Common, r.trusted, r.stderr)) {
		r.keep(c, "git config drift (run `crew git-baseline`)")
		return false, 0
	}
	if !wtgit.GitlinkOK(c.admin, c.wtpath) {
		r.keepSay(c, tampered)
		return false, 0
	}
	// --force skips git's own submodule refusal, and the status below hides
	// gitlinks: one staged since the first gate would go with its embedded repo.
	staged, ok := r.wtOutput(ctx, c, "ls-files", "--stage")
	if !ok {
		r.keepSay(c, "could not read its index")
		return false, 0
	}
	if hasGitlink(staged) {
		r.keepSay(c, "it has submodules")
		return false, 0
	}
	st, err := wtgit.Status(ctx, c.admin, c.wtpath, wtgit.Opts{Stderr: r.stderr}, "--untracked-files=all")
	if err != nil {
		r.keepSay(c, "git status failed")
		return false, 0
	}
	switch {
	case r.discard != "":
		if why := r.discardRecheck(ctx, c, st); why != "" {
			r.keepSay(c, why)
			return false, 0
		}
	case c.markTree:
		if !r.holdsMarkTree(ctx, c) {
			r.keepSay(c, "changed since its migrate snapshot")
			return false, 0
		}
	case st != "":
		r.keepSay(c, "uncommitted changes")
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
			r.keepSay(c, "removal failed partway; "+c.wtpath+" is no longer a worktree: tell the human (fix its permissions, then remove it)")
			removeAnchor(anchor)
		case !wtgit.GitlinkOK(c.admin, c.wtpath):
			r.keepSay(c, tampered)
		default:
			r.keepSay(c, "worktree removal failed")
		}
		return false, 0
	}

	suffix := ""
	if r.discard != "" {
		suffix = ", discarded"
	}
	r.say("reaped " + c.branch + " (" + c.label + suffix + ")")
	removeAnchor(anchor)
	if err := r.p.Append(rowLog(r.paths, c.crewID), r.reapRow(c)); err != nil {
		// Best-effort: the tree is gone, so the reaped row, forget, branch
		// delete and label release must still run, and the sweep go on.
		_, _ = fmt.Fprintf(r.stderr, "crew reap: could not record the reap row for %s: %v\n", c.branch, err)
	}
	r.postReaped(c)
	r.forget(c.branch, c.crewID)
	r.deleteBranch(ctx, c)
	r.deleteOriginBranch(ctx, c)
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

// KillWorkerWindows stops the worker at wt: its windows, matched by pane path
// or the @worktree stamp (never a name), then the processes still rooted in
// wt. say reports each signalled process.
func KillWorkerWindows(ctx context.Context, tm tmux.Client, pt proc.Table, wt string, say func(string)) {
	killWindows(ctx, tm, wt)
	killLeftovers(ctx, pt, wt, false, say)
}

// killLeftovers signals every process whose cwd is in wt, this process and its
// ancestors excepted. A dangling cwd still matches by string.
func killLeftovers(ctx context.Context, pt proc.Table, wt string, dry bool, say func(string)) {
	anc := proc.Ancestors(pt)
	for _, c := range pt.Cwds(ctx) {
		if !strings.HasPrefix(c.Dir+"/", wt+"/") || c.PID == os.Getpid() || anc[c.PID] {
			continue
		}
		pid := strconv.Itoa(c.PID)
		if dry {
			say("would kill pid " + pid + " (cwd " + c.Dir + ")")
			continue
		}
		pt.Kill(c.PID)
		say("killed pid " + pid + " (cwd " + c.Dir + ")")
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
func killWindows(ctx context.Context, tm tmux.Client, wt string) {
	for _, w := range worktreeWindows(ctx, tm, wt) {
		_, _ = tm.Run(ctx, "kill-window", "-t", w)
	}
}

// worktreeWindows lists the ids of the windows whose pane path or @worktree is wt.
func worktreeWindows(ctx context.Context, tm tmux.Client, wt string) []string {
	var ids []string
	rows, _ := tm.Run(ctx, "list-windows", "-a", "-F", "#{window_id} #{pane_current_path} #{@worktree}")
	for line := range strings.SplitSeq(rows, "\n") {
		f := strings.Fields(line)
		if (len(f) > 1 && f[1] == wt) || (len(f) > 2 && f[2] == wt) {
			ids = append(ids, f[0])
		}
	}
	return ids
}

// discardRecheck is --discard's keep gate before the removal: a new path or
// state change after the save is not in the patch. Status lines carry no
// content, so the tree comparison catches a later write to an already-dirty
// path. It returns why the tree is kept, "" to go on.
func (r *run) discardRecheck(ctx context.Context, c *candidate, st string) string {
	if st != r.discardSt {
		return "changed while saving"
	}
	tree := r.scratchTree(ctx, c, "discard.", "refs/heads/"+c.branch, false)
	if tree == "" {
		return "could not re-check its uncommitted state"
	}
	if tree != r.discardTree {
		return "changed while saving"
	}
	return ""
}

// ignoredScaffoldRe is the root scaffold a scratch tree carries even when the
// repo gitignores it: scaffoldRe's root names minus WORKER_TASK.md, which a
// migrated worker's destination writes itself.
var ignoredScaffoldRe = regexp.MustCompile(`^(SPEC|PLAN|DECOMPOSITION|REVIEW_NOTES|PLAN_ROUND[0-9]+)\.md$`)

// ScratchTree is `_discard_tree`: a throwaway index at idx seeded from base,
// then `add -A`, which stages tracked, staged and untracked non-ignored files
// without touching the worker's own index. ignoredScaffold also force-adds the
// ignoredScaffoldRe files at the worktree root, so a mid-run worker's
// gitignored plan files are in the tree. It returns the index's tree; stderr
// gets write-tree's own errors.
func ScratchTree(ctx context.Context, admin, wt, base, idx string, ignoredScaffold bool, stderr io.Writer) (string, error) {
	steps := [][]string{{"read-tree", base}, {"add", "-A"}}
	if ignoredScaffold {
		if names := rootScaffold(wt); len(names) > 0 {
			steps = append(steps, append([]string{"add", "-f", "--"}, names...))
		}
	}
	for _, args := range steps {
		cmd, err := wtgit.Command(ctx, admin, wt, wtgit.Opts{Stderr: io.Discard, IndexFile: idx}, args...)
		if err != nil {
			return "", err
		}
		if err := cmd.Run(); err != nil {
			return "", err
		}
	}
	tree, ok := indexOutput(ctx, admin, wt, idx, stderr, "write-tree")
	if !ok {
		return "", errors.New("git write-tree failed")
	}
	return tree, nil
}

// rootScaffold is the ignoredScaffoldRe regular files at wt's root. Found by
// a directory read, not a pathspec: add fails on a pathspec for a missing
// file, and a directory would pull in all its ignored contents. A name add -A
// already staged is re-added unchanged.
func rootScaffold(wt string) []string {
	entries, _ := os.ReadDir(wt)
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && ignoredScaffoldRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

// indexOutput is `$(GIT_INDEX_FILE=idx _wt_git admin wt args…)` with git's
// stderr to stderr; an empty idx leaves the worktree's own index.
func indexOutput(ctx context.Context, admin, wt, idx string, stderr io.Writer, args ...string) (string, bool) {
	cmd, err := wtgit.Command(ctx, admin, wt, wtgit.Opts{Stderr: stderr, IndexFile: idx}, args...)
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
		TS:       r.p.Clock.Now().UnixMilli(),
		Kind:     "reap",
		CrewID:   c.crewID,
		Branch:   c.branch,
		Repo:     repoTag(r.paths.Common),
		PR:       c.pr,
		PRState:  c.prState,
		Worktree: c.wtpath,
	}
	if c.mode == "migrated" {
		row.MigratedTo = c.rel.ToHost
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
	TS         int64    `json:"ts"`
	Kind       string   `json:"kind"`
	CrewID     string   `json:"crew_id,omitempty"`
	Branch     string   `json:"branch"`
	Repo       string   `json:"repo,omitempty"`
	PR         string   `json:"pr"`
	PRState    string   `json:"pr_state"`
	Worktree   string   `json:"worktree"`
	MigratedTo string   `json:"migrated_to,omitempty"`
	Issues     []string `json:"issues,omitzero"`
	Discarded  *string  `json:"discarded,omitempty"`
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
	_ = r.p.Append(rowLog(r.paths, c.scrow), rowLine(reapedRow{
		TS:     r.p.Clock.Now().UnixMilli(),
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
// keeps it: its patch applies to that branch's tip. A migrated branch goes
// while its tip is still the mark's, which its gate found on origin.
func (r *run) deleteBranch(ctx context.Context, c *candidate) {
	ref := "refs/heads/" + c.branch
	migrated := c.mode == "migrated"
	if r.discard != "" || !migrated && (c.mode != "pr" || c.prState != "MERGED") {
		return
	}
	if _, err := r.p.Git.Run(ctx, "", "show-ref", "--verify", "--quiet", ref); err != nil {
		return
	}
	want, what := c.rel.Tip, "the migrate mark's tip"
	if !migrated {
		what = "the merged PR head"
		want, _ = r.p.Gh.Run(ctx, "pr", "view", c.pr, "--json", "headRefOid", "--jq", ".headRefOid")
	}
	switch {
	case want == "":
		r.note("kept local branch " + c.branch + " — could not verify the merged PR head")
	case r.revParse(ctx, ref) != want:
		r.note("kept local branch " + c.branch + " — tip diverges from " + what)
	case !r.guardOK(gitguard.Check(r.paths.Common, r.paths.Common, r.stderr)):
		r.note("kept local branch " + c.branch + " — git config drift")
	default:
		if cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: io.Discard}, "branch", "-D", "--", c.branch); err == nil {
			_ = cmd.Run()
		}
	}
}

var oidRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// deleteOriginBranch removes the origin branch dispatch pushed at its base, so
// a finished no-PR crew stops reading as a claim on another machine. The lease
// makes it a compare-and-delete: origin deletes only while the tip is still
// the recorded dispatch oid, so a branch the worker pushed to stays. A worker
// can forge the recorded oid, so only feat/ branches that are not origin's
// default are touched, and only when the tip holds no commit absent from this
// clone's other remote-tracking refs. That check is local: a worker that can
// write those refs, or a stale unpruned one, can still satisfy it.
func (r *run) deleteOriginBranch(ctx context.Context, c *candidate) {
	if c.mode != "issue" || !strings.HasPrefix(c.branch, "feat/") {
		return
	}
	if def, ok := r.commonOutput(ctx, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); ok &&
		strings.TrimPrefix(def, "refs/remotes/origin/") == c.branch {
		return
	}
	oid := r.pushedOID(c.branch)
	if oid == "" {
		return
	}
	out, ok := r.commonOutput(ctx, "rev-list", oid, "--not", "--exclude=origin/"+c.branch, "--remotes")
	if !ok {
		r.note("kept origin branch " + c.branch + " — could not check its tip against other remote-tracking refs")
		return
	}
	if out != "" {
		r.note("kept origin branch " + c.branch + " — its tip holds commits no other remote-tracking ref has")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ref := "refs/heads/" + c.branch
	var stderr bytes.Buffer
	cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: &stderr},
		"-c", "hook.pre-push.enabled=false", "-c", "http.lowSpeedLimit=1", "-c", "http.lowSpeedTime=20",
		"push", "-q", "--no-verify", "--force-with-lease="+ref+":"+oid, "origin", ":"+ref)
	if err == nil {
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
		// The context kills git, but a transport child holding stderr would
		// still block Run.
		cmd.WaitDelay = 2 * time.Second
		err = cmd.Run()
	}
	switch {
	case err == nil:
		r.note("deleted origin branch " + c.branch + " (still at its dispatch base)")
	case strings.Contains(stderr.String(), "(stale info)"):
		r.note("kept origin branch " + c.branch + " — origin moved past its dispatch base")
	default:
		r.say("could not delete origin branch " + c.branch + " (still at its dispatch base?) — delete it by hand once origin is reachable")
	}
}

// pushedOID is the oid the branch's newest create row recorded as pushed to
// origin, "" when the row has none or it is not a full object id. Resume rows
// never push, so they must not mask the create row.
func (r *run) pushedOID(branch string) string {
	raws, err := readRaws(r.paths.Log)
	if err != nil {
		return ""
	}
	oid := ""
	for _, raw := range raws {
		row, _ := raw.(map[string]any)
		if row["kind"] != "dispatch" || row["branch"] != branch || row["resume"] == true {
			continue
		}
		oid, _ = row["pushed_oid"].(string)
	}
	if !oidRe.MatchString(oid) {
		return ""
	}
	return oid
}

// revParse is `$(git rev-parse <ref>)` from the trusted cwd, git's stderr to
// the reap's.
func (r *run) revParse(ctx context.Context, ref string) string {
	out, err := r.p.Git.Run(ctx, "", "rev-parse", ref)
	if err != nil {
		r.gitStderr(err)
	}
	return out
}

// releaseLabels drops `dispatched` from the issues this branch closes.
// Best-effort: a Linear PR closes no GitHub issue, and a gh failure must not
// block the sweep — but a failed lookup is noted, so it cannot pass for "no
// closing issues" and leave the label stuck without a trace.
func (r *run) releaseLabels(ctx context.Context, c *candidate) {
	// A migrated worker's issue is still being worked, on the other host; a
	// pushed worker's issue is still open.
	if c.mode == "migrated" || c.mode == "pushed" {
		return
	}
	closing := strings.Join(c.issues, " ")
	if c.mode != "issue" {
		out, err := r.p.Gh.Combined(ctx, "pr", "view", c.pr, "--json", "closingIssuesReferences", "--jq", ".closingIssuesReferences[].number")
		if err != nil {
			r.note("could not resolve closing issues for PR " + c.pr + " (" + c.branch + "): " + out)
			out = ""
		}
		closing = out
	}
	for _, issue := range strings.Fields(closing) {
		if _, err := r.p.Gh.Run(ctx, "issue", "edit", issue, "--remove-label", "dispatched"); err != nil {
			r.note("could not remove the dispatched label from #" + issue + " (" + c.branch + ")")
			continue
		}
		if err := claimindex.Release(ctx, r.p.Git, r.paths.Common, r.p.Getenv, issue, "", r.p.Clock.Now()); err != nil {
			r.note("could not record the release of #" + issue + " in the claim index: " + err.Error())
		}
	}
}
