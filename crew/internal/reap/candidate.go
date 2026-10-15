package reap

import (
	"context"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// candidate is one branch reap considers: its `_reap_latest` row plus what the
// arm's loop variables carried into `_reap_remove`.
type candidate struct {
	latestRow
	wtpath string // the branch's worktree
	admin  string // its realpath admin dir, set by the admin gate
	mode   string // "pr", or "issue" when no PR vouches for the branch
	// pr is the PR URL ("-" for none yet, "" in issue mode); prSrc is "own"
	// only for the latest session's own pr_url, else "fallback".
	pr, prSrc string
	prState   string   // MERGED or CLOSED, or ISSUE_CLOSED in issue mode
	label     string   // what the outcome lines name: the PR state or "issue #n[, #m] CLOSED"
	issues    []string // the claimed issues, issue mode only
	// ignored is the last cross-repo pr_url skipped, as the note suffix names
	// it; nopr is issue mode's "<state> but no PR …" lead-in; why is a failed
	// lookup's reason.
	ignored, nopr, why string
}

// ign is `${ignored:+ ($ignored)}`.
func (c *candidate) ign() string {
	if c.ignored == "" {
		return ""
	}
	return " (" + c.ignored + ")"
}

// scaffoldRe is `reap_scaffold_re`: the untracked scaffold our own pipeline
// writes into every worktree, which must never read as uncommitted work. Keep
// it in sync with the writers or a new scaffold file pins the worktree
// forever: WORKER_TASK.md (dispatch.sh), SPEC.md/PLAN.md/DECOMPOSITION.md (the
// spec-plan-critic flow), REVIEW_NOTES.md (EVIDENCE_REVIEW.md's worktree-root
// ledger), docs/superpowers/plans/*.md and PLAN_ROUND<N>.md (the superpowers
// writing-plans skill). Anchored on the porcelain `?? ` prefix and the full
// path so a real file merely named e.g. `src/PLAN.md` still counts as dirt.
var scaffoldRe = regexp.MustCompile(`^\?\? (WORKER_TASK\.md|SPEC\.md|PLAN\.md|DECOMPOSITION\.md|REVIEW_NOTES\.md|PLAN_ROUND[0-9]+\.md|docs/superpowers/plans/[^/]*\.md)$`)

var (
	prURLRe   = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/[0-9]+$`)
	numericRe = regexp.MustCompile(`^[0-9]+$`)
)

// prURLInRepo is `_pr_url_in_repo`: a pr_url is unvalidated worker-written
// text, so it alone must never pick the repo gh is pointed at.
func prURLInRepo(url, repo string) bool {
	m := prURLRe.FindStringSubmatch(url)
	return m != nil && m[1]+"/"+m[2] == repo
}

// cntrlToQ is `${u//[[:cntrl:]]/?}`; bytes that are not UTF-8 pass through.
func cntrlToQ(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if r != utf8.RuneError && unicode.IsControl(r) {
			b.WriteByte('?')
		} else {
			b.WriteString(s[:n])
		}
		s = s[n:]
	}
	return b.String()
}

// pickPR is `_reap_pick_pr`: the first bus PR — own, earlier, reap row — that
// is a PR of this repo. A mismatch is never MERGED or CLOSED, only "no PR from
// that source"; ignored names the last one skipped.
func (r *run) pickPR(c *candidate) {
	c.pr, c.prSrc, c.ignored = "-", "fallback", ""
	src := "own"
	for _, u := range []string{c.own, c.earlier, c.reaprow} {
		if u != "-" {
			if prURLInRepo(u, r.repo) {
				c.pr, c.prSrc = u, src
				return
			}
			c.ignored = "ignoring cross-repo pr_url " + cntrlToQ(u)
		}
		src = "fallback"
	}
}

// findPR is `_reap_find_pr`: with no bus PR, look the branch's PR up on
// GitHub. False sets why.
func (r *run) findPR(ctx context.Context, c *candidate) bool {
	if c.pr != "-" {
		return true
	}
	c.prSrc = "fallback"
	prs, err := r.p.Gh(ctx, "pr", "list", "--head", c.branch, "--state", "all", "--json", "url,state,headRefOid,isCrossRepository")
	if err != nil {
		c.why = "could not list PRs for " + c.branch
		return false
	}
	if prs == "" {
		return true
	}
	pr, ok := prFromList(prs)
	if !ok {
		c.why = "could not read PRs for " + c.branch
		return false
	}
	c.pr = pr
	return true
}

// issueClosed is `_reap_issue_closed`: no PR, so a done/failed branch whose
// claimed issues are all CLOSED. It sets issues, pr, prState, label and nopr;
// false sets why.
func (r *run) issueClosed(ctx context.Context, c *candidate) bool {
	c.nopr = c.state + " but no PR on the bus or GitHub" + c.ign()
	if c.state != "done" && c.state != "failed" {
		c.why = c.nopr + "; only done/failed reclaim on a closed issue"
		return false
	}
	claimed, err := r.claimed(c.branch)
	if err != nil {
		c.why = c.nopr + "; could not read the bus"
		return false
	}
	if claimed == "" {
		c.why = c.nopr + "; no claim-issue row"
		return false
	}
	c.issues = nil
	for n := range strings.SplitSeq(claimed, "\n") {
		if !numericRe.MatchString(n) {
			c.why = c.nopr + "; claim-issue row names a non-numeric issue"
			return false
		}
		state, _ := r.p.Gh(ctx, "issue", "view", n, "--json", "state", "--jq", ".state")
		if state == "" {
			c.why = c.nopr + "; could not read issue #" + n
			return false
		}
		if state != "CLOSED" {
			c.why = c.nopr + "; issue #" + n + " is " + state
			return false
		}
		c.issues = append(c.issues, n)
	}
	c.pr, c.prState = "", "ISSUE_CLOSED"
	c.label = "issue #" + strings.Join(c.issues, ", #") + " CLOSED"
	return true
}

// keep is the loop's `note "keeping $branch — …"`.
func (r *run) keep(c *candidate, why string) {
	r.note("keeping " + c.branch + " — " + why)
}

// prGate is the PR-mode check: the PR is MERGED or CLOSED, and a PR that is
// not the latest session's own vouches only for a tip at or behind its head.
// False has noted why the branch is kept.
func (r *run) prGate(ctx context.Context, c *candidate) bool {
	c.prState, _ = r.p.Gh(ctx, "pr", "view", c.pr, "--json", "state", "--jq", ".state")
	switch c.prState {
	case "MERGED", "CLOSED":
	case "":
		r.keep(c, "could not read PR state ("+c.pr+")"+c.ign())
		return false
	default:
		r.keep(c, "PR "+c.prState+c.ign())
		return false
	}
	c.label = c.prState
	if c.prSrc == "own" {
		return true
	}
	head, _ := r.p.Gh(ctx, "pr", "view", c.pr, "--json", "headRefOid", "--jq", ".headRefOid")
	if head == "" {
		r.keep(c, "could not verify "+c.pr+"'s head"+c.ign())
		return false
	}
	if !r.guardOK(gitguard.Check(r.paths.Common, r.paths.Common, r.stderr)) {
		r.keep(c, "git config drift")
		return false
	}
	switch r.isAncestor(ctx, c.branch, head) {
	case 0:
		return true
	case 1:
		r.keep(c, "its tip has commits past "+c.pr+c.ign())
	default:
		r.keep(c, "could not compare with "+c.pr+" head (not fetched?)"+c.ign())
	}
	return false
}

// isAncestor is `_wt_git_common … merge-base --is-ancestor … 2>/dev/null`'s
// status; a guard refusal is the function's `return 1`.
func (r *run) isAncestor(ctx context.Context, branch, head string) int {
	cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: io.Discard}, "merge-base", "--is-ancestor", "refs/heads/"+branch, head)
	if err != nil {
		return 1
	}
	cmd.Stdout = r.stdout
	return exitStatus(cmd.Run())
}

