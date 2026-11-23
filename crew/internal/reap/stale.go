package reap

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/snooze"
)

// keepClass is why a kept branch could need a human, for the stale classifier;
// every keep that is not one of these (an open PR, a live engine, drift, an
// unreadable or unverifiable anything) is classNone and never stale.
type keepClass int

const (
	classNone keepClass = iota
	classDirty
	classOpenIssue
	classUnpushed
	classNoDefault
)

// staleInfo is a stale keep's evidence: the reason, the worktree and the two
// commands a human runs to look at it and to drop it.
type staleInfo struct{ reason, worktree, inspect, remove string }

// staleRec is the newest `stale` bus row of a branch.
type staleRec struct {
	stale  bool
	reason string
	ts     float64
}

// shq is a single-quoted shell word.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// staleAfterText is the threshold as the reason names it: whole hours, else seconds.
func staleAfterText(secs int64) string {
	if secs > 0 && secs%3600 == 0 {
		return strconv.FormatInt(secs/3600, 10) + "h"
	}
	return strconv.FormatInt(secs, 10) + "s"
}

// keepHuman is keep for the outcomes a human may have to clean up: when the
// branch is also stale it prints and (outside --dry-run and --json) posts the
// evidence. The tree is never touched.
func (r *run) keepHuman(ctx context.Context, c *candidate, why string, class keepClass) {
	c.class = class
	st, stale := r.classifyStale(ctx, c, why)
	k := Keep{Repo: r.paths.Common, Crew: c.scrow, Branch: c.branch, Reason: why}
	if stale {
		k.Stale, k.StaleReason, k.Worktree, k.Inspect, k.Remove = true, st.reason, st.worktree, st.inspect, st.remove
	}
	if r.keeps != nil {
		r.keeps.keeps = append(r.keeps.keeps, k)
	}
	r.note("keeping " + c.branch + " — " + why)
	if !stale {
		return
	}
	r.note("  stale: needs human cleanup — " + st.reason)
	r.note("  inspect: " + st.inspect)
	r.note("  remove:  " + st.remove)
	if r.staleNow == nil {
		r.staleNow = map[string]bool{}
	}
	r.staleNow[c.branch] = true
	if !r.dry && !r.json {
		r.postStale(c, st)
	}
}

// classifyStale decides whether a keep of class c.class is stale (never while `crew cleanup` has it snoozed): a terminal
// worker with no engine pane whose claimed issues are all CLOSED or whose last
// status is older than --stale-after.
func (r *run) classifyStale(ctx context.Context, c *candidate, why string) (staleInfo, bool) {
	if c.class == classNone || r.snoozed(ctx, c) || !slices.Contains(terminalStates, any(c.state)) || len(r.enginePanes(c.wtpath)) > 0 {
		return staleInfo{}, false
	}
	var cause string
	switch ts := c.statusTS(); {
	case r.claimedClosed(ctx, c.branch):
		cause = "issue closed"
	case ts > 0 && (float64(r.p.Clock.Now().UnixMilli())-ts)/1000 > float64(r.staleSecs()):
		cause = "older than " + staleAfterText(r.staleSecs())
	default:
		return staleInfo{}, false
	}
	common := shq(r.paths.Common)
	return staleInfo{
		reason:   why + "; " + cause,
		worktree: c.wtpath,
		inspect:  "git -C " + shq(c.wtpath) + " status --short; git -C " + shq(c.wtpath) + " log --oneline " + shq(c.branch) + " --not --remotes",
		remove:   "git -C " + common + " worktree remove --force " + shq(c.wtpath) + " && git -C " + common + " branch -D " + shq(c.branch),
	}, true
}

// snoozed is true while `crew cleanup` holds this branch's snooze, which a new
// tip or worktree ends.
func (r *run) snoozed(ctx context.Context, c *candidate) bool {
	return snooze.Active(r.paths.Dir, c.branch, snooze.Key{Tip: r.revParse(ctx, "refs/heads/"+c.branch), Worktree: c.wtpath}, r.p.Clock.Now())
}

// statusTS is the latest status row's ts in ms, 0 when unreadable.
func (c *candidate) statusTS() float64 {
	ts, err := strconv.ParseFloat(c.ts, 64)
	if err != nil {
		return 0
	}
	return ts
}

// claimedClosed is true only when the branch has claim-issue rows and gh
// reads every one of them as CLOSED; anything unreadable is not closed.
func (r *run) claimedClosed(ctx context.Context, branch string) bool {
	claimed, err := r.claimed(branch)
	if err != nil || claimed == "" {
		return false
	}
	for n := range strings.SplitSeq(claimed, "\n") {
		if !numericRe.MatchString(n) {
			return false
		}
		if state, _ := r.p.Gh.Run(ctx, "issue", "view", n, "--json", "state", "--jq", ".state"); state != "CLOSED" {
			return false
		}
	}
	return true
}

// busDocs is the bus read once per run; the rows reap appends are tracked in
// staleRecs instead of re-read.
func (r *run) busDocs() []map[string]any {
	if r.docs != nil {
		return r.docs
	}
	r.docs = []map[string]any{}
	raws, err := readRaws(r.paths.Log)
	if err != nil {
		return r.docs
	}
	for _, raw := range raws {
		if doc, ok := raw.(map[string]any); ok {
			r.docs = append(r.docs, doc)
		}
	}
	return r.docs
}

