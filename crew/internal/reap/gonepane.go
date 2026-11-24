package reap

import (
	"context"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// activeSession is a branch's newest worker session, still in a non-terminal state.
type activeSession struct {
	branch, session, crew string
}

// activeSessions is the newest worker status per branch whose state is not
// terminal. "reaped" is a finished session too, though terminalStates omits it.
func (r *run) activeSessions() []activeSession {
	newest := map[string]map[string]any{}
	var order []string
	for _, doc := range r.busDocs() {
		from, _ := doc["from"].(string)
		if doc["kind"] != "status" || !strings.HasPrefix(from, "worker:") {
			continue
		}
		branch := widBranch(from)
		if _, seen := newest[branch]; !seen {
			order = append(order, branch)
		}
		newest[branch] = doc
	}
	var out []activeSession
	for _, branch := range order {
		doc := newest[branch]
		body, _ := doc["body"].(map[string]any)
		state, _ := body["state"].(string)
		if slices.Contains(terminalStates, any(state)) || state == "reaped" || state == "" {
			continue
		}
		from, _ := doc["from"].(string)
		crew, _ := doc["crew_id"].(string)
		out = append(out, activeSession{branch, from, crew})
	}
	return out
}

// gonePane marks a non-terminal worker session exited when nothing of it is
// left alive: no window stamped for its branch, no engine pane in its worktree
// and no stall-watch for its worker id. A worker whose process died without
// posting a terminal status otherwise sits in `working` forever and its PR's
// landing is never acted on. Tmux must answer and list a window, else the pass
// stands down: a dead tmux server would read as every worker gone.
func (r *run) gonePane(ctx context.Context) {
	sessions := r.activeSessions()
	if len(sessions) == 0 {
		return
	}
	wins, err := r.p.Tmux.Run(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{@crew_branch}\t#{@crew_dir}")
	if err != nil || strings.TrimSpace(wins) == "" {
		return
	}
	panes, err := r.p.Tmux.Run(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}")
	if err != nil {
		return
	}
	r.live = panes
	porcelain, err := r.p.Git.Run(ctx, "", "worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	marked := false
	for _, s := range sessions {
		wt := roster.WorktreePath(porcelain, s.branch)
		if wt == "" || !filekind.IsDir(wt) {
			continue
		}
		if stampedIn(wins, s.branch, r.paths.Dir) || len(r.enginePanes(wt)) > 0 || r.stallWatching(ctx, s.session) {
			continue
		}
		row := latestRow{branch: s.branch, session: s.session, scrow: s.crew}
		if r.remoteDispatch(row) || r.snoozed(ctx, &candidate{latestRow: row, wtpath: wt}) {
			continue
		}
		if r.dry {
			r.note("would mark " + s.branch + " exited (engine pane and window gone)")
			continue
		}
		detail := "engine pane and window gone (no window stamped for " + s.branch + ", no engine pane in " + wt + ", stall-watch gone)"
		err := r.p.Append(rowLog(r.paths, s.crew), rowLine(reapedRow{
			TS:     r.p.Clock.Now().UnixMilli(),
			CrewID: s.crew,
			From:   s.session,
			To:     "dispatcher:" + s.crew,
			Kind:   "status",
			Body:   reapedBody{State: "exited", Detail: detail, Source: "reap"},
		}))
		if err != nil {
			continue
		}
		marked = true
		r.note("marked " + s.branch + " exited (engine pane and window gone)")
	}
	if marked {
		r.docs = nil
	}
}

// widBranch is the branch of a `worker:<branch>#<session>` id.
func widBranch(id string) string {
	id = strings.TrimPrefix(id, "worker:")
	if i := strings.LastIndex(id, "#"); i >= 0 {
		return id[:i]
	}
	return id
}

// stampedIn is true when a list-windows row is stamped for branch and this bus.
func stampedIn(rows, branch, dir string) bool {
	for line := range strings.SplitSeq(rows, "\n") {
		f := fields(line, 3)
		if f[0] != "" && f[1] == branch && f[2] == dir {
			return true
		}
	}
	return false
}

// stallWatching is true while a `crew stall-watch` runs for exactly this worker
// id. A role's stall-watch is named role:<branch>:<role>, so it never matches.
func (r *run) stallWatching(ctx context.Context, session string) bool {
	for _, c := range r.p.Proc.Cwds(ctx) {
		args := r.p.Proc.Args(c.PID)
		if strings.Contains(args, "stall-watch") && slices.Contains(strings.Fields(args), session) {
			return true
		}
	}
	return false
}
