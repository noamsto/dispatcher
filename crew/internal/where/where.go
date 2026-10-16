// Package where is `crew where`: the human-usable address for a worker's pane —
// codename, `session:window.pane`, window name, role and the jump command a
// dispatcher relays to a human.
//
// The trust rule is the arm's and it is the point: a target resolves from
// dispatcher-anchored state only, the window's `@crew_*` tmux stamps and the
// bus's `dispatch` rows. Never from git discovery inside a worktree and never
// from the worker's env beyond `_crew_id`'s own default, because the command
// answers "where is that worker's pane" from outside its worktree.
//
// Its one read of the fold is the difference between a window that is gone and a
// target nothing ever dispatched: resolve.Resolve answers "the bus knows this
// branch", and the arm says so instead of claiming the target is unknown.
package where

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
)

const (
	exitFailure = 1
	// Usage is the arm's `crew where` signature, quoted in every refusal.
	Usage = "usage: crew where <codename|branch|%id> [--crew ID]"
	// jump is the line's tail: what a human or a dispatcher pastes to get there.
	jump = "   jump: ! tmux switch-client -t "
)

// Probes are the arm's two tmux reads, injected so tests never touch a server.
// A non-nil error is the arm's failed `tmux … 2>/dev/null`: the read itself
// broke, which the arm refuses as itself rather than as "the target is gone".
type Probes struct {
	Windows func() (string, error)
	Panes   func() (string, error)
}

// Options is everything Run reads beyond the bus: the crew the arm defaults to
// (`_crew_id`), which `--crew` overrides, and the tmux probes.
type Options struct {
	CrewID func() string
	Probes Probes
	// Remote runs argv on host as crew with its stdout going to stdout, and
	// returns its status: how a worker dispatched there is asked where it is.
	// Nil routes nothing.
	Remote func(host, crew string, argv []string, stdout io.Writer) int
}

// window is one crew-anchored window, in the six fields the arm kept from the
// `list-windows` format: id, branch, codename, session, window index, name.
type window struct {
	id, branch, name, session, index, winName string
}