// enginePane is one `live` row running an engine in the candidate's worktree.
type enginePane struct{ win, pane, cmd string }

// idleSample is a provably idle claude or pi pane's window, pane and first capture.
type idleSample struct{ win, pane, engine, frame string }

// enginePanes is the live panes running an engine at exactly wt.
func (r *run) enginePanes(wt string) []enginePane {
	var out []enginePane
	for line := range strings.SplitSeq(r.live, "\n") {
		f := fields(line, 4)
		if f[0] == "" || !paneIsEngineAt(f[2]+" "+f[3], wt) {
			continue
		}
		out = append(out, enginePane{f[0], f[1], f[2]})
	}
	return out
}

// paneIsEngineAt is `_pane_is_engine_at`: the path after the row's first space
// must equal wt exactly; the command before it must be an engine.
func paneIsEngineAt(row, wt string) bool {
	cmd, path, found := strings.Cut(row, " ")
	if !found {
		path = row
	}
	return path == wt && roster.IsEngineCmd(cmd)
}

// fields splits a tmux row on tabs into exactly n cells by position.
func fields(line string, n int) []string {
	f := strings.SplitN(line, "\t", n)
	for len(f) < n {
		f = append(f, "")
	}
	return f
}

// idleEngines is the engine-pane keep ladder: a branch with an engine in its
// tree is reclaimed only when it is done (or pr_open) with nothing posted since,
// and every engine pane is a claude or pi pane provably idle on its first sample. The
// idle samples come back for the re-check before the kill; false has noted why
// the branch is kept.
func (r *run) idleEngines(ctx context.Context, c *candidate) ([]idleSample, bool) {
	panes := r.enginePanes(c.wtpath)
	if len(panes) == 0 {
		return nil, true
	}
	keep := ""
	if c.state != "done" && c.state != "pr_open" {
		keep = c.state + " session"
	}
	if keep == "" && c.later == "1" {
		keep = "session posted after its " + c.state
	}
	var idle []idleSample
	if keep == "" {
		for _, p := range panes {
			engine := strings.TrimSuffix(strings.TrimPrefix(p.cmd, "."), "-wrapped")
			if engine != "claude" && engine != "pi" {
				keep = p.cmd + " has no idle signature"
				break
			}
			plain := r.p.Tmux(ctx, "capture-pane", "-p", "-t", p.pane)
			if plain == "" {
				keep = "empty capture of " + p.pane
				break
			}
			if why, ok := r.paneIdle(ctx, engine, p.pane, plain); !ok {
				keep = why
				break
			}
			idle = append(idle, idleSample{p.win, p.pane, engine, plain})
		}
	}
	if keep != "" {
		r.keep(c, "an engine is still running there ("+keep+")")
		return nil, false
	}
	return idle, true
}

