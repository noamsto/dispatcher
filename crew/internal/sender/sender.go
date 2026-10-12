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
	"strconv"
	"strings"
)

// Format is the `tmux list-panes -a -F` format Probes.Panes runs; window options
// resolve through it, so each line is the pane's pid and its window's stamps.
const Format = "#{pane_pid}\x1f#{@crew_branch}\x1f#{@crew_role}\x1f#{@crew_id}"

// Probes are the reads Check makes; the zero value checks nothing.
type Probes struct {
	Getenv func(string) string       // nil: no env
	Panes  func() (string, error)    // tmux list-panes -a -F Format; nil: no tmux
	Parent func(pid int) (int, bool) // the pid's parent, false when unknown; nil: no ancestry
	PID    int                       // the caller's own pid
}

type stamp struct{ branch, role, crew string }

// Check returns "" when this caller may post a row from `from` into `crew`,
// else the refusal reason (no "crew: <sub>:" prefix, no trailing newline).
func Check(from, crew string, p Probes) string {
	if strings.HasPrefix(from, "dispatcher:") && p.Getenv != nil &&
		(p.Getenv("CREW_WORKER_ID") != "" || p.Getenv("CREW_ROLE_ID") != "") {
		return "this is a worker or role session (CREW_WORKER_ID or CREW_ROLE_ID is set) and only the dispatcher posts as dispatcher:<crew>; post as your own id ($CREW_WORKER_ID for a lead, role:<branch>:<role> for a grid role)"
	}
	s, ok := p.anchor()
	if !ok || s.branch == "" {
		return ""
	}
	if s.role == "" || s.role == "lead" {
		if !leadID(from, s.branch) {
			return "this pane is stamped as the lead of " + s.branch + ", which posts only as worker:" + s.branch + " or worker:" + s.branch + "#s<session> ($CREW_WORKER_ID)"
		}
	} else if from != "role:"+s.branch+":"+s.role {
		return "this pane is stamped as role:" + s.branch + ":" + s.role + ", the only id it posts as"
	}
	if s.crew != "" && s.crew != crew {
		return "this pane's window is stamped for crew " + s.crew + ", not " + crew
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

// anchor is the stamps of the nearest pane whose pid is the caller or one of
// its ancestors.
func (p Probes) anchor() (stamp, bool) {
	if p.Panes == nil || p.Parent == nil {
		return stamp{}, false
	}
	out, err := p.Panes()
	if err != nil {
		return stamp{}, false
	}
	panes := map[int]stamp{}
	for line := range strings.Lines(out) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\x1f")
		if len(f) != 4 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil || pid <= 0 {
			continue
		}
		panes[pid] = stamp{branch: f[1], role: f[2], crew: f[3]}
	}
	cur := p.PID
	for range 32 {
		if s, ok := panes[cur]; ok {
			return s, true
		}
		next, ok := p.Parent(cur)
		if !ok || next <= 0 {
			break
		}
		cur = next
	}
	return stamp{}, false
}