// pane is one line of the `list-panes` output — every pane of every window,
// which is what the arm searched for a `%id` target: id, window, role, index.
type pane struct {
	id, window, role, index string
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// die is `_where_die`: the arm's prefix, its exit status, nothing on stdout.
func die(stderr io.Writer, format string, args ...any) int {
	say(stderr, "crew: where: %s\n", fmt.Sprintf(format, args...))
	return exitFailure
}

// Request is the arm's validated arguments: Crew is the `--crew` override ("" for
// the `_crew_id` default) and Target is never empty.
type Request struct {
	Crew, Target string
}

// Run is the arm: the two tmux reads, the resolution, the line.
func Run(req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	crew, target := req.Crew, req.Target
	if crew == "" && o.CrewID != nil {
		crew = o.CrewID()
	}

	// A tmux read failure is not "the target is gone".
	winsRaw, err := o.Probes.Windows()
	if err != nil {
		return die(stderr, "cannot read tmux windows (is a tmux server running?)")
	}
	panesRaw, err := o.Probes.Panes()
	if err != nil {
		return die(stderr, "cannot read tmux panes (is a tmux server running?)")
	}
	panes := readPanes(panesRaw)

	if o.Remote != nil {
		host, branch, err := remote.ResolveTarget(resolve.Rows(paths.Log), target, crew, localMatch(readWindows(winsRaw, paths.Dir, crew), target))
		if err != nil {
			return die(stderr, "%v", err)
		}
		as := crew
		if host != "" && crew == "" {
			// No crew of its own: the remote is asked as the dispatch row's crew.
			events, _ := bus.ReadEventsTolerant(paths.Log)
			if host, as, err = remote.RouteAny(events, branch); err != nil {
				return die(stderr, "CREW_ID not set and %v", err)
			}
		}
		if host != "" {
			return whereRemote(host, branch, as, winsRaw, panes, paths, stdout, stderr, o)
		}
	}

	win, hit, code := resolveTarget(readWindows(winsRaw, paths.Dir, crew), panes, target, crew, paths, stderr)
	if code != 0 {
		return code
	}

	// A window addressed by branch or codename means its lead pane.
	if hit.id == "" {
		hit, code = leadPane(panes, win.id)
		if code != 0 {
			return die(stderr, "no pane in the window for '%s'", target)
		}
	}
	// A plain (non-grid) dispatch window stamps no @crew_role: its lone pane is lead.
	role := hit.role
	if role == "" {
		role = "lead"
	}
	// An older window stamped no @crew_name: the pool gives the codename dispatch
	// would have stamped.
	name := win.name
	if name == "" {
		name = poolName(win.branch)
	}

	// The line a human or a dispatcher relays verbatim: em dash, three spaces
	// before `jump:`, the window name raw between literal quotes (a quote is legal
	// in a branch name, which is what the window is named from).
	say(stdout, "%s — %s:%s.%s \"%s\" (%s pane)%s%s\n",
		name, win.session, win.index, hit.index, win.winName, role, jump, hit.id)
	return 0
}

// localMatch is the branch the local windows give a target (its own, else its
// codename's), joined with "," when several share the codename, and "" for none.
func localMatch(wins []window, target string) string {
	for _, w := range wins {
		if w.branch == target {
			return w.branch
		}
	}
	var matches []window
	for _, w := range wins {
		if w.name == target {
			matches = append(matches, w)
		}
	}
	return joinBranches(matches, ",")
}

// whereRemote answers for a worker dispatched on host. A mirror window on this
// machine is the jump target, and its line is the usual one; with no mirror the
// host is asked, and its line is passed on. Either way the line ends ` [host h]`.
func whereRemote(host, branch, crew, winsRaw string, panes []pane, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	mirror, ok := mirrorWindow(winsRaw, branch, crew)
	if !ok {
		var out bytes.Buffer
		if rc := o.Remote(host, crew, []string{"crew", "where", "--crew", crew, "--", branch}, &out); rc != 0 {
			return rc
		}
		say(stdout, "%s [host %s]\n", strings.TrimSuffix(out.String(), "\n"), host)
		return 0
	}
	hit, code := leadPane(panes, mirror.id)
	if code != 0 {
		return die(stderr, "no pane in the window for '%s'", branch)
	}
	name := mirror.name
	if name == "" {
		for _, r := range resolve.Resolve(resolve.Rows(paths.Log), branch, crew) {
			if r.Branch == branch {
				name = r.Name
			}
		}
	}
	if name == "" {
		name = poolName(branch)
	}
	role := hit.role
	if role == "" {
		role = "lead"
	}
	say(stdout, "%s — %s:%s.%s \"%s\" (%s pane)%s%s [host %s]\n",
		name, mirror.session, mirror.index, hit.index, mirror.winName, role, jump, hit.id, host)
	return 0
}

// mirrorWindow is the local window that mirrors a remote worker's: the one whose
// @bridge_crew_branch is the branch and whose @bridge_crew_id is unset or this crew's.
func mirrorWindow(raw, branch, crew string) (window, bool) {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 10 || f[8] != branch || (f[9] != "" && f[9] != crew) {
			continue
		}
		return window{id: f[0], name: f[4], session: f[5], index: f[6], winName: f[7]}, true
	}
	return window{}, false
}

// readWindows is the arm's crew filter over `list-windows`: the eight format
// fields, a non-empty branch, this repo's crew dir, and this crew's id when the
// window carries one. An empty caller crew (no CREW_ID, no WORKER_TASK.md)
// filters nothing at all.
func readWindows(raw, dir, crew string) []window {
	var wins []window
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 8 || f[1] == "" || f[2] != dir {
			continue
		}
		if crew != "" && f[3] != "" && f[3] != crew {
			continue
		}
		wins = append(wins, window{
			id: f[0], branch: f[1], name: f[4], session: f[5], index: f[6], winName: f[7],
		})
	}
	return wins
}