// paneIdle is the engine's idle predicate over an already-taken plain capture.
// A pi keep reason names the pi frame.
func (r *run) paneIdle(ctx context.Context, engine, pane, plain string) (string, bool) {
	if engine == "pi" {
		why, ok := frame.PiIdleReason(plain)
		if !ok {
			why = "pi frame: " + why
		}
		return why, ok
	}
	colored := r.p.Tmux(ctx, "capture-pane", "-e", "-p", "-t", pane)
	return frame.PaneIdleReason(plain, colored, false)
}

// treeGates are the gates before reap's own anchored reads of the tree: the
// gitlink points at the real admin dir, no config.worktree, no submodule to
// recurse into (#539), and no uncommitted work beyond the scaffold. They set
// admin; false has noted why the branch is kept.
func (r *run) treeGates(ctx context.Context, c *candidate) bool {
	admin, err := wtgit.AdminDir(r.paths.Common, c.wtpath)
	if err != nil {
		r.keep(c, "no git admin dir for "+c.wtpath)
		return false
	}
	c.admin = admin
	if !wtgit.GitlinkOK(admin, c.wtpath) {
		r.keep(c, "its .git does not point at its git admin dir")
		return false
	}
	if _, err := os.Lstat(admin + "/config.worktree"); err == nil {
		r.keep(c, admin+"/config.worktree exists")
		return false
	}
	staged, ok := r.wtOutput(ctx, c, "ls-files", "--stage")
	if !ok {
		r.keep(c, "could not read its index")
		return false
	}
	if hasGitlink(staged) {
		r.keep(c, "it has submodules")
		return false
	}
	// --untracked-files=all: the default mode collapses a new untracked
	// directory to its own name, which never matches the plans pattern.
	st, err := wtgit.Status(ctx, admin, c.wtpath, wtgit.Opts{Stderr: r.stderr}, "--untracked-files=all")
	if err != nil {
		r.keep(c, "git status failed")
		return false
	}
	if dirt(st) {
		r.keep(c, "uncommitted changes")
		return false
	}
	return true
}

