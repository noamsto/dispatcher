// Package sender is the shared check the bus write paths (msg, status, reply,
// hold) run on a row's self-asserted `from`.
//
// It is a tripwire on the sanctioned write path, not authentication: every
// session runs as the same uid, and a deliberate process can unset its env or
// double-fork out of the pane tree. Without an anchor pane (outside tmux, a
// nohup'd process reparented away from its pane, a pid-namespaced sandbox that
// hides the host ancestor) only the env rule applies.
package sender

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// Format is the `tmux list-panes -a -F` format Probes.Tmux runs; window options
// resolve through it, so each line is the pane's pid, its window's stamps, and
// the window and pane ids.
const Format = "#{pane_pid}\x1f#{@crew_branch}\x1f#{@crew_role}\x1f#{@crew_id}\x1f#{window_id}\x1f#{pane_id}"

// Probes are the reads Check makes; the zero value checks nothing.
type Probes struct {
	Getenv func(string) string // nil: no env
	Tmux   tmux.Client         // reads list-panes -a -F Format; nil: no tmux
	Proc   proc.Table          // walks the caller's ancestry; nil: no ancestry
}

type stamp struct {
	pid                              int
	branch, role, crew, window, pane string
}

// Check returns "" when this caller may post a row from `from` into `crew`,
// else the refusal reason (no "crew: <sub>:" prefix, no trailing newline).
func Check(ctx context.Context, from, crew string, p Probes) string {
	if reason := p.envRefusal(from); reason != "" {
		return reason
	}
	s, ok := p.anchor(p.rows(ctx))
	if !ok || s.branch == "" {
		return ""
	}
	return s.refuse(from, crew, "this pane")
}

// Watch is Check for a watchdog: the --pane it watches must also be stamped for
// `from` (when stamped at all), and sit in the caller's window when the caller
// is anchored. The pane must be spelled as a pane id (%N), the form the lookup
// matches. When tmux lists panes, the pane must exist: an id not yet allocated
// could be created and stamped for another branch after the watchdog's grace.
func Watch(ctx context.Context, from, crew, pane string, p Probes) string {
	if reason := p.envRefusal(from); reason != "" {
		return reason
	}
	if !paneID(pane) {
		return "pane " + pane + " is not a pane id (%N)"
	}
	rows := p.rows(ctx)
	a, anchored := p.anchor(rows)
	anchored = anchored && a.branch != ""
	if anchored {
		if reason := a.refuse(from, crew, "this pane"); reason != "" {
			return reason
		}
	}
	var target stamp
	found := false
	for _, r := range rows {
		if r.pane == pane {
			target, found = r, true
			break
		}
	}
	subject := "pane " + pane
	if anchored && (!found || target.window != a.window) {
		return subject + " is outside this pane's window"
	}
	if !found && rows != nil {
		return subject + " is not a live pane"
	}
	if !found || target.branch == "" {
		return ""
	}
	return target.refuse(from, crew, subject)
}

// paneID: % followed by digits with no leading zero, as tmux prints #{pane_id}.
func paneID(s string) bool {
	d, ok := strings.CutPrefix(s, "%")
	if !ok || d == "" || (d[0] == '0' && len(d) > 1) {
		return false
	}
	for _, c := range d {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (p Probes) envRefusal(from string) string {
	if strings.HasPrefix(from, "dispatcher:") && p.Getenv != nil &&
		(p.Getenv("CREW_WORKER_ID") != "" || p.Getenv("CREW_ROLE_ID") != "") {
		return "this is a worker or role session (CREW_WORKER_ID or CREW_ROLE_ID is set) and only the dispatcher posts as dispatcher:<crew>; post as your own id ($CREW_WORKER_ID for a lead, role:<branch>:<role> for a grid role)"
	}
	return ""
}

// refuse is the stamp rule: a stamped lead pane posts only as its branch's
// lead, a stamped role pane only as its role id, in its window's stamped crew.
func (s stamp) refuse(from, crew, subject string) string {
	if s.role == "" || s.role == "lead" {
		if !leadID(from, s.branch) {
			return subject + " is stamped as the lead of " + s.branch + ", which posts only as worker:" + s.branch + " or worker:" + s.branch + "#s<session> ($CREW_WORKER_ID)"
		}
	} else if from != "role:"+s.branch+":"+s.role {
		return subject + " is stamped as role:" + s.branch + ":" + s.role + ", the only id it posts as"
	}
	if s.crew != "" && s.crew != crew {
		return subject + "'s window is stamped for crew " + s.crew + ", not " + crew
	}
	return ""
}

// leadID: worker:<b> or worker:<b>#s<sid>. The session suffix is matched after
// the known branch, since a branch may itself contain `#`.
func leadID(from, branch string) bool {
	rest, ok := strings.CutPrefix(from, "worker:"+branch)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	sid, ok := strings.CutPrefix(rest, "#s")
	return ok && !strings.Contains(sid, "#")
}

// rows parses one list-panes read; nil when there is no tmux or the read fails.
func (p Probes) rows(ctx context.Context) []stamp {
	if p.Tmux == nil {
		return nil
	}
	out, err := p.Tmux.Run(ctx, "list-panes", "-a", "-F", Format)
	if err != nil {
		return nil
	}
	var rows []stamp
	for line := range strings.Lines(out) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\x1f")
		if len(f) != 6 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid <= 0 {
			continue
		}
		rows = append(rows, stamp{pid: pid, branch: f[1], role: f[2], crew: f[3], window: f[4], pane: f[5]})
	}
	return rows
}

// anchor is the stamps of the nearest pane in rows whose pid is the caller or
// one of its ancestors.
func (p Probes) anchor(rows []stamp) (stamp, bool) {
	if p.Proc == nil {
		return stamp{}, false
	}
	panes := map[int]stamp{}
	for _, r := range rows {
		panes[r.pid] = r
	}
	cur := os.Getpid()
	for range 32 {
		if s, ok := panes[cur]; ok {
			return s, true
		}
		next, ok := p.Proc.Parent(cur)
		if !ok || next <= 0 {
			break
		}
		cur = next
	}
	return stamp{}, false
}