// readPanes is the raw `list-panes` list: the arm searched every pane, not just
// this crew's, so another crew's `%id` reaches the "not a pane of this crew"
// refusal rather than "no pane".
func readPanes(raw string) []pane {
	var panes []pane
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		panes = append(panes, pane{id: f[1], window: f[0], role: f[2], index: f[3]})
	}
	return panes
}

// resolveTarget is the arm's resolution: a `%pane` id keeps that exact pane,
// else the branch, else the codename, else whatever the bus says about it. The
// zero pane means "this window's lead pane".
func resolveTarget(wins []window, panes []pane, target, crew string, paths bus.Paths, stderr io.Writer) (window, pane, int) {
	if isPaneID(target) {
		hit, ok := findPane(panes, func(p pane) bool { return p.id == target })
		if !ok {
			return window{}, pane{}, die(stderr, "no pane %s", target)
		}
		for _, w := range wins {
			if w.id == hit.window {
				return w, hit, 0
			}
		}
		return window{}, pane{}, die(stderr, "pane %s is not a pane of this crew", target)
	}

	for _, w := range wins {
		if w.branch == target {
			return w, pane{}, 0
		}
	}

	var matches []window
	for _, w := range wins {
		if w.name == target {
			matches = append(matches, w)
		}
	}
	if len(matches) > 1 {
		return window{}, pane{}, die(stderr, "ambiguous codename '%s' — matches %s; pass a branch or %%pane",
			target, joinBranches(matches, ","))
	}
	if len(matches) == 1 {
		return matches[0], pane{}, 0
	}

	// No window carries it: the bus knows the target, or nothing does. The arm
	// joined the branches with "," and read the result's shape as its message.
	var branches []string
	for _, r := range resolve.Resolve(resolve.Rows(paths.Log), target, crew) {
		branches = append(branches, r.Branch)
	}
	dbr := strings.Join(branches, ",")
	switch {
	case strings.Contains(dbr, ","):
		return window{}, pane{}, die(stderr, "ambiguous target '%s' — matches %s; pass a branch or %%pane", target, dbr)
	case dbr != "":
		return window{}, pane{}, die(stderr, "no live pane for '%s' (branch %s) — its window is gone", target, dbr)
	}
	return window{}, pane{}, die(stderr, "no worker matches '%s'", target)
}

// leadPane is the arm's two awk passes over the window's panes: the pane the
// grid stamped lead, else the window's first pane. ok is false for a window with
// no pane at all, which the arm refused.
func leadPane(panes []pane, winID string) (pane, int) {
	if hit, ok := findPane(panes, func(p pane) bool { return p.window == winID && p.role == "lead" }); ok {
		return hit, 0
	}
	hit, ok := findPane(panes, func(p pane) bool { return p.window == winID })
	if !ok {
		return pane{}, exitFailure
	}
	return hit, 0
}

// isPaneID is the arm's `'%'[0-9]*` case: a `%` and then a digit, so `%lead` or
// a bare `%` is a name.
func isPaneID(target string) bool {
	return len(target) >= 2 && target[0] == '%' && target[1] >= '0' && target[1] <= '9'
}

// findPane is the arm's `awk … { print $2; exit }`: the first pane matching, and
// whether there was one at all.
func findPane(panes []pane, match func(pane) bool) (pane, bool) {
	for _, p := range panes {
		if match(p) {
			return p, true
		}
	}
	return pane{}, false
}

func joinBranches(wins []window, sep string) string {
	branches := make([]string, len(wins))
	for i, w := range wins {
		branches[i] = w.branch
	}
	return strings.Join(branches, sep)
}

// poolName is `_identity <branch> | jq -r .name`: the codename the pool gives
// this branch, which is what `dispatch` stamped when it made the window.
func poolName(branch string) string {
	id, _ := identity.At(identity.Slot(branch)).Get("name")
	name, _ := id.AsString()
	return name
}