// wtOutput is `$(_wt_git admin wt args…)`.
func (r *run) wtOutput(ctx context.Context, c *candidate, args ...string) (string, bool) {
	cmd, err := wtgit.Command(ctx, c.admin, c.wtpath, wtgit.Opts{Stderr: r.stderr}, args...)
	if err != nil {
		return "", false
	}
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err == nil
}

// commonOutput is `$(_wt_git_common common args…)`.
func (r *run) commonOutput(ctx context.Context, args ...string) (string, bool) {
	cmd, err := wtgit.CommonCommand(ctx, r.paths.Common, wtgit.Opts{Stderr: r.stderr}, args...)
	if err != nil {
		return "", false
	}
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err == nil
}

// hasGitlink is `grep -q '^160000 '` over `ls-files --stage`.
func hasGitlink(staged string) bool {
	for line := range strings.SplitSeq(staged, "\n") {
		if strings.HasPrefix(line, "160000 ") {
			return true
		}
	}
	return false
}

// dirt is `$(printf '%s\n' "$st" | grep -vE "$reap_scaffold_re")` being
// non-empty: a status line that is not scaffold.
func dirt(st string) bool {
	var kept []string
	for line := range strings.SplitSeq(st, "\n") {
		if !scaffoldRe.MatchString(line) {
			kept = append(kept, line)
		}
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n") != ""
}

// pushedGate is issue mode's check: no PR vouches for the branch, so every
// commit must already be on a remote or patch-equivalent to one on the
// default branch. False has noted why the branch is kept.
func (r *run) pushedGate(ctx context.Context, c *candidate) bool {
	if !r.guardOK(gitguard.Check(r.paths.Common, r.paths.Common, r.stderr)) {
		r.keep(c, "git config drift")
		return false
	}
	def, ok := r.commonOutput(ctx, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
	if !ok {
		def = ""
		if _, ok := r.commonOutput(ctx, "show-ref", "--verify", "--quiet", "refs/remotes/origin/main"); ok {
			def = "refs/remotes/origin/main"
		}
	}
	if def == "" {
		r.keep(c, c.nopr+"; no origin default branch ref")
		return false
	}
	short := strings.TrimPrefix(def, "refs/remotes/")
	unpushed, ok := r.commonOutput(ctx, "rev-list", "refs/heads/"+c.branch, "--not", "--remotes")
	cherry, ok2 := "", false
	if ok {
		cherry, ok2 = r.commonOutput(ctx, "cherry", def, "refs/heads/"+c.branch)
	}
	if !ok || !ok2 {
		r.keep(c, c.nopr+"; could not compare its commits to "+short)
		return false
	}
	unpushedLines := strings.Split(unpushed, "\n")
	ahead := 0
	for line := range strings.SplitSeq(cherry, "\n") {
		mark, sha := splitMark(line)
		if mark == "+" && slices.Contains(unpushedLines, sha) {
			ahead++
		}
	}
	if ahead > 0 {
		r.keep(c, c.nopr+"; "+strconv.Itoa(ahead)+" commit(s) not on any remote and not patch-equivalent to "+short)
		return false
	}
	return true
}

// splitMark is `read -r mark sha` under the default IFS: the first field, and
// the rest with its surrounding blanks trimmed.
func splitMark(line string) (mark, rest string) {
	line = strings.Trim(line, " \t")
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimLeft(line[i:], " \t")
}

// killIdle kills the idle-engine windows, role panes with them. The first
// sample is seconds stale by now (gh, git status): a pane that changed at all,
// or a branch that posted since, keeps everything. False has noted why the
// branch is kept.
func (r *run) killIdle(ctx context.Context, c *candidate, idle []idleSample) bool {
	if !r.guardOK(gitguard.Check(r.paths.Common, c.admin, r.stderr)) ||
		!r.guardOK(gitguard.CheckCwd(r.paths.Common, r.trusted, r.stderr)) {
		r.keep(c, "git config drift")
		return false
	}
	resample := ""
	for _, s := range idle {
		plain := r.p.Tmux(ctx, "capture-pane", "-p", "-t", s.pane)
		if _, ok := r.paneIdle(ctx, s.engine, s.pane, plain); plain != s.frame || !ok {
			resample = "pane changed between samples"
			break
		}
	}
	if resample == "" && r.postedSince(c.branch, c.ts) {
		resample = "the branch posted since"
	}
	if resample != "" {
		r.keep(c, resample)
		return false
	}
	wins := make([]string, len(idle))
	for i, s := range idle {
		wins[i] = s.win
	}
	slices.Sort(wins)
	for _, w := range slices.Compact(wins) {
		if w == "" {
			continue
		}
		if r.dry {
			r.say("would kill window " + w + " (" + c.branch + " idle engine)")
			continue
		}
		r.p.TmuxRun(ctx, "kill-window", "-t", w)
		r.say("killed window " + w + " (" + c.branch + " idle engine)")
	}
	return true
}

// consider is one pass of the candidate loop, in the arm's order. It reports
// whether the branch was reclaimed, and a non-zero status when a cwd-discovered
// git failed (the arm's `set -e`).
func (r *run) consider(ctx context.Context, row latestRow) (bool, int) {
	c := &candidate{latestRow: row, mode: "pr"}
	wt, code := r.worktreePath(ctx, c.branch)
	if code != 0 {
		return false, code
	}
	if wt == "" || !isDir(wt) {
		return false, 0
	}
	c.wtpath = wt

	r.pickPR(c)
	if !r.findPR(ctx, c) {
		r.keep(c, c.why+c.ign())
		return false, 0
	}
	if c.pr == "-" {
		c.mode = "issue"
		if !r.issueClosed(ctx, c) {
			r.keep(c, c.why)
			return false, 0
		}
	} else if !r.prGate(ctx, c) {
		return false, 0
	}

	idle, ok := r.idleEngines(ctx, c)
	if !ok {
		return false, 0
	}
	// Never remove the worktree the caller is standing in: it would leave the
	// invoking shell (or dispatch itself) on a path that no longer exists.
	if strings.HasPrefix(r.reapCwd+"/", c.wtpath+"/") {
		r.keep(c, "it is the current worktree")
		return false, 0
	}
	if !r.treeGates(ctx, c) {
		return false, 0
	}
	if c.mode == "issue" && !r.pushedGate(ctx, c) {
		return false, 0
	}
	if len(idle) > 0 && !r.killIdle(ctx, c, idle) {
		return false, 0
	}
	return r.remove(ctx, c)
}