// staleRecord is the newest `stale` row of branch, from the bus unless this
// run posted one.
func (r *run) staleRecord(branch string) staleRec {
	if rec, ok := r.staleRecs[branch]; ok {
		return rec
	}
	var rec staleRec
	for _, doc := range r.busDocs() {
		if doc["kind"] != "stale" || doc["branch"] != branch {
			continue
		}
		ts, _ := jsonFloat(doc["ts"])
		if ts < rec.ts {
			continue
		}
		stale, _ := doc["stale"].(bool)
		reason, _ := doc["reason"].(string)
		rec = staleRec{stale: stale, reason: reason, ts: ts}
	}
	return rec
}

func jsonFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case interface{ Float64() (float64, error) }:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

type staleRow struct {
	TS       int64  `json:"ts"`
	CrewID   string `json:"crew_id"`
	Kind     string `json:"kind"`
	Branch   string `json:"branch"`
	Session  string `json:"session"`
	Stale    bool   `json:"stale"`
	Reason   string `json:"reason,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Inspect  string `json:"inspect,omitempty"`
	Remove   string `json:"remove,omitempty"`
}

// appendStale writes one `stale` row and remembers it. Never aborts the sweep.
func (r *run) appendStale(c *candidate, row staleRow) {
	if c.session == "" || c.scrow == "" {
		return
	}
	now := r.p.Clock.Now().UnixMilli()
	row.TS, row.CrewID, row.Kind, row.Branch, row.Session = now, c.scrow, "stale", c.branch, c.session
	if err := r.p.Append(rowLog(r.paths, c.scrow), rowLine(row)); err != nil {
		return
	}
	if r.staleRecs == nil {
		r.staleRecs = map[string]staleRec{}
	}
	r.staleRecs[c.branch] = staleRec{stale: row.Stale, reason: row.Reason, ts: float64(now)}
}

// postStale appends a stale:true row unless the branch's newest stale row,
// newer than its last status, already says the same.
func (r *run) postStale(c *candidate, st staleInfo) {
	if prev := r.staleRecord(c.branch); prev.stale && prev.reason == st.reason && prev.ts > c.statusTS() {
		return
	}
	r.appendStale(c, staleRow{Stale: true, Reason: st.reason, Worktree: st.worktree, Inspect: st.inspect, Remove: st.remove})
}

// clearStale posts stale:false for a branch whose standing stale row is still
// in force but which this pass kept for some other reason (or not at all).
func (r *run) clearStale(row latestRow) {
	if r.dry || r.json || r.staleNow[row.branch] {
		return
	}
	c := &candidate{latestRow: row}
	if prev := r.staleRecord(row.branch); prev.stale && prev.ts > c.statusTS() {
		r.appendStale(c, staleRow{})
	}
}

// retireGhost retires a finished session whose worktree is already gone: it
// posts the `reaped` status and forgets the branch's records, as a reclaim
// would. Detached worktrees, a window stamped for the branch and remote
// dispatches are left alone, since reap cannot see their worktree.
func (r *run) retireGhost(ctx context.Context, row latestRow) (bool, int) {
	if !slices.Contains(terminalStates, any(row.state)) || row.session == "" || row.scrow == "" {
		return false, 0
	}
	out, err := r.p.Git.Run(ctx, "", "worktree", "list", "--porcelain")
	if err != nil {
		r.gitStderr(err)
		return false, shellStatus(err)
	}
	if worktreeAlive(out, row.branch) || detachedPresent(out) {
		return false, 0
	}
	if r.windowStamped(ctx, row.branch) || r.remoteDispatch(row) {
		return false, 0
	}
	if r.dry {
		r.note("would retire " + row.branch + " (worktree already gone)")
		return false, 0
	}
	r.postReaped(&candidate{latestRow: row, mode: "ghost", label: "worktree already gone"})
	r.forget(row.branch)
	r.note("retired " + row.branch + " (worktree already gone)")
	return true, 0
}

// worktreeAlive reports a listed worktree that may still be this branch's: one
// on the branch, or a non-main one in the dispatcher's slug-named directory
// (a worker that switched its tree to another branch). Paths are whole lines,
// so spaces survive.
func worktreeAlive(porcelain, branch string) bool {
	slug := strings.ReplaceAll(branch, "/", "-")
	for i, block := range strings.Split(porcelain, "\n\n") {
		var dir, onBranch string
		for line := range strings.SplitSeq(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				dir = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch "):
				onBranch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
			}
		}
		if dir == "" || !filekind.IsDir(dir) {
			continue
		}
		if onBranch == branch || (i > 0 && filepath.Base(dir) == slug) {
			return true
		}
	}
	return false
}

// detachedPresent reports a listed detached worktree whose directory exists:
// its branch cannot be told, so it could be this one.
func detachedPresent(porcelain string) bool {
	var dir string
	for line := range strings.SplitSeq(porcelain, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			dir = strings.TrimPrefix(line, "worktree ")
		case line == "detached" && dir != "" && filekind.IsDir(dir):
			return true
		}
	}
	return false
}

// windowStamped is true when a tmux window is stamped for this bus and branch.
func (r *run) windowStamped(ctx context.Context, branch string) bool {
	rows := r.p.tmuxOut(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{@crew_branch}\t#{@crew_dir}")
	for line := range strings.SplitSeq(rows, "\n") {
		f := fields(line, 3)
		if f[0] != "" && f[1] == branch && f[2] == r.paths.Dir {
			return true
		}
	}
	return false
}

// remoteDispatch is true when the branch's newest dispatch row is remote.
func (r *run) remoteDispatch(row latestRow) bool {
	docs := r.busDocs()
	for i := len(docs) - 1; i >= 0; i-- {
		doc := docs[i]
		if doc["kind"] != "dispatch" || doc["branch"] != row.branch || doc["crew_id"] != row.scrow {
			continue
		}
		return doc["remote"] == true
	}
	return false
}
